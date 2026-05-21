(function () {
  const cfg = window.NANOTDB_DASH_CONFIG || { apiBaseURL: "", refreshSeconds: 10 };
  const dbSelect = document.getElementById("dbSelect");
  const refreshBtn = document.getElementById("refreshBtn");
  const statusEl = document.getElementById("status");
  const overviewPane = document.getElementById("overviewPane");
  const databasePane = document.getElementById("databasePane");
  const filesPane = document.getElementById("filesPane");
  const runtimePane = document.getElementById("runtimePane");
  const panes = {
    overview: overviewPane,
    database: databasePane,
    files: filesPane,
    runtime: runtimePane,
  };
  let activeTab = "overview";
  let refreshTimer = null;
  let selectedDataFileByDB = Object.create(null);
  let fileActionStatusByDB = Object.create(null);
  let showRuntimeWALByDB = Object.create(null);

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
        '<td>' + (row.error || '-') + '</td>' +
        '</tr>';
    }).join('');
    return '<div class="table-wrap"><table><thead><tr><th>Path</th><th>Bytes</th><th>Frames</th><th>Records</th><th>Start</th><th>End</th><th>Error</th></tr></thead><tbody>' + body + '</tbody></table></div>';
  }

  function renderToggle(name, checked, label) {
    return '<label class="toggle-control"><input type="checkbox" class="toggle-input" data-toggle-name="' + escapeHTML(name) + '"' + (checked ? ' checked' : '') + ' /><span>' + label + '</span></label>';
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
        { label: "Open Pages", value: number(items.reduce((sum, item) => sum + (item.open_pages || 0), 0)) },
        { label: "WAL Bytes", value: number(items.reduce((sum, item) => sum + (item.wal_bytes || 0), 0)) },
      ]) +
      renderTable([
        { key: "name", label: "Database" },
        { key: "metricCount", label: "Metrics" },
        { key: "openPages", label: "Open Pages" },
        { key: "dataFiles", label: ".dat Files" },
        { key: "dataBytes", label: ".dat Bytes" },
        { key: "walFiles", label: ".wal Files" },
        { key: "walBytes", label: ".wal Bytes" },
        { key: "walBuffer", label: "WAL Buffer" },
      ], items.map((item) => ({
        name: item.name,
        metricCount: number(item.metric_count),
        openPages: number(item.open_pages),
        dataFiles: number(item.data_files),
        dataBytes: number(item.data_bytes),
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
    const datRows = dataFiles.map((item) => ({
      path: item.path,
      bytes: item.bytes,
      frames: item.frames,
      records: item.records,
      start: item.min_utc || '-',
      end: item.max_utc || '-',
      error: item.scan_error || '',
    }));
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
    const walRows = (result.wal || []).map((item) => ({
      path: '<span class="codeish">' + item.path + '</span>',
      bytes: number(item.bytes),
      records: number(item.records),
      tail: String(!!item.has_tail),
      reason: item.stop_reason || '-',
      error: item.scan_error || '',
    }));
    const recordRows = (result.records || []).map((item) => ({
      index: number(item.index),
      metric: '<span class="codeish">' + (item.metric_name || item.metric_id) + '</span>',
      ts: item.timestamp,
      type: item.value_type,
      value: String(item.value),
    }));
    if (!selectedPath || !dataFiles.some((item) => item.path === selectedPath)) {
      selectedPath = dataFiles.length ? dataFiles[0].path : '';
    }
    if (selectedPath) {
      selectedDataFileByDB[db] = selectedPath;
    }
    const selectedFile = dataFiles.find((item) => item.path === selectedPath) || null;
    const fileActionStatus = fileActionStatusByDB[db] || '';
    const recompactDisabled = !selectedFile || !selectedFile.part || !!selectedFile.active || !!selectedFile.scan_error;
    const recompactHelp = !selectedFile
      ? 'No file selected.'
      : selectedFile.active
        ? 'This partition is still open in memory, so recompact is disabled.'
        : 'Rewrite this sealed file using the current page size limits.';
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
    filesPane.innerHTML = '<div class="section-head"><h2>Files</h2><p>On-disk .dat/.wal inspection, plus decoded WAL records.</p></div>' +
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
      '<div class="subpanel"><div class="section-head"><h3>Pages</h3><p>' + (selectedFile ? '<span class="codeish">' + escapeHTML(selectedFile.path) + '</span>' : 'No file selected.') + '</p></div><div class="subpanel-controls"><div class="action-note">' + recompactHelp + '</div><button type="button" class="action-button" id="recompactSelectedFile"' + (recompactDisabled ? ' disabled' : '') + '>Recompact Selected File</button></div>' + (fileActionStatus ? '<div class="action-status">' + escapeHTML(fileActionStatus) + '</div>' : '') + pagesTable + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>WAL Files</h3><p>Scan status and tail diagnostics.</p></div>' + renderTable([
        { key: 'path', label: 'Path' },
        { key: 'bytes', label: 'Bytes' },
        { key: 'records', label: 'Records' },
        { key: 'tail', label: 'Tail' },
        { key: 'reason', label: 'Stop Reason' },
        { key: 'error', label: 'Error' },
      ], walRows) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Decoded WAL Records</h3><p>Records found while scanning current .wal files.</p></div>' + renderTable([
        { key: 'index', label: 'Idx' },
        { key: 'metric', label: 'Metric' },
        { key: 'ts', label: 'Timestamp' },
        { key: 'type', label: 'Type' },
        { key: 'value', label: 'Value' },
      ], recordRows) + '</div>' +
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
    const recompactBtn = document.getElementById('recompactSelectedFile');
    if (recompactBtn) {
      recompactBtn.addEventListener('click', async () => {
        if (!selectedFile || !selectedFile.part || selectedFile.active) {
          return;
        }
        if (!window.confirm('Recompact ' + selectedFile.path + ' using the current page limits?')) {
          return;
        }
        recompactBtn.disabled = true;
        fileActionStatusByDB[db] = 'Recompacting ' + selectedFile.part + '...';
        setStatus('Recompacting ' + selectedFile.part + '...');
        loadFiles().catch((err) => {
          console.error(err);
        });
        try {
          const payload = await postJSON(apiURL('/api/engine/recompact'), { db: db, part: selectedFile.part });
          const resultPayload = payload.data && payload.data.result;
          fileActionStatusByDB[db] = 'Recompacted ' + selectedFile.part + ': ' + number(resultPayload.old_frames) + ' -> ' + number(resultPayload.new_frames) + ' frames';
          await loadFiles();
          setStatus('Recompacted ' + selectedFile.part + ': ' + number(resultPayload.old_frames) + ' -> ' + number(resultPayload.new_frames) + ' frames');
        } catch (err) {
          console.error(err);
          fileActionStatusByDB[db] = err && err.message ? err.message : 'Recompact failed';
          setStatus(err && err.message ? err.message : 'Recompact failed');
          loadFiles().catch((loadErr) => {
            console.error(loadErr);
          });
        }
      });
    }
  }

  async function loadRuntime() {
    const db = dbSelect.value;
    if (!db) {
      runtimePane.innerHTML = '<div class="empty">No database selected.</div>';
      return;
    }
    const payload = await fetchJSON(apiURL("/api/engine/runtime?db=" + encodeURIComponent(db)));
    const result = payload.data && payload.data.result;
    const runtime = result.runtime || {};
    const showLiveWAL = !!showRuntimeWALByDB[db];
    const liveWALRows = (result.wal || []).map((record) => ({
      index: number(record.index),
      metric: '<span class="codeish">' + (record.metric_name || record.metric_id) + '</span>',
      timestamp: record.timestamp,
      type: record.value_type,
      value: String(record.value),
    }));
    runtimePane.innerHTML = '<div class="section-head"><h2>Runtime</h2><p>Live open pages and current WAL contents for the loaded database.</p></div>' +
      renderSummaryCards([
        { label: 'Open Pages', value: number((runtime.open_pages || []).length) },
        { label: 'WAL Records', value: number((result.wal || []).length) },
        { label: 'WAL Buffer', value: number(runtime.stats && runtime.stats.WAL && runtime.stats.WAL.BufferBytes) },
        { label: 'Recent Flushes', value: number(runtime.stats && runtime.stats.WAL && runtime.stats.WAL.FlushCount) },
      ]) +
      '<div class="stack">' +
      '<div class="subpanel"><div class="section-head"><h3>Open Pages</h3><p>In-memory page state before flush to .dat files.</p></div>' + renderTable([
        { key: 'day', label: 'Day' },
        { key: 'records', label: 'Records' },
        { key: 'metrics', label: 'Unique Metrics' },
        { key: 'bytes', label: 'Value Bytes' },
        { key: 'start', label: 'Start' },
        { key: 'end', label: 'End' },
        { key: 'age', label: 'Age' },
        { key: 'full', label: 'Full' },
      ], (runtime.open_pages || []).map((page) => ({
        day: page.day,
        records: number(page.records),
        metrics: number(page.unique_metrics),
        bytes: number(page.value_bytes),
        start: page.start_timestamp_ns ? new Date(page.start_timestamp_ns / 1e6).toISOString() : '-',
        end: page.end_timestamp_ns ? new Date(page.end_timestamp_ns / 1e6).toISOString() : '-',
        age: durationFromNs(page.age_ns),
        full: String(!!page.full),
      }))) + '</div>' +
      '<div class="subpanel"><div class="section-head"><h3>Live WAL Records</h3><p>Decoded records currently present in the active WAL.</p></div>' +
      '<div class="subpanel-controls">' + renderToggle('show-runtime-wal', showLiveWAL, 'Display live WAL records') + '<span class="subtle-count">' + number(liveWALRows.length) + ' records available</span></div>' +
      (showLiveWAL ? renderTable([
        { key: 'index', label: 'Idx' },
        { key: 'metric', label: 'Metric' },
        { key: 'timestamp', label: 'Timestamp' },
        { key: 'type', label: 'Type' },
        { key: 'value', label: 'Value' },
      ], liveWALRows) : '<div class="empty">Live WAL records are hidden by default. Enable the toggle to render them.</div>') + '</div>' +
      '</div>';
    const toggle = runtimePane.querySelector('[data-toggle-name="show-runtime-wal"]');
    if (toggle) {
      toggle.addEventListener('change', () => {
        showRuntimeWALByDB[db] = toggle.checked;
        loadRuntime().catch((err) => {
          console.error(err);
          setStatus(err && err.message ? err.message : 'Runtime refresh failed');
        });
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
      } else if (activeTab === 'runtime') {
        await loadRuntime();
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
    document.querySelectorAll('.tab').forEach((btn) => {
      btn.classList.toggle('active', btn.dataset.tab === name);
    });
    refreshActiveTab();
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
  document.querySelectorAll('.tab').forEach((btn) => {
    btn.addEventListener('click', () => activateTab(btn.dataset.tab));
  });

  scheduleRefresh();
  refreshActiveTab();
})();