(function () {
  const cfg = window.NANOTDB_DASH_CONFIG || { apiBaseURL: "", refreshSeconds: 10 };
  const dbSelect = document.getElementById("dbSelect");
  const refreshBtn = document.getElementById("refreshBtn");
  const statusEl = document.getElementById("status");
  const overviewPane = document.getElementById("overviewPane");
  const databasePane = document.getElementById("databasePane");
  const filesPane = document.getElementById("filesPane");
  const walPane = document.getElementById("walPane");
  const runtimePane = document.getElementById("runtimePane");
  const internalEventsPane = document.getElementById("internalEventsPane");
  const panes = {
    overview: overviewPane,
    database: databasePane,
    files: filesPane,
    wal: walPane,
    runtime: runtimePane,
    "internal-events": internalEventsPane,
  };
  // The Internal Events tab keeps the catalog drawer open/closed
  // across refreshes so a redraw doesn't snap it shut while an
  // operator is looking at it.
  let internalEventsCatalogOpen = false;
  let internalEventsToggleBusy = false;
  const tabButtons = Array.from(document.querySelectorAll('.engine-tab'));
  let activeTab = "overview";
  let refreshTimer = null;
  let selectedDataFileByDB = Object.create(null);
  let fileCompactStatusByKey = Object.create(null);
  let fileCompactBusyByKey = Object.create(null);

  function apiURL(path) {
    const base = typeof cfg.apiBaseURL === "string" ? cfg.apiBaseURL.replace(/\/$/, "") : "";
    return base + path;
  }

  async function fetchJSON(url) {
    const res = await fetch(url, { cache: "no-store" });
    if (!res.ok) {
      throw new Error("HTTP " + res.status + " for " + url);
    }
    return res.json();
  }

  async function postJSON(url, payload) {
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload || {}),
    });
    const data = await res.json().catch(() => null);
    if (!res.ok) {
      throw new Error((data && data.error) || ("HTTP " + res.status + " for " + url));
    }
    return data;
  }

  function setStatus(text) {
    statusEl.textContent = text;
  }

  function escapeHTML(value) {
    return String(value)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function number(v) {
    if (v == null || Number.isNaN(Number(v))) return "-";
    return Number(v).toLocaleString();
  }

  function formatBytes(v) {
    if (v == null || Number.isNaN(Number(v))) return "-";
    const bytes = Number(v);
    const units = ["B", "KB", "MB", "GB", "TB"];
    let value = Math.abs(bytes);
    let idx = 0;
    while (value >= 1024 && idx < units.length - 1) {
      value /= 1024;
      idx += 1;
    }
    const signed = bytes < 0 ? -value : value;
    const formatted = Math.abs(signed) >= 100 || idx === 0 ? signed.toFixed(0) : signed.toFixed(1);
    return formatted + " " + units[idx];
  }

  function decimal(v, digits) {
    if (v == null || Number.isNaN(Number(v))) return "-";
    return Number(v).toFixed(digits == null ? 2 : digits).replace(/\.0+$/, "").replace(/(\.\d*?)0+$/, "$1");
  }

  function durationFromNs(ns) {
    if (!ns) return "-";
    const ms = Number(ns) / 1e6;
    if (ms < 1000) return ms.toFixed(0) + " ms";
    const sec = ms / 1000;
    if (sec < 60) return sec.toFixed(1) + " s";
    const min = sec / 60;
    if (min < 60) return min.toFixed(1) + " min";
    const hr = min / 60;
    return hr.toFixed(1) + " h";
  }

  function formatClock(value) {
    if (!value) return "-";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return value;
    if (date.getUTCFullYear() <= 1) return "never";
    return date.toLocaleString();
  }

  function ageFromValue(value) {
    if (!value) return "-";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "-";
    if (date.getUTCFullYear() <= 1) return "never";
    let diffMs = Date.now() - date.getTime();
    if (diffMs < 0) diffMs = 0;
    if (diffMs < 1000) return "just now";
    const sec = Math.floor(diffMs / 1000);
    if (sec < 60) return sec + "s ago";
    const min = Math.floor(sec / 60);
    if (min < 60) return min + "m ago";
    const hr = Math.floor(min / 60);
    if (hr < 48) return hr + "h ago";
    const day = Math.floor(hr / 24);
    return day + "d ago";
  }

  function renderTable(columns, rows) {
    if (!rows.length) {
      return '<div class="empty">No rows</div>';
    }
    const head = columns.map((col) => "<th>" + col.label + "</th>").join("");
    const body = rows.map((row) => "<tr>" + columns.map((col) => "<td>" + (row[col.key] == null || row[col.key] === "" ? "-" : row[col.key]) + "</td>").join("") + "</tr>").join("");
    return '<div class="table-wrap"><table><thead><tr>' + head + '</tr></thead><tbody>' + body + '</tbody></table></div>';
  }

  function renderSummaryCards(items) {
    return '<div class="summary-grid">' + items.map((item) => '<div class="summary-card"><div class="label">' + item.label + '</div><div class="value">' + item.value + '</div></div>').join("") + '</div>';
  }

  function renderWALPreviewTable(records) {
    return renderTable([
      { key: 'index', label: 'Idx' },
      { key: 'metric', label: 'Metric' },
      { key: 'timestamp', label: 'Timestamp' },
      { key: 'type', label: 'Type' },
      { key: 'value', label: 'Value' },
    ], (records || []).map((record) => ({
      index: number(record.index),
      metric: '<span class="codeish">' + escapeHTML(record.metric_name || String(record.metric_id || '-')) + '</span>',
      timestamp: record.timestamp || '-',
      type: record.value_type || '-',
      value: record.value == null ? '-' : escapeHTML(String(record.value)),
    })));
  }

  function fileActionKey(db, part) {
    return db + "|" + part;
  }

  function renderSelectableFilesTable(rows, selectedPath) {
    if (!rows.length) {
      return '<div class="empty">No rows</div>';
    }
    const body = rows.map((row) => {
      const selected = row.path === selectedPath;
      return '<tr class="selectable-row' + (selected ? ' selected' : '') + '">' +
        '<td><button type="button" class="file-select' + (selected ? ' selected' : '') + '" data-file-path="' + escapeHTML(row.path) + '"><span class="codeish">' + escapeHTML(row.path) + '</span></button></td>' +
        '<td>' + number(row.bytes) + '</td>' +
        '<td>' + number(row.frames) + '</td>' +
        '<td>' + number(row.records) + '</td>' +
        '<td>' + (row.start || '-') + '</td>' +
        '<td>' + (row.end || '-') + '</td>' +
        '<td>' + (row.action || '-') + '</td>' +
        '<td>' + (row.status || '-') + '</td>' +
        '<td>' + (row.error || '-') + '</td>' +
        '</tr>';
    }).join('');
      return '<div class="table-wrap"><table><thead><tr><th>Path</th><th>Bytes</th><th>Frames</th><th>Records</th><th>Start</th><th>End</th><th>Compact</th><th>Status</th><th>Error</th></tr></thead><tbody>' + body + '</tbody></table></div>';
  }

  function yesNo(value) {
    return value ? 'yes' : 'no';
  }

  function explainSetting(key, value, settings) {
    if (key === 'wal_fsync_policy') {
      return value === 'always'
        ? 'Every WAL append is fsynced. Best crash durability, highest write latency.'
        : 'WAL is fsynced on segment seal/reset, not every append. Graceful restart is safe, sudden power loss can lose the newest appends.';
    }
    if (key === 'durability_profile') {
      if (value === 'strict') return 'Page flushes fsync data files and persist catalogs. Safest flush behavior.';
      if (value === 'balanced') return 'Page flushes fsync data files, but catalog persistence is deferred.';
      return 'Flushes prioritize throughput over immediate persistence.';
    }
    if (key === 'sync_data_file') {
      return value ? 'Each page flush syncs the .dat file to disk.' : 'Page flushes do not fsync the .dat file immediately.';
    }
    if (key === 'sync_catalog') {
      return value ? 'Catalog writes are persisted during flush/close.' : 'Catalog persistence can lag until later writes or shutdown.';
    }
    if (key === 'stats_interval') {
      return settings.stats_enabled ? 'Internal engine metrics are emitted on this interval.' : 'Internal engine metric emission is disabled.';
    }
    if (key === 'default_wal_enabled') {
      return value ? 'New databases use WAL by default.' : 'New databases skip WAL by default.';
    }
    if (key === 'default_wal_skip_before') {
      return 'Samples older than now minus this duration skip WAL by default.';
    }
    if (key === 'default_page_max_records') {
      return 'Open pages flush after reaching this record count.';
    }
    if (key === 'default_page_max_bytes') {
      return 'Open pages flush after reaching approximately this payload size.';
    }
    if (key === 'default_page_max_age') {
      return 'Open pages flush after this age even if they are not full.';
    }
    if (key === 'default_max_active_days') {
      return 'This many day partitions can stay open in memory before older ones are sealed.';
    }
    if (key === 'default_retention_days') {
      return 'Persisted data older than this retention window is eligible for cleanup.';
    }
    return 'Active runtime setting loaded from the current engine configuration.';
  }

  function renderSettingsTable(settings) {
    const rows = [
      { name: 'Listen', value: settings.listen || '-', meaning: 'HTTP listen address for the running NanoTDB server.' },
      { name: 'WAL fsync policy', value: settings.wal_fsync_policy || '-', meaning: explainSetting('wal_fsync_policy', settings.wal_fsync_policy, settings) },
      { name: 'WAL max segment size', value: number(settings.wal_max_segment_size), meaning: 'WAL segment size limit before seal/reset work happens.' },
      { name: 'Durability profile', value: settings.durability_profile || '-', meaning: explainSetting('durability_profile', settings.durability_profile, settings) },
      { name: 'Sync data file', value: yesNo(settings.sync_data_file), meaning: explainSetting('sync_data_file', settings.sync_data_file, settings) },
      { name: 'Sync catalog', value: yesNo(settings.sync_catalog), meaning: explainSetting('sync_catalog', settings.sync_catalog, settings) },
      { name: 'Stats enabled', value: yesNo(settings.stats_enabled), meaning: 'Whether internal engine self-metrics are emitted into the internal database.' },
      { name: 'Stats interval', value: settings.stats_interval || '-', meaning: explainSetting('stats_interval', settings.stats_interval, settings) },
      { name: 'Web enabled', value: yesNo(settings.web_enabled), meaning: 'Whether the built-in web UI is enabled.' },
      { name: 'Dashboard path', value: settings.dashboard_path || '-', meaning: 'Dashboard route prefix served by NanoTDB.' },
      { name: 'Explore path', value: settings.explore_path || '-', meaning: 'Explore route prefix served by NanoTDB for the manual metric picker view.' },
      { name: 'Engine path', value: settings.engine_path || '-', meaning: 'Engine inspector route prefix served by NanoTDB.' },
      { name: 'Default WAL enabled', value: yesNo(settings.default_wal_enabled), meaning: explainSetting('default_wal_enabled', settings.default_wal_enabled, settings) },
      { name: 'Default WAL skip-before', value: settings.default_wal_skip_before || '-', meaning: explainSetting('default_wal_skip_before', settings.default_wal_skip_before, settings) },
      { name: 'Default grace', value: settings.default_grace || '-', meaning: 'Retention grace window copied into new database manifests.' },
      { name: 'Default retention days', value: number(settings.default_retention_days), meaning: explainSetting('default_retention_days', settings.default_retention_days, settings) },
      { name: 'Default max active days', value: number(settings.default_max_active_days), meaning: explainSetting('default_max_active_days', settings.default_max_active_days, settings) },
      { name: 'Default partition', value: settings.default_partition || '-', meaning: 'Partition key granularity for new databases.' },
      { name: 'Default page max records', value: number(settings.default_page_max_records), meaning: explainSetting('default_page_max_records', settings.default_page_max_records, settings) },
      { name: 'Default page max bytes', value: number(settings.default_page_max_bytes), meaning: explainSetting('default_page_max_bytes', settings.default_page_max_bytes, settings) },
      { name: 'Default page max age', value: settings.default_page_max_age || '-', meaning: explainSetting('default_page_max_age', settings.default_page_max_age, settings) },
    ];
    return renderTable([
      { key: 'name', label: 'Setting' },
      { key: 'value', label: 'Value' },
      { key: 'meaning', label: 'Meaning' },
    ], rows);
  }

  async function loadOverview() {
    const payload = await fetchJSON(apiURL("/api/engine/overview"));
    const items = (payload.data && payload.data.result) || [];
    const settings = (payload.data && payload.data.settings) || {};
    overviewPane.innerHTML = '<div class="section-head"><h2>Overview</h2><p>Loaded databases, quick runtime stats, and file counts.</p></div>' +
      renderSummaryCards([
        { label: "Databases", value: number(items.length) },
        { label: "Metrics", value: number(items.reduce((sum, item) => sum + (item.metric_count || 0), 0)) },
        { label: "Events", value: number(items.reduce((sum, item) => sum + (item.event_count || 0), 0)) },
        { label: "Open Pages", value: number(items.reduce((sum, item) => sum + (item.open_pages || 0), 0)) },
        { label: "WAL Bytes", value: number(items.reduce((sum, item) => sum + (item.wal_bytes || 0), 0)) },
      ]) +
      renderTable([
        { key: "name", label: "Database" },
        { key: "metricCount", label: "Metrics" },
        { key: "eventCount", label: "Events" },
        { key: "openPages", label: "Open Pages" },
        { key: "dataFiles", label: ".dat Files" },
        { key: "dataBytes", label: ".dat Bytes" },
        { key: "eventFiles", label: "events Files" },
        { key: "eventBytes", label: "events Bytes" },
        { key: "walFiles", label: ".wal Files" },
        { key: "walBytes", label: ".wal Bytes" },
        { key: "walBuffer", label: "WAL Buffer" },
      ], items.map((item) => ({
        name: item.error ? (item.name + ' <span class="codeish" style="color:#f88" title="' + item.error.replace(/"/g, '&quot;') + '">(error)</span>') : item.name,
        metricCount: number(item.metric_count),
        eventCount: number(item.event_count),
        openPages: number(item.open_pages),
        dataFiles: number(item.data_files),
        dataBytes: number(item.data_bytes),
        eventFiles: number(item.event_files),
        eventBytes: number(item.event_bytes),
        walFiles: number(item.wal_files),
        walBytes: number(item.wal_bytes),
        walBuffer: number(item.stats && item.stats.WAL && item.stats.WAL.BufferBytes),
      }))) +
      '<div class="subpanel overview-settings"><div class="section-head"><h3>Runtime Settings</h3><p>Active server settings and default database policy, interpreted in the browser.</p></div>' + renderSettingsTable(settings) + '</div>';

    const current = dbSelect.value;
    dbSelect.innerHTML = "";
    items.forEach((item) => {
      const opt = document.createElement("option");
      opt.value = item.name;
      opt.textContent = item.name;
      dbSelect.appendChild(opt);
    });
    if (current && items.some((item) => item.name === current)) {
      dbSelect.value = current;
    }
  }

  async function loadDatabase() {
    const db = dbSelect.value;
    if (!db) {
      databasePane.innerHTML = '<div class="empty">No database selected.</div>';
      return;
    }
    const payload = await fetchJSON(apiURL("/api/engine/database?db=" + encodeURIComponent(db)));
    const result = payload.data && payload.data.result;
    const summary = result && result.summary;
    const metrics = (result && result.metrics) || [];
    const events = (result && result.events) || [];
    const eventsEnabled = !!(result && result.events_enabled);
    const eventsPanel = !eventsEnabled
      ? '<div class="subpanel"><div class="section-head"><h3>Events</h3><p>Events are not enabled for this database.</p></div></div>'
      : '<div class="subpanel"><div class="section-head"><h3>Events</h3><p>Event catalog for the selected database (' + number(events.length) + ' event' + (events.length === 1 ? '' : 's') + ').</p></div>' +
        (events.length === 0
          ? '<div class="empty">No events recorded yet.</div>'
          : renderTable([
              { key: "name", label: "Event" },
              { key: "id", label: "ID" },
              { key: "type", label: "Value Type" },
              { key: "lastTimestamp", label: "Last Captured" },
            ], events.map((item) => ({
              name: '<span class="codeish">' + item.name + '</span>',
              id: number(item.id),
              type: item.value_type,
              lastTimestamp: item.last_timestamp || '-',
            })))) +
        '</div>';
    databasePane.innerHTML = '<div class="section-head"><h2>Database</h2><p>Manifest defaults, live counters, and metric catalog.</p></div>' +
      renderSummaryCards([
        { label: "Metrics", value: number(summary.metric_count) },
        { label: "Open Pages", value: number(summary.open_pages) },
        { label: "Data Flushes", value: number(summary.stats && summary.stats.DataFile && summary.stats.DataFile.FlushCount) },
        { label: "WAL Appends", value: number(summary.stats && summary.stats.WAL && summary.stats.WAL.AppendCount) },
      ]) +
      '<div class="stack">' +
      '<div class="subpanel"><div class="section-head"><h3>Manifest</h3><p>Per-database runtime policy.</p></div>' + renderTable([
        { key: "grace", label: "Grace" },
        { key: "retention", label: "Retention Days" },
        { key: "active", label: "Max Active Days" },
        { key: "partition", label: "Partition" },
        { key: "wal", label: "WAL" },
        { key: "skipBefore", label: "WAL Skip Before" },
        { key: "pageAge", label: "Page Max Age" },
      ], [{
        grace: summary.manifest && summary.manifest.grace,
        retention: number(summary.manifest && summary.manifest.retention_days),
        active: number(summary.manifest && summary.manifest.max_active_days),
        partition: summary.manifest && summary.manifest.partition,
        wal: summary.manifest && summary.manifest.wal_enabled ? "enabled" : "disabled",
        skipBefore: summary.manifest && summary.manifest.wal_skip_before,
        pageAge: summary.manifest && summary.manifest.page_max_age,
      }]) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Metrics</h3><p>Metric catalog for the selected database.</p></div>' + renderTable([
        { key: "name", label: "Metric" },
        { key: "id", label: "ID" },
        { key: "type", label: "Type" },
        { key: "lastValue", label: "Last Value" },
        { key: "lastTimestamp", label: "Last Captured" },
      ], metrics.map((item) => ({
        name: '<span class="codeish">' + item.name + '</span>',
        id: number(item.id),
        type: item.type,
        lastValue: item.last_value || '-',
        lastTimestamp: item.last_timestamp || '-',
      }))) + '</div>' +
      eventsPanel +
      '</div>';
  }

  async function loadFiles() {
    const db = dbSelect.value;
    if (!db) {
      filesPane.innerHTML = '<div class="empty">No database selected.</div>';
      return;
    }
  let selectedPath = selectedDataFileByDB[db] || '';
  const filesURL = "/api/engine/files?db=" + encodeURIComponent(db) + (selectedPath ? "&data_file=" + encodeURIComponent(selectedPath) : "");
  const payload = await fetchJSON(apiURL(filesURL));
    const result = payload.data && payload.data.result;
    const dataFiles = result.data || [];
    const metricFiles = result.metric || [];
    const metricRows = metricFiles.map((item) => ({
      path: '<span class="codeish">' + item.path + '</span>',
      bytes: number(item.bytes),
      frames: number(item.frames),
      metrics: number(item.distinct_metrics),
      points: number(item.points),
      payload: number(item.avg_payload_bytes),
      start: item.min_utc || '-',
      end: item.max_utc || '-',
      error: item.scan_error || '',
    }));
    if (!selectedPath || !dataFiles.some((item) => item.path === selectedPath)) {
      selectedPath = dataFiles.length ? dataFiles[0].path : '';
    }
    if (selectedPath) {
      selectedDataFileByDB[db] = selectedPath;
    }
    const selectedFile = dataFiles.find((item) => item.path === selectedPath) || null;
    const datRows = dataFiles.map((item) => {
      const key = fileActionKey(db, item.part || item.path);
      const disabled = !item.part || !!item.active || !!item.scan_error || !!fileCompactBusyByKey[key];
      let title = 'Build a metric-v2 file for this sealed partition.';
      if (item.active) {
        title = 'This partition is still open in memory, so compact is disabled.';
      } else if (item.scan_error) {
        title = 'Compact is disabled until the source file can be scanned.';
      }
      return {
        path: item.path,
        bytes: item.bytes,
        frames: item.frames,
        records: item.records,
        start: item.min_utc || '-',
        end: item.max_utc || '-',
        action: '<button type="button" class="action-button action-button--small data-file-compact-btn" data-part="' + escapeHTML(item.part || '') + '" data-path="' + escapeHTML(item.path) + '"' + (disabled ? ' disabled' : '') + ' title="' + escapeHTML(title) + '">Compact</button>',
        status: fileCompactStatusByKey[key] ? escapeHTML(fileCompactStatusByKey[key]) : '-',
        error: item.scan_error || '',
      };
    });
    const pagesTable = selectedFile && selectedFile.pages && selectedFile.pages.length ? renderTable([
      { key: 'index', label: 'Idx' },
      { key: 'offset', label: 'Offset' },
      { key: 'bytes', label: 'Bytes' },
      { key: 'compressed', label: 'Compressed' },
      { key: 'uncompressed', label: 'Uncompressed' },
      { key: 'avgDiskBytes', label: 'Avg Bytes/Point' },
      { key: 'records', label: 'Records' },
      { key: 'duration', label: 'Duration' },
      { key: 'start', label: 'Start' },
      { key: 'end', label: 'End' },
    ], selectedFile.pages.map((page) => ({
      index: number(page.index),
      offset: number(page.offset),
      bytes: number(page.frame_bytes),
      compressed: number(page.compressed_len),
      uncompressed: number(page.uncompressed_len),
      avgDiskBytes: decimal(page.avg_disk_bytes_per_point, 2),
      records: number(page.records),
      duration: durationFromNs(page.duration_ns),
      start: page.start_utc,
      end: page.end_utc,
    }))) : '<div class="empty">' + (selectedFile ? 'Selected file has no page frames.' : 'No data files found.') + '</div>';
    const eventFiles = (result.events) || [];
    const eventRows = eventFiles.map((item) => ({
      path: '<span class="codeish">' + escapeHTML(item.path) + '</span>',
      bytes: number(item.bytes),
      frames: number(item.frames),
      records: number(item.records),
      start: item.min_utc || '-',
      end: item.max_utc || '-',
      error: item.scan_error || '',
    }));
    filesPane.innerHTML = '<div class="section-head"><h2>Files</h2><p>On-disk data and metric partitions, plus selected page inspection.</p></div>' +
      '<div class="stack">' +
      '<div class="subpanel"><div class="section-head"><h3>Data Files</h3><p>Select a .dat file to inspect only its pages.</p></div>' + renderSelectableFilesTable(datRows, selectedPath) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Metric Files</h3><p>Trailer-only scan of query-optimized metric partitions.</p></div>' + renderTable([
        { key: 'path', label: 'Path' },
        { key: 'bytes', label: 'Bytes' },
        { key: 'frames', label: 'Frames' },
        { key: 'metrics', label: 'Metrics' },
        { key: 'points', label: 'Points' },
        { key: 'payload', label: 'Avg Payload' },
        { key: 'start', label: 'Start' },
        { key: 'end', label: 'End' },
        { key: 'error', label: 'Error' },
      ], metricRows) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Event Files</h3><p>Sealed events-*.dat partitions (header-only scan).</p></div>' +
        (eventRows.length === 0
          ? '<div class="empty">No sealed event partitions on disk yet.</div>'
          : renderTable([
              { key: 'path', label: 'Path' },
              { key: 'bytes', label: 'Bytes' },
              { key: 'frames', label: 'Frames' },
              { key: 'records', label: 'Records' },
              { key: 'start', label: 'Start' },
              { key: 'end', label: 'End' },
              { key: 'error', label: 'Error' },
            ], eventRows)) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Pages</h3><p>' + (selectedFile ? '<span class="codeish">' + escapeHTML(selectedFile.path) + '</span>' : 'No file selected.') + '</p></div>' + pagesTable + '</div>' +
      '</div>';
    filesPane.querySelectorAll('.file-select').forEach((button) => {
      button.addEventListener('click', () => {
        selectedDataFileByDB[db] = button.dataset.filePath || '';
        loadFiles().catch((err) => {
          console.error(err);
          setStatus(err && err.message ? err.message : 'Files refresh failed');
        });
      });
    });
    filesPane.querySelectorAll('.data-file-compact-btn').forEach((button) => {
      button.addEventListener('click', async () => {
        const part = button.dataset.part || '';
        const path = button.dataset.path || '';
        if (!part) {
          return;
        }
        const key = fileActionKey(db, part);
        selectedDataFileByDB[db] = path;
        fileCompactBusyByKey[key] = true;
        fileCompactStatusByKey[key] = 'Compacting to metric-v2...';
        setStatus('Compacting ' + part + ' to metric-v2...');
        loadFiles().catch((err) => {
          console.error(err);
        });
        try {
          const payload = await postJSON(apiURL('/api/engine/compact_metric'), { db: db, part: part });
          const resultPayload = payload.data && payload.data.result;
          const savedText = formatBytes(resultPayload.saved_bytes);
          const message = 'Compacted ' + part + ': ' + formatBytes(resultPayload.data_bytes) + ' raw -> ' + formatBytes(resultPayload.metric_bytes) + ' metric (' + savedText + ' saved)';
          fileCompactStatusByKey[key] = message;
          setStatus(message);
          await loadFiles();
        } catch (err) {
          console.error(err);
          const message = err && err.message ? err.message : 'Metric compact failed';
          fileCompactStatusByKey[key] = message;
          setStatus(message);
          loadFiles().catch((loadErr) => {
            console.error(loadErr);
          });
        } finally {
          delete fileCompactBusyByKey[key];
        }
      });
    });
  }

  async function loadWAL() {
    const db = dbSelect.value;
    if (!db) {
      walPane.innerHTML = '<div class="empty">No database selected.</div>';
      return;
    }
    const [filesPayload, runtimePayload] = await Promise.all([
      fetchJSON(apiURL('/api/engine/files?db=' + encodeURIComponent(db))),
      fetchJSON(apiURL('/api/engine/runtime?db=' + encodeURIComponent(db))),
    ]);
    const filesResult = filesPayload.data && filesPayload.data.result;
    const runtimeResult = runtimePayload.data && runtimePayload.data.result;
    const walFiles = (filesResult && filesResult.wal) || [];
    const scannedPreview = (filesResult && filesResult.record_preview) || { total: 0, first: [], last: [] };
    const runtime = (runtimeResult && runtimeResult.runtime) || {};
    const livePreview = (runtimeResult && runtimeResult.wal_preview) || { total: 0, first: [], last: [] };
    const walStats = (runtime.stats && runtime.stats.WAL) || {};
    const flushRows = ((walStats.RecentFlushes || []).slice().reverse()).map((item) => ({
      at: formatClock(item.At),
      age: ageFromValue(item.At),
      bytes: number(item.Bytes),
    }));
    const walRows = walFiles.map((item) => ({
      path: '<span class="codeish">' + escapeHTML(item.path) + '</span>',
      bytes: number(item.bytes),
      records: number(item.records),
      decoded: number(item.decoded_bytes),
      start: item.min_utc || '-',
      end: item.max_utc || '-',
      tail: item.has_tail ? 'yes' : 'no',
      tailBytes: number(item.tail_bytes),
      reason: item.stop_reason || '-',
      error: item.scan_error || '-',
    }));
    const totalWALBytes = walFiles.reduce((sum, item) => sum + Number(item.bytes || 0), 0);
    const totalFileRecords = walFiles.reduce((sum, item) => sum + Number(item.records || 0), 0);
    walPane.innerHTML = '<div class="section-head"><h2>WAL</h2><p>Current WAL health, preview samples, and recent flush history.</p></div>' +
      renderSummaryCards([
        { label: 'WAL Files', value: number(walFiles.length) },
        { label: 'WAL Bytes', value: number(totalWALBytes) },
        { label: 'Scanned Records', value: number(totalFileRecords || scannedPreview.total) },
        { label: 'Live Records', value: number(livePreview.total) },
        { label: 'WAL Buffer', value: number(walStats.BufferBytes) },
        { label: 'Flushes', value: number(walStats.FlushCount) },
        { label: 'Last Append', value: ageFromValue(walStats.LastAppendAt) },
        { label: 'Last Flush', value: ageFromValue(walStats.LastFlushAt) },
      ]) +
      '<div class="stack">' +
      '<div class="subpanel"><div class="section-head"><h3>WAL Files</h3><p>Per-file scan status, record counts, tail state, and time range.</p></div>' + renderTable([
        { key: 'path', label: 'Path' },
        { key: 'bytes', label: 'Bytes' },
        { key: 'records', label: 'Records' },
        { key: 'decoded', label: 'Decoded Bytes' },
        { key: 'start', label: 'Start' },
        { key: 'end', label: 'End' },
        { key: 'tail', label: 'Tail' },
        { key: 'tailBytes', label: 'Tail Bytes' },
        { key: 'reason', label: 'Stop Reason' },
        { key: 'error', label: 'Error' },
      ], walRows) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>WAL Preview</h3><p>' + number(livePreview.total) + ' decoded live records. Showing the first and last samples only.</p></div>' +
      '<div class="stack">' +
      '<div class="subpanel"><div class="section-head"><h3>First Records</h3><p>Earliest visible records in the active WAL.</p></div>' + renderWALPreviewTable(livePreview.first || []) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Last Records</h3><p>Newest visible records in the active WAL.</p></div>' + renderWALPreviewTable(livePreview.last || []) + '</div>' +
      '</div></div>' +
      '<div class="subpanel"><div class="section-head"><h3>Recent Flushes</h3><p>Newest flush events recorded by the active WAL.</p></div>' + renderTable([
        { key: 'at', label: 'At' },
        { key: 'age', label: 'Age' },
        { key: 'bytes', label: 'Bytes' },
      ], flushRows) + '</div>' +
      '</div>';
  }

  async function loadRuntime() {
    const payload = await fetchJSON(apiURL("/api/engine/runtime"));
    const result = payload.data && payload.data.result;
    const process = result.process || {};
    const goMem = result.go_mem || {};
    const openPages = result.open_pages || [];
    const totalRecords = openPages.reduce((sum, page) => sum + Number(page.records || 0), 0);
    const totalMetrics = openPages.reduce((sum, page) => sum + Number(page.unique_metrics || 0), 0);
    const totalBytes = openPages.reduce((sum, page) => sum + Number(page.value_bytes || 0), 0);
    const oldestAgeNS = openPages.reduce((maxAge, page) => Math.max(maxAge, Number(page.age_ns || 0)), 0);
    runtimePane.innerHTML = '<div class="section-head"><h2>Runtime</h2><p>Process-wide engine runtime, Go memory stats, and open pages across all databases.</p></div>' +
      renderSummaryCards([
        { label: 'Databases', value: number(result.database_count) },
        { label: 'Active DBs', value: number(result.active_database_count) },
        { label: 'Open Pages', value: number(openPages.length) },
        { label: 'Open Events Pages', value: number((result.open_events_pages || []).length) },
        { label: 'Events', value: number(result.event_count) },
        { label: 'RSS', value: formatBytes(process.rss_bytes) },
        { label: 'Heap Alloc', value: formatBytes(goMem.heap_alloc_bytes) },
        { label: 'Go Sys', value: formatBytes(goMem.sys_bytes) },
        { label: 'Goroutines', value: number(process.num_goroutine) },
        { label: 'Proc Age', value: ageFromValue(process.started_at) },
        { label: 'GC Cycles', value: number(goMem.num_gc) },
      ]) +
      '<div class="stack">' +
      '<div class="subpanel"><div class="section-head"><h3>Process</h3><p>OS process memory and Go runtime counters for the running server.</p></div>' + renderTable([
        { key: 'name', label: 'Metric' },
        { key: 'value', label: 'Value' },
      ], [
        { name: 'Started', value: formatClock(process.started_at) },
        { name: 'Process age', value: ageFromValue(process.started_at) },
        { name: 'RSS', value: formatBytes(process.rss_bytes) },
        { name: 'Goroutines', value: number(process.num_goroutine) },
        { name: 'CPU threads available', value: number(process.num_cpu) },
        { name: 'Tracked databases', value: number(result.database_count) },
        { name: 'Active databases', value: number(result.active_database_count) },
        { name: 'Known metrics', value: number(result.metric_count) },
        { name: 'Open page records', value: number(totalRecords) },
        { name: 'Open page metric slots', value: number(totalMetrics) },
        { name: 'Open page value bytes', value: formatBytes(totalBytes) },
        { name: 'Oldest open page age', value: durationFromNs(oldestAgeNS) },
      ]) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Go Memory</h3><p>Go runtime memstats snapshot from the running process.</p></div>' + renderTable([
        { key: 'name', label: 'Stat' },
        { key: 'value', label: 'Value' },
      ], [
        { name: 'Alloc', value: formatBytes(goMem.alloc_bytes) },
        { name: 'TotalAlloc', value: formatBytes(goMem.total_alloc_bytes) },
        { name: 'Sys', value: formatBytes(goMem.sys_bytes) },
        { name: 'HeapAlloc', value: formatBytes(goMem.heap_alloc_bytes) },
        { name: 'HeapSys', value: formatBytes(goMem.heap_sys_bytes) },
        { name: 'HeapInuse', value: formatBytes(goMem.heap_inuse_bytes) },
        { name: 'HeapIdle', value: formatBytes(goMem.heap_idle_bytes) },
        { name: 'StackInuse', value: formatBytes(goMem.stack_inuse_bytes) },
        { name: 'StackSys', value: formatBytes(goMem.stack_sys_bytes) },
        { name: 'NextGC', value: formatBytes(goMem.next_gc_bytes) },
        { name: 'GC count', value: number(goMem.num_gc) },
        { name: 'GC CPU fraction', value: decimal(goMem.gc_cpu_fraction, 4) },
        { name: 'Last GC', value: formatClock(goMem.last_gc_at) },
      ]) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Open Pages</h3><p>In-memory page state before flush to .dat files across all databases.</p></div>' + renderTable([
        { key: 'database', label: 'Database' },
        { key: 'day', label: 'Day' },
        { key: 'records', label: 'Records' },
        { key: 'metrics', label: 'Unique Metrics' },
        { key: 'bytes', label: 'Value Bytes' },
        { key: 'start', label: 'Start' },
        { key: 'end', label: 'End' },
        { key: 'age', label: 'Age' },
        { key: 'full', label: 'Full' },
      ], openPages.map((page) => ({
        database: '<span class="codeish">' + escapeHTML(page.database || '-') + '</span>',
        day: page.day,
        records: number(page.records),
        metrics: number(page.unique_metrics),
        bytes: formatBytes(page.value_bytes),
        start: page.start_timestamp_ns ? new Date(page.start_timestamp_ns / 1e6).toISOString() : '-',
        end: page.end_timestamp_ns ? new Date(page.end_timestamp_ns / 1e6).toISOString() : '-',
        age: durationFromNs(page.age_ns),
        full: String(!!page.full),
      }))) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Open Events Pages</h3><p>In-memory events pages per database/partition before flush to events-*.dat.</p></div>' +
        ((result.open_events_pages || []).length === 0
          ? '<div class="empty">No open events pages.</div>'
          : renderTable([
              { key: 'database', label: 'Database' },
              { key: 'day', label: 'Day' },
              { key: 'records', label: 'Records' },
              { key: 'start', label: 'Start' },
              { key: 'end', label: 'End' },
              { key: 'age', label: 'Age' },
              { key: 'maxRecords', label: 'Max Records' },
            ], (result.open_events_pages || []).map((page) => ({
              database: '<span class="codeish">' + escapeHTML(page.database || '-') + '</span>',
              day: page.day,
              records: number(page.records),
              start: page.start_timestamp_ns ? new Date(page.start_timestamp_ns / 1e6).toISOString() : '-',
              end: page.end_timestamp_ns ? new Date(page.end_timestamp_ns / 1e6).toISOString() : '-',
              age: durationFromNs(page.age_ns),
              maxRecords: number(page.max_records),
            })))) + '</div>' +
      '</div>';
  }

  // formatInternalEventValue extracts the typed value field from an
  // event response. Mirrors the dashboard widget's extractor — events
  // can be int32, float32, or none-typed (no value, just a payload).
  function formatInternalEventValue(evt) {
    if (!evt) return '';
    if (evt.int32 != null) return String(evt.int32);
    if (evt.float32 != null) return String(evt.float32);
    if (evt.value != null) return String(evt.value);
    return '';
  }

  // formatInternalEventPayload renders a one-line summary of an event
  // payload — first 8 keys, truncated to 240 chars. The full payload
  // (as JSON) is also exposed via the row's title attribute so a hover
  // tooltip reveals everything that didn't fit.
  function formatInternalEventPayload(payload) {
    if (!payload || typeof payload !== 'object') return '';
    const entries = Object.entries(payload).slice(0, 8);
    if (entries.length === 0) return '';
    const summary = entries.map(([k, v]) => {
      let val = v;
      if (typeof v === 'object') {
        try { val = JSON.stringify(v); } catch (e) { val = String(v); }
      }
      return k + '=' + String(val);
    }).join(' ');
    return summary.length > 240 ? summary.slice(0, 237) + '...' : summary;
  }

  // fullInternalEventPayload renders the entire payload object as a
  // pretty-printed JSON string for use as a tooltip / title attribute.
  function fullInternalEventPayload(payload) {
    if (!payload || typeof payload !== 'object') return '';
    try { return JSON.stringify(payload, null, 2); } catch (e) { return String(payload); }
  }

  // toggleInternalEventsGroup posts an on/off flip to
  // /api/v1/internal-events/groups, then triggers a redraw on success.
  // Click-busy guard prevents double-flips while the request is in
  // flight; the global setInterval will redraw within ~10s either way.
  async function toggleInternalEventsGroup(group, on) {
    if (internalEventsToggleBusy) return;
    internalEventsToggleBusy = true;
    try {
      await postJSON(apiURL('/api/v1/internal-events/groups'), { [group]: on ? 'on' : 'off' });
      await loadInternalEvents();
    } catch (err) {
      setStatus(err && err.message ? err.message : 'Toggle failed');
    } finally {
      internalEventsToggleBusy = false;
    }
  }

  async function loadInternalEvents() {
    // Three concurrent fetches:
    //   1. Group state (with source: default | engine.toml | runtime)
    //   2. Recent nanotdb.* events
    //   3. Recent drip.* events
    // The catalog (every defined event) is also fetched so the drawer
    // can render. Failures of any single call render an inline error
    // for that subpanel rather than tanking the whole tab.
    const lookback = 60 * 60; // 1h, in seconds
    const end = new Date();
    const start = new Date(end.getTime() - lookback * 1000);
    const startISO = encodeURIComponent(start.toISOString());
    const endISO = encodeURIComponent(end.toISOString());

    const [groupsRes, catalogRes, nanoEventsRes, dripEventsRes] = await Promise.all([
      fetchJSON(apiURL('/api/v1/internal-events/groups')).catch((err) => ({ error: err })),
      fetchJSON(apiURL('/api/v1/internal-events/catalog')).catch((err) => ({ error: err })),
      fetchJSON(apiURL('/api/v1/events?db=internal&name=nanotdb.*&start=' + startISO + '&end=' + endISO + '&limit=100')).catch((err) => ({ error: err })),
      fetchJSON(apiURL('/api/v1/events?db=internal&name=drip.*&start=' + startISO + '&end=' + endISO + '&limit=50')).catch((err) => ({ error: err })),
    ]);

    const groups = (groupsRes && groupsRes.data && groupsRes.data.groups) || [];
    const catalog = (catalogRes && catalogRes.data && catalogRes.data.groups) || [];
    const nanoEvents = (nanoEventsRes && nanoEventsRes.data && nanoEventsRes.data.result) || [];
    const dripEvents = (dripEventsRes && dripEventsRes.data && dripEventsRes.data.result) || [];
    const masterEnabled = catalogRes && catalogRes.data && catalogRes.data.master_enabled;
    const destDB = (catalogRes && catalogRes.data && catalogRes.data.destination_db) || 'internal';

    // Merged feed, newest first. Cap by combined budget so a chatty
    // burst can't flood the DOM.
    const recent = [];
    nanoEvents.forEach((e) => recent.push(e));
    dripEvents.forEach((e) => recent.push(e));
    recent.sort((a, b) => (b.ts || 0) - (a.ts || 0));
    const recentRows = recent.slice(0, 150).map((evt) => ({
      time: '<span title="' + escapeHTML(String(evt.ts || '')) + '">' + escapeHTML(new Date((evt.ts || 0) / 1e6).toLocaleString()) + '</span>',
      name: '<span class="codeish">' + escapeHTML(evt.name || '?') + '</span>',
      value: escapeHTML(formatInternalEventValue(evt)),
      payload: '<span class="codeish-soft" title="' + escapeHTML(fullInternalEventPayload(evt.payload)) + '">' + escapeHTML(formatInternalEventPayload(evt.payload)) + '</span>',
    }));

    // Groups table with inline toggle. A click on the toggle button
    // posts to /groups; the button is rebuilt on the next redraw.
    const groupsRows = groups.map((g) => {
      const stateLabel = g.enabled
        ? '<span class="badge badge-on">on</span>'
        : '<span class="badge badge-off">off</span>';
      const toggleText = g.enabled ? 'Disable' : 'Enable';
      const next = g.enabled ? 'off' : 'on';
      const sourceLabel = g.source === 'runtime'
        ? '<span class="badge badge-runtime">' + g.source + '</span>'
        : '<span class="badge-soft">' + g.source + '</span>';
      return {
        name: '<span class="codeish">' + escapeHTML(g.name) + '</span>',
        state: stateLabel,
        defaultState: g.default ? 'on' : 'off',
        source: sourceLabel,
        action: '<button type="button" class="link-btn" data-toggle-group="' + escapeHTML(g.name) + '" data-toggle-next="' + next + '">' + toggleText + '</button>',
      };
    });

    // Catalog drawer — a collapsible listing of every registered
    // event. Body is only built when open to avoid the DOM cost when
    // the drawer is collapsed (common case).
    const catalogBody = !internalEventsCatalogOpen ? '' :
      catalog.map((g) => {
        const events = (g.events || []).map((e) => ({
          name: '<span class="codeish">' + escapeHTML(e.name) + '</span>',
          type: e.value_type,
          units: e.value_units || '-',
          description: escapeHTML(e.description || ''),
        }));
        return '<div class="subpanel catalog-group"><div class="section-head"><h4>' + escapeHTML(g.name) +
          ' <span class="badge-soft">' + (g.enabled ? 'on' : 'off') + '</span></h4></div>' +
          renderTable([
            { key: 'name', label: 'Event' },
            { key: 'type', label: 'Value' },
            { key: 'units', label: 'Units' },
            { key: 'description', label: 'Description' },
          ], events) + '</div>';
      }).join('');

    internalEventsPane.innerHTML =
      '<div class="section-head"><h2>Internal Events</h2><p>Engine and drip lifecycle events flowing into the <span class="codeish">' + escapeHTML(destDB) + '</span> database. ' +
        (masterEnabled ? '<span class="badge badge-on">master: enabled</span>' : '<span class="badge badge-off">master: disabled</span>') + '</p></div>' +
      '<div class="stack">' +
      '<div class="subpanel"><div class="section-head"><h3>Recent Events (last 1h)</h3><p>Merged feed from <span class="codeish">nanotdb.*</span> and <span class="codeish">drip.*</span>, newest first.</p></div>' +
        (recentRows.length === 0
          ? '<div class="empty">No internal events in the lookback window.</div>'
          : renderTable([
              { key: 'time', label: 'Time' },
              { key: 'name', label: 'Event' },
              { key: 'value', label: 'Value' },
              { key: 'payload', label: 'Payload' },
            ], recentRows)) +
      '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Group Toggles</h3><p>Flip per-group emission on or off. Runtime changes do not persist across restart; persistent changes go in <span class="codeish">engine.toml</span>.</p></div>' +
        (groupsRows.length === 0
          ? '<div class="empty">No groups defined.</div>'
          : renderTable([
              { key: 'name', label: 'Group' },
              { key: 'state', label: 'State' },
              { key: 'defaultState', label: 'Default' },
              { key: 'source', label: 'Source' },
              { key: 'action', label: '' },
            ], groupsRows)) +
      '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Catalog Reference <button type="button" class="link-btn" id="internal-events-catalog-toggle">' +
        (internalEventsCatalogOpen ? 'Hide' : 'Show') + '</button></h3>' +
        '<p>Every event the engine or drip may emit. Useful for chasing down what a specific name means.</p></div>' +
        (internalEventsCatalogOpen ? catalogBody : '') +
      '</div>' +
      '</div>';

    // Wire up the inline buttons after innerHTML replacement — there
    // is no JSX, so event listeners go on after each redraw.
    internalEventsPane.querySelectorAll('button[data-toggle-group]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const group = btn.getAttribute('data-toggle-group');
        const next = btn.getAttribute('data-toggle-next');
        toggleInternalEventsGroup(group, next === 'on');
      });
    });
    const catalogToggleBtn = document.getElementById('internal-events-catalog-toggle');
    if (catalogToggleBtn) {
      catalogToggleBtn.addEventListener('click', () => {
        internalEventsCatalogOpen = !internalEventsCatalogOpen;
        loadInternalEvents();
      });
    }
  }

  async function refreshActiveTab() {
    setStatus('Refreshing ' + activeTab + '...');
    try {
      await loadOverview();
      if (activeTab === 'database') {
        await loadDatabase();
      } else if (activeTab === 'files') {
        await loadFiles();
      } else if (activeTab === 'wal') {
        await loadWAL();
      } else if (activeTab === 'runtime') {
        await loadRuntime();
      } else if (activeTab === 'internal-events') {
        await loadInternalEvents();
      }
      setStatus('Updated ' + new Date().toLocaleTimeString());
    } catch (err) {
      console.error(err);
      setStatus(err && err.message ? err.message : 'Refresh failed');
    }
  }

  function activateTab(name) {
    activeTab = name;
    Object.keys(panes).forEach((key) => {
      panes[key].hidden = key !== name;
    });
    tabButtons.forEach((btn) => {
      const isActive = btn.dataset.tab === name;
      btn.classList.toggle('active', isActive);
      btn.setAttribute('aria-selected', isActive ? 'true' : 'false');
      btn.tabIndex = isActive ? 0 : -1;
    });
    refreshActiveTab();
  }

  function focusTab(offset) {
    const currentIndex = tabButtons.findIndex((btn) => btn.dataset.tab === activeTab);
    if (currentIndex < 0) return;
    const nextIndex = (currentIndex + offset + tabButtons.length) % tabButtons.length;
    const nextTab = tabButtons[nextIndex];
    nextTab.focus();
    activateTab(nextTab.dataset.tab);
  }

  function scheduleRefresh() {
    if (refreshTimer) {
      window.clearInterval(refreshTimer);
      refreshTimer = null;
    }
    const sec = Number(cfg.refreshSeconds || 0);
    if (sec > 0) {
      refreshTimer = window.setInterval(refreshActiveTab, sec * 1000);
    }
  }

  refreshBtn.addEventListener('click', refreshActiveTab);
  dbSelect.addEventListener('change', refreshActiveTab);
  tabButtons.forEach((btn) => {
    btn.addEventListener('click', () => activateTab(btn.dataset.tab));
    btn.addEventListener('keydown', (event) => {
      if (event.key === 'ArrowRight') {
        event.preventDefault();
        focusTab(1);
      } else if (event.key === 'ArrowLeft') {
        event.preventDefault();
        focusTab(-1);
      } else if (event.key === 'Home') {
        event.preventDefault();
        tabButtons[0].focus();
        activateTab(tabButtons[0].dataset.tab);
      } else if (event.key === 'End') {
        event.preventDefault();
        tabButtons[tabButtons.length - 1].focus();
        activateTab(tabButtons[tabButtons.length - 1].dataset.tab);
      }
    });
  });

  scheduleRefresh();
  refreshActiveTab();
})();