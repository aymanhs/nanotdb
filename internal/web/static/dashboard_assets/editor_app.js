(function () {
  function readEditorConfig() {
    const configEl = document.getElementById("dashboardEditorConfig");
    if (configEl && configEl.textContent) {
      try {
        return JSON.parse(configEl.textContent);
      } catch (err) {
        console.error("failed to parse editor config", err);
      }
    }
    return window.NANOTDB_DASH_CONFIG || { basePath: "/dashboard", editorPath: "/dashboard/edit", refreshSeconds: 10, apiBaseURL: "" };
  }

  const cfg = readEditorConfig();
  const chartState = new Map();
  const state = {
    originalConfig: null,
    draftConfig: null,
    databases: [],
    metricsByDB: new Map(),
    selectedGroupId: "",
    selectedWidgetId: "",
    expandedGroups: new Set(),
    expandedWidgets: new Set(),
    expandedSeries: new Set(),
    previewTimer: null,
  };

  const groupsListEl = document.getElementById("groupsList");
  const widgetsListEl = document.getElementById("widgetsList");
  const widgetEditorEl = document.getElementById("widgetEditor");
  const previewHostEl = document.getElementById("previewHost");
  const previewLabelEl = document.getElementById("previewLabel");
  const widgetUsageBadgeEl = document.getElementById("widgetUsageBadge");
  const statusEl = document.getElementById("editorStatus");
  const backupStatusEl = document.getElementById("backupStatus");
  const titleInput = document.getElementById("dashboardTitleInput");
  const defaultDbSelect = document.getElementById("defaultDbSelect");
  const metricOptions = document.getElementById("metricOptions");
  const aggregateOptions = document.getElementById("aggregateOptions");
  const existingWidgetSelect = document.getElementById("existingWidgetSelect");
  let aggregateCatalog = { result: [], default: "avg" };

  function apiURL(path) {
    const base = typeof cfg.apiBaseURL === "string" ? cfg.apiBaseURL.replace(/\/$/, "") : "";
    return base + path;
  }

  async function fetchJSON(url, options) {
    const res = await fetch(url, Object.assign({ cache: "no-store" }, options || {}));
    const text = await res.text();
    let payload = null;
    if (text) {
      try {
        payload = JSON.parse(text);
      } catch (err) {
        payload = text;
      }
    }
    if (!res.ok) {
      let message = "HTTP " + res.status + " for " + url;
      if (payload && typeof payload === "object" && Array.isArray(payload.errors) && payload.errors.length > 0) {
        message = payload.errors.join("; ");
      } else if (typeof payload === "string" && payload.trim()) {
        message = payload.trim();
      }
      throw new Error(message);
    }
    return payload;
  }

  function setStatus(message, kind) {
    statusEl.textContent = message || "";
    statusEl.className = kind ? "status-" + kind : "";
  }

  function setBackupStatus(message) {
    backupStatusEl.textContent = message || "";
  }

  function deepClone(value) {
    return JSON.parse(JSON.stringify(value));
  }

  function normalizeConfig(input) {
    const config = input && typeof input === "object" ? input : {};
    if (!Array.isArray(config.groups)) {
      config.groups = [];
    }
    if (!config.widgets || typeof config.widgets !== "object") {
      config.widgets = {};
    }
    config.groups.forEach((group) => {
      if (!Array.isArray(group.widgets)) {
        group.widgets = [];
      }
    });
    Object.keys(config.widgets).forEach((widgetId) => {
      const widget = config.widgets[widgetId] || {};
      if (!Array.isArray(widget.series)) {
        widget.series = [];
      }
      config.widgets[widgetId] = widget;
    });
    return config;
  }

  function slugify(value, fallback) {
    const base = String(value || "")
      .trim()
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "_")
      .replace(/^_+|_+$/g, "");
    return base || fallback || "item";
  }

  function uniqueID(existingIDs, proposed) {
    const taken = new Set(existingIDs || []);
    let value = slugify(proposed, "item");
    if (!taken.has(value)) {
      return value;
    }
    let idx = 2;
    while (taken.has(value + "_" + idx)) {
      idx += 1;
    }
    return value + "_" + idx;
  }

  function widgetIDs() {
    return Object.keys((state.draftConfig && state.draftConfig.widgets) || {});
  }

  function selectedGroup() {
    return (state.draftConfig.groups || []).find((group) => group.id === state.selectedGroupId) || null;
  }

  function selectedWidget() {
    if (!state.selectedWidgetId) {
      return null;
    }
    return state.draftConfig.widgets[state.selectedWidgetId] || null;
  }

  function ensureSelection() {
    const groups = state.draftConfig.groups || [];
    if (!groups.find((group) => group.id === state.selectedGroupId)) {
      state.selectedGroupId = groups.length > 0 ? groups[0].id : "";
    }
    const group = selectedGroup();
    if (!group) {
      state.selectedWidgetId = "";
      return;
    }
    if (!group.widgets.includes(state.selectedWidgetId)) {
      state.selectedWidgetId = group.widgets.length > 0 ? group.widgets[0] : "";
    }
    if (state.selectedGroupId) {
      state.expandedGroups.add(state.selectedGroupId);
    }
    if (state.selectedWidgetId) {
      state.expandedWidgets.add(state.selectedWidgetId);
    }
  }

  function widgetUsageMap(config) {
    const usage = new Map();
    (config.groups || []).forEach((group) => {
      (group.widgets || []).forEach((widgetId) => {
        if (!usage.has(widgetId)) {
          usage.set(widgetId, []);
        }
        usage.get(widgetId).push(group.id);
      });
    });
    return usage;
  }

  function firstKnownMetric() {
    const db = state.draftConfig.default_db || state.databases[0] || "";
    const items = state.metricsByDB.get(db) || [];
    return items[0] || "";
  }

  function defaultSeries(index) {
    return {
      label: "Series " + (index + 1),
      query: firstKnownMetric(),
    };
  }

  function syncChartSeries(widget) {
    const widgetType = widgetChartType(widget);
    if ((widgetType !== "line_chart" && widgetType !== "aggregate_band") || !Array.isArray(widget.series)) {
      return;
    }
    const interval = typeof widget.interval === "string" ? widget.interval.trim() : "";
    widget.series.forEach((series) => {
      if (!series) {
        return;
      }
      const usesAggregateWindow = widgetType === "aggregate_band" || Boolean(series.aggregate && String(series.aggregate).trim());
      if (usesAggregateWindow && interval) {
        series.window = interval;
      } else {
        delete series.window;
      }
    });
  }

  function defaultWidget() {
    return {
      type: "numbers",
      title: "New Widget",
      refresh_sec: cfg.refreshSeconds || 10,
      auto_refresh: true,
      series: [defaultSeries(0)],
    };
  }

  function dbOptionsHTML(selectedValue, includeInherit) {
    const options = [];
    if (includeInherit) {
      options.push('<option value="">Inherit default</option>');
    }
    state.databases.forEach((db) => {
      const selected = db === selectedValue ? ' selected' : '';
      options.push('<option value="' + escapeHTML(db) + '"' + selected + '>' + escapeHTML(db) + '</option>');
    });
    return options.join("");
  }

  function populateMetaForm() {
    titleInput.value = state.draftConfig.title || "";
    defaultDbSelect.innerHTML = dbOptionsHTML(state.draftConfig.default_db || "", false);
    if (!defaultDbSelect.value && state.databases.length > 0) {
      defaultDbSelect.value = state.databases[0];
    }
  }

  function populateMetricDatalist() {
    const all = new Set();
    state.metricsByDB.forEach((metrics) => {
      (metrics || []).forEach((metric) => all.add(metric));
    });
    const values = Array.from(all).sort();
    metricOptions.innerHTML = values.map((metric) => '<option value="' + escapeHTML(metric) + '"></option>').join("");
  }

  function populateAggregateDatalist() {
  if (!aggregateOptions) {
    return;
  }
  aggregateOptions.innerHTML = (aggregateCatalog.result || []).map((aggregate) => '<option value="' + escapeHTML(aggregate) + '"></option>').join("");
  }

  async function loadAggregates() {
  const payload = await fetchJSON(apiURL("/api/v1/aggregates"));
  const data = payload && payload.data ? payload.data : {};
  aggregateCatalog = {
    result: Array.isArray(data.result) ? data.result.slice() : [],
    default: typeof data.default === "string" && data.default.trim() ? data.default.trim() : "avg",
  };
  populateAggregateDatalist();
  }

  async function loadMetricsForDB(db) {
    if (!db || state.metricsByDB.has(db)) {
      return;
    }
    const payload = await fetchJSON(apiURL("/api/v1/metrics?db=" + encodeURIComponent(db)));
    const items = (payload.data && payload.data.result) || [];
    state.metricsByDB.set(db, items.slice().sort());
  }

  async function loadDatabasesAndMetrics() {
    const payload = await fetchJSON(apiURL("/api/v1/databases"));
    const databases = ((payload.data && payload.data.result) || []).slice().sort();
    state.databases = databases;
    await Promise.all(databases.map((db) => loadMetricsForDB(db)));
    populateMetricDatalist();
    populateMetaForm();
  }

  function markDirty(message) {
    setStatus(message || "Unsaved changes", "");
  }

  function escapeHTML(value) {
    return String(value == null ? "" : value)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/\"/g, "&quot;");
  }

  function moveArrayItem(items, index, delta) {
    const next = index + delta;
    if (next < 0 || next >= items.length) {
      return;
    }
    const temp = items[index];
    items[index] = items[next];
    items[next] = temp;
  }

  function renameWidget(oldID, newID) {
    const trimmed = slugify(newID, oldID);
    if (!trimmed || trimmed === oldID) {
      return oldID;
    }
    const ids = widgetIDs().filter((id) => id !== oldID);
    const targetID = uniqueID(ids, trimmed);
    const widgets = state.draftConfig.widgets;
    widgets[targetID] = widgets[oldID];
    delete widgets[oldID];
    state.draftConfig.groups.forEach((group) => {
      group.widgets = group.widgets.map((widgetId) => (widgetId === oldID ? targetID : widgetId));
    });
    if (state.selectedWidgetId === oldID) {
      state.selectedWidgetId = targetID;
    }
    return targetID;
  }

  function cleanupSeries(series) {
    if (!series.transform || typeof series.transform !== "object") {
      delete series.transform;
    } else {
      const t = series.transform;
      if (!(typeof t.factor === "number") && t.factor !== 0) delete t.factor;
      if (!(typeof t.offset === "number") && t.offset !== 0) delete t.offset;
      if (!(typeof t.decimals === "number")) delete t.decimals;
      if (!t.unit) delete t.unit;
      if (!t.format) delete t.format;
      if (Object.keys(t).length === 0) delete series.transform;
    }
    if (!series.thresholds || typeof series.thresholds !== "object") {
      delete series.thresholds;
    } else {
      const thresholds = series.thresholds;
      if (!thresholds.direction) delete thresholds.direction;
      if (!(typeof thresholds.warning === "number") && thresholds.warning !== 0) delete thresholds.warning;
      if (!(typeof thresholds.critical === "number") && thresholds.critical !== 0) delete thresholds.critical;
      if (Object.keys(thresholds).length === 0) delete series.thresholds;
    }
    if (!series.db) delete series.db;
    if (!series.database) delete series.database;
    if (!series.label) delete series.label;
    if (!series.query) delete series.query;
    if (!series.metric) delete series.metric;
    if (!series.aggregate) delete series.aggregate;
    if (!series.window) delete series.window;
  }

  function refreshExistingWidgetSelect() {
    const group = selectedGroup();
    const options = ['<option value="">Shared widget</option>'];
    widgetIDs().sort().forEach((widgetId) => {
      if (!group || group.widgets.includes(widgetId)) {
        return;
      }
      options.push('<option value="' + escapeHTML(widgetId) + '">' + escapeHTML(widgetId) + '</option>');
    });
    existingWidgetSelect.innerHTML = options.join("");
  }

  function renderGroups() {
    const groups = state.draftConfig.groups || [];
    groupsListEl.innerHTML = "";
    if (groups.length === 0) {
      groupsListEl.innerHTML = '<div class="widget-editor-empty">No groups yet.</div>';
      return;
    }
    groups.forEach((group, index) => {
      const card = document.createElement("details");
      card.className = "accordion-card" + (group.id === state.selectedGroupId ? " active" : "");
      card.dataset.groupId = group.id;
      card.open = group.id === state.selectedGroupId;
      card.innerHTML =
        '<summary class="accordion-summary">' +
          '<div class="accordion-main">' +
            '<div class="accordion-text">' +
              '<p class="item-card-title">' + escapeHTML(group.label || group.id) + '</p>' +
              '<p class="item-card-meta">' + String(group.widgets.length) + ' widgets</p>' +
            '</div>' +
          '</div>' +
          '<div class="accordion-tools">' +
            '<button type="button" class="editor-btn icon-btn" data-action="move-up" title="Move group up" aria-label="Move group up">▴</button>' +
            '<button type="button" class="editor-btn icon-btn" data-action="move-down" title="Move group down" aria-label="Move group down">▾</button>' +
            '<button type="button" class="editor-btn icon-btn" data-action="delete" title="Delete group" aria-label="Delete group">✕</button>' +
          '</div>' +
        '</summary>' +
        '<div class="accordion-body">' +
          '<div class="form-grid">' +
            '<label>Group ID<input type="text" data-action="group-id" value="' + escapeHTML(group.id) + '" /></label>' +
            '<label>Label<input type="text" data-action="group-label" value="' + escapeHTML(group.label || "") + '" /></label>' +
          '</div>' +
        '</div>';
      const summary = card.querySelector('.accordion-summary');
      const tools = card.querySelector('.accordion-tools');
      tools.querySelectorAll('button').forEach((button) => {
        button.addEventListener('click', (event) => {
          event.preventDefault();
          event.stopPropagation();
        });
      });
      card.addEventListener('toggle', () => {
        if (card.open && state.selectedGroupId !== group.id) {
          state.selectedGroupId = group.id;
          ensureSelection();
          renderAll();
          schedulePreview();
        }
      });
      summary.addEventListener("click", (event) => {
        if (event.target.closest('.accordion-tools')) {
          return;
        }
        state.selectedGroupId = group.id;
        ensureSelection();
        renderAll();
        schedulePreview();
      });
      tools.querySelector('[data-action="move-up"]').addEventListener("click", () => {
        moveArrayItem(groups, index, -1);
        renderAll();
        schedulePreview();
        markDirty();
      });
      tools.querySelector('[data-action="move-down"]').addEventListener("click", () => {
        moveArrayItem(groups, index, 1);
        renderAll();
        schedulePreview();
        markDirty();
      });
      tools.querySelector('[data-action="delete"]').addEventListener("click", () => {
        if (!window.confirm('Delete group "' + (group.label || group.id) + '"?')) {
          return;
        }
        state.expandedGroups.delete(group.id);
        groups.splice(index, 1);
        ensureSelection();
        renderAll();
        schedulePreview();
        markDirty();
      });
      card.querySelector('[data-action="group-id"]').addEventListener("change", (event) => {
        const nextID = uniqueID(groups.filter((_, idx) => idx !== index).map((item) => item.id), event.target.value || group.id);
        group.id = nextID;
        if (state.selectedGroupId === group.id) {
          state.selectedGroupId = nextID;
        } else if (state.selectedGroupId === event.target.defaultValue) {
          state.selectedGroupId = nextID;
        }
        renderAll();
        schedulePreview();
        markDirty();
      });
      card.querySelector('[data-action="group-label"]').addEventListener("change", (event) => {
        group.label = event.target.value;
        renderAll();
        schedulePreview();
        markDirty();
      });
      groupsListEl.appendChild(card);
    });
  }

  function renderWidgets() {
    const group = selectedGroup();
    refreshExistingWidgetSelect();
    widgetsListEl.innerHTML = "";
    if (!group) {
      widgetsListEl.innerHTML = '<div class="widget-editor-empty">Select or add a group first.</div>';
      return;
    }
    const usage = widgetUsageMap(state.draftConfig);
    if (group.widgets.length === 0) {
      widgetsListEl.innerHTML = '<div class="widget-editor-empty">No widgets in this group yet.</div>';
      return;
    }
    group.widgets.forEach((widgetId, index) => {
      const widget = state.draftConfig.widgets[widgetId];
      const groups = usage.get(widgetId) || [];
      const card = document.createElement("article");
      card.className = "accordion-card widget-picker-card" + (widgetId === state.selectedWidgetId ? " active" : "");
      card.dataset.widgetId = widgetId;
      card.innerHTML =
        '<div class="accordion-summary">' +
          '<div class="accordion-main">' +
            '<div class="accordion-text">' +
              '<p class="item-card-title">' + escapeHTML((widget && widget.title) || widgetId) + '</p>' +
              '<p class="item-card-meta">Refresh ' + String((widget && widget.refresh_sec) || cfg.refreshSeconds || 10) + 's' +
              ((widget && widget.type) === 'line_chart' ? ' · ' + escapeHTML(widget.lookback || '1h') + ' / ' + escapeHTML(widget.interval || '30s') : '') + '</p>' +
            '</div>' +
            '<div class="badge-row">' +
              (groups.length > 1 ? '<span class="usage-pill">Used in ' + groups.length + ' groups</span>' : '') +
              (widget && widget.auto_refresh === false ? '<span class="pill">Static</span>' : '') +
            '</div>' +
          '</div>' +
          '<div class="accordion-tools">' +
            '<button type="button" class="editor-btn icon-btn" data-action="move-up" title="Move widget up" aria-label="Move widget up">▴</button>' +
            '<button type="button" class="editor-btn icon-btn" data-action="move-down" title="Move widget down" aria-label="Move widget down">▾</button>' +
            '<button type="button" class="editor-btn icon-btn" data-action="remove" title="Remove widget from group" aria-label="Remove widget from group">✕</button>' +
          '</div>' +
        '</div>';
      const summary = card.querySelector('.accordion-summary');
      const tools = card.querySelector('.accordion-tools');
      tools.querySelectorAll('button').forEach((button) => {
        button.addEventListener('click', (event) => {
          event.preventDefault();
          event.stopPropagation();
        });
      });
      summary.addEventListener("click", (event) => {
        if (event.target.closest('.accordion-tools')) {
          return;
        }
        state.selectedWidgetId = widgetId;
        renderWidgets();
        renderWidgetEditor();
      });
      tools.querySelector('[data-action="move-up"]').addEventListener("click", () => {
        moveArrayItem(group.widgets, index, -1);
        renderWidgets();
        schedulePreview();
        markDirty();
      });
      tools.querySelector('[data-action="move-down"]').addEventListener("click", () => {
        moveArrayItem(group.widgets, index, 1);
        renderWidgets();
        schedulePreview();
        markDirty();
      });
      tools.querySelector('[data-action="remove"]').addEventListener("click", () => {
        group.widgets.splice(index, 1);
        ensureSelection();
        renderAll();
        schedulePreview();
        markDirty();
      });
      widgetsListEl.appendChild(card);
    });
  }

  function optionalNumber(value) {
    const text = String(value == null ? "" : value).trim();
    if (!text) {
      return null;
    }
    const parsed = Number(text);
    return Number.isFinite(parsed) ? parsed : null;
  }

  function renderWidgetEditor() {
    const widget = selectedWidget();
    const widgetId = state.selectedWidgetId;
    const usage = widgetUsageMap(state.draftConfig).get(widgetId) || [];
    if (!widget || !widgetId) {
      widgetUsageBadgeEl.textContent = "";
      widgetEditorEl.className = "widget-editor-empty";
      widgetEditorEl.textContent = "Select a widget to edit it.";
      return;
    }
    widgetUsageBadgeEl.innerHTML = usage.length > 1 ? '<span class="usage-pill">Used in ' + usage.length + ' groups</span>' : '<span class="pill">Used in 1 group</span>';
    widgetEditorEl.className = "";

    syncChartSeries(widget);

    const widgetType = widget.type || "numbers";
    const effectiveWidgetType = widgetChartType(widget);
    const isLineChart = effectiveWidgetType === "line_chart" || effectiveWidgetType === "aggregate_band";
    const isAggregateBand = effectiveWidgetType === "aggregate_band";
    const usesAggregateBandShortcut = isAggregateBand && Array.isArray(widget.series) && widget.series.length === 1;
    const isSingleNumber = widgetType === "number";
    const series = Array.isArray(widget.series) ? widget.series : [];
    const openSeriesKey = Array.from(state.expandedSeries).find((key) => key.startsWith(widgetId + ':')) || (widgetId + ':0');
    const seriesMarkup = series.map((item, idx) => {
      const transform = item.transform || {};
      const thresholds = item.thresholds || {};
      const seriesKey = widgetId + ':' + idx;
      const seriesTitle = isAggregateBand
        ? (usesAggregateBandShortcut
          ? (item.query || item.metric || item.label || "Band Source")
          : (item.aggregate ? item.aggregate.toUpperCase() : (item.label || ("Series " + (idx + 1)))))
        : (item.label || ("Series " + (idx + 1)));
      return (
        '<details class="series-card" data-series-index="' + idx + '"' + (openSeriesKey === seriesKey ? ' open' : '') + '>' +
          '<summary class="series-summary">' +
            '<div class="series-main">' +
              '<div class="accordion-text">' +
                '<h4>' + escapeHTML(seriesTitle) + '</h4>' +
              '</div>' +
            '</div>' +
            '<div class="series-tools">' +
              '<button type="button" class="editor-btn icon-btn" data-action="series-up" title="Move series up" aria-label="Move series up">▴</button>' +
              '<button type="button" class="editor-btn icon-btn" data-action="series-down" title="Move series down" aria-label="Move series down">▾</button>' +
              '<button type="button" class="editor-btn icon-btn" data-action="series-duplicate" title="Duplicate series" aria-label="Duplicate series">⧉</button>' +
              '<button type="button" class="editor-btn icon-btn" data-action="series-remove" title="Remove series" aria-label="Remove series">✕</button>' +
            '</div>' +
          '</summary>' +
          '<div class="series-body">' +
            '<div class="series-grid-primary">' +
              '<label>Query<input type="text" list="metricOptions" data-field="query" value="' + escapeHTML(item.query || item.metric || "") + '" /></label>' +
              '<label>Database<select data-field="db">' + dbOptionsHTML(item.db || item.database || "", true) + '</select></label>' +
              (isAggregateBand ? '' : '<label>Label<input type="text" data-field="label" value="' + escapeHTML(item.label || "") + '" /></label>') +
              (isAggregateBand && !usesAggregateBandShortcut ? '<label>Aggregate<input type="text" data-field="aggregate" placeholder="e.g. avg" value="' + escapeHTML(item.aggregate || "") + '" /></label>' : '') +
            '</div>' +
            (!isAggregateBand ?
            '<div class="series-grid-secondary">' +
              '<label>Aggregate<input type="text" list="aggregateOptions" data-field="aggregate" placeholder="e.g. ' + escapeHTML(aggregateCatalog.default || 'avg') + '" value="' + escapeHTML(item.aggregate || "") + '" /></label>' +
              '<label>Factor<input type="number" step="any" data-field="transform.factor" value="' + escapeHTML(transform.factor == null ? "" : transform.factor) + '" /></label>' +
              '<label>Offset<input type="number" step="any" data-field="transform.offset" value="' + escapeHTML(transform.offset == null ? "" : transform.offset) + '" /></label>' +
              '<label>Unit<input type="text" data-field="transform.unit" value="' + escapeHTML(transform.unit || "") + '" /></label>' +
              '<label>Decimals<input type="number" step="1" min="0" data-field="transform.decimals" value="' + escapeHTML(transform.decimals == null ? "" : transform.decimals) + '" /></label>' +
              '<label>Format<input type="text" data-field="transform.format" placeholder="e.g. {value} or {duration}" value="' + escapeHTML(transform.format || "") + '" title="Format template. Use {value} for the raw/scaled metric, or {duration} for a human-readable duration (e.g. 5d 4h 30m)." /><span class="field-hint">Use <code>{duration}</code> for durations/uptime.</span></label>' +
            '</div>' :
            '<div class="series-grid-secondary">' +
              '<label>Factor<input type="number" step="any" data-field="transform.factor" value="' + escapeHTML(transform.factor == null ? "" : transform.factor) + '" /></label>' +
              '<label>Offset<input type="number" step="any" data-field="transform.offset" value="' + escapeHTML(transform.offset == null ? "" : transform.offset) + '" /></label>' +
              '<label>Unit<input type="text" data-field="transform.unit" value="' + escapeHTML(transform.unit || "") + '" /></label>' +
              '<label>Decimals<input type="number" step="1" min="0" data-field="transform.decimals" value="' + escapeHTML(transform.decimals == null ? "" : transform.decimals) + '" /></label>' +
              '<label>Format<input type="text" data-field="transform.format" placeholder="e.g. {value} or {duration}" value="' + escapeHTML(transform.format || "") + '" title="Format template. Use {value} for the raw/scaled metric, or {duration} for a human-readable duration (e.g. 5d 4h 30m)." /><span class="field-hint">Use <code>{duration}</code> for durations/uptime.</span></label>' +
            '</div>') +
            (isLineChart ? '' :
            '<div class="series-grid-thresholds">' +
              '<label>Threshold<select data-field="thresholds.direction">' +
                '<option value="">None</option>' +
                '<option value="above"' + (thresholds.direction === "above" ? ' selected' : '') + '>Above</option>' +
                '<option value="below"' + (thresholds.direction === "below" ? ' selected' : '') + '>Below</option>' +
              '</select></label>' +
              '<label>Warning<input type="number" step="any" data-field="thresholds.warning" value="' + escapeHTML(thresholds.warning == null ? "" : thresholds.warning) + '" /></label>' +
              '<label>Critical<input type="number" step="any" data-field="thresholds.critical" value="' + escapeHTML(thresholds.critical == null ? "" : thresholds.critical) + '" /></label>' +
            '</div>') +
          '</div>' +
        '</details>'
      );
    }).join("");

    widgetEditorEl.innerHTML =
      '<div class="editor-form">' +
        '<section class="editor-section">' +
          '<div class="widget-top-grid">' +
            '<label>Widget ID<input id="widgetIdInput" type="text" value="' + escapeHTML(widgetId) + '" /></label>' +
            '<label>Title<input id="widgetTitleInput" type="text" value="' + escapeHTML(widget.title || "") + '" /></label>' +
            '<label>Type<select id="widgetTypeSelect">' +
              '<option value="number"' + (widgetType === "number" ? ' selected' : '') + '>Single Number</option>' +
              '<option value="numbers"' + (widgetType === "numbers" ? ' selected' : '') + '>Snapshot</option>' +
              '<option value="line_chart"' + (widgetType === "line_chart" ? ' selected' : '') + '>Line Chart</option>' +
              '<option value="aggregate_band"' + (widgetType === "aggregate_band" ? ' selected' : '') + '>Aggregate Band</option>' +
            '</select></label>' +
            '<label>Refresh Sec<input id="widgetRefreshInput" type="number" min="0" step="1" value="' + escapeHTML(widget.refresh_sec == null ? (cfg.refreshSeconds || 10) : widget.refresh_sec) + '" /></label>' +
            '<label class="toggle-field" title="Toggle automatic refresh"><input id="widgetAutoRefreshInput" type="checkbox"' + (widget.auto_refresh !== false ? ' checked' : '') + ' /><span class="toggle-switch" aria-hidden="true"></span><span class="toggle-label">Auto refresh</span></label>' +
          '</div>' +
          (isLineChart ?
            '<div class="widget-settings-grid">' +
              '<label>Lookback<input id="widgetLookbackInput" type="text" value="' + escapeHTML(widget.lookback || "1h") + '" /></label>' +
              '<label>' + (isAggregateBand ? 'Band / Interval' : 'Interval') + '<input id="widgetIntervalInput" type="text" value="' + escapeHTML(widget.interval || "30s") + '" /></label>' +
            '</div>' :
            '') +
          (isAggregateBand ? '<p class="editor-note">Aggregate band charts use widget interval as the band window. With one series, just choose the metric. Add more series only if you want manual min/avg/max aggregate control.</p>' : '') +
          (effectiveWidgetType === "line_chart" ? '<p class="editor-note">Line-chart aggregate buckets use widget interval for every series. Per-series windows are not used.</p>' : '') +
          (isSingleNumber ? '<p class="editor-note">Single number widgets use only the first series.</p>' : '') +
        '</section>' +
        '<section class="editor-section">' +
          '<div class="pane-head">' +
            '<h3>Series</h3>' +
            '<button type="button" id="addSeriesBtn" class="editor-btn">Add Series</button>' +
          '</div>' +
          (seriesMarkup || '<div class="widget-editor-empty">Add at least one series.</div>') +
        '</section>' +
      '</div>';

    widgetEditorEl.querySelector("#widgetIdInput").addEventListener("change", (event) => {
      const nextID = renameWidget(widgetId, event.target.value);
      state.selectedWidgetId = nextID;
      renderAll();
      schedulePreview();
      markDirty();
    });
    widgetEditorEl.querySelector("#widgetTitleInput").addEventListener("change", (event) => {
      widget.title = event.target.value;
      renderWidgets();
      schedulePreview();
      markDirty();
    });
    widgetEditorEl.querySelector("#widgetTypeSelect").addEventListener("change", (event) => {
      widget.type = event.target.value;
      delete widget.presentation;
      if (widget.type === "line_chart" || widget.type === "aggregate_band") {
        widget.lookback = widget.lookback || "1h";
        widget.interval = widget.interval || "30s";
      } else {
        delete widget.lookback;
        delete widget.interval;
      }
      if (!Array.isArray(widget.series) || widget.series.length === 0) {
        widget.series = [defaultSeries(0)];
      }
      if (widget.type === "line_chart" || widget.type === "aggregate_band") {
        syncChartSeries(widget);
      }
      if (widget.type === "number" && widget.series.length > 1) {
        widget.series = [widget.series[0]];
      }
      renderWidgetEditor();
      renderWidgets();
      schedulePreview();
      markDirty();
    });
    widgetEditorEl.querySelector("#widgetRefreshInput").addEventListener("change", (event) => {
      widget.refresh_sec = Number(event.target.value || 0);
      renderWidgets();
      markDirty();
    });
    widgetEditorEl.querySelector("#widgetAutoRefreshInput").addEventListener("change", (event) => {
      widget.auto_refresh = Boolean(event.target.checked);
      renderWidgets();
      schedulePreview();
      markDirty();
    });
    if (isLineChart) {
      widgetEditorEl.querySelector("#widgetLookbackInput").addEventListener("change", (event) => {
        widget.lookback = event.target.value;
        schedulePreview();
        markDirty();
      });
      widgetEditorEl.querySelector("#widgetIntervalInput").addEventListener("change", (event) => {
        widget.interval = event.target.value;
        syncChartSeries(widget);
        schedulePreview();
        markDirty();
      });
    }
    widgetEditorEl.querySelector("#addSeriesBtn").addEventListener("click", () => {
      if (widget.type === "number") {
        widget.series = [widget.series[0] || defaultSeries(0)];
        state.expandedSeries = new Set([widgetId + ':0']);
      } else {
        widget.series.push(defaultSeries(widget.series.length));
        state.expandedSeries = new Set([widgetId + ':' + (widget.series.length - 1)]);
      }
      syncChartSeries(widget);
      renderWidgetEditor();
      schedulePreview();
      markDirty();
    });
    widgetEditorEl.querySelectorAll(".series-card").forEach((card) => {
      const seriesIndex = Number(card.dataset.seriesIndex);
      const seriesKey = widgetId + ':' + seriesIndex;
      const seriesItem = widget.series[seriesIndex];
      const tools = card.querySelector('.series-tools');
      tools.querySelectorAll('button').forEach((button) => {
        button.addEventListener('click', (event) => {
          event.preventDefault();
          event.stopPropagation();
        });
      });
      card.querySelector('.series-summary').addEventListener('click', (event) => {
        if (event.target.closest('.series-tools')) {
          return;
        }
        event.preventDefault();
        state.expandedSeries = new Set([seriesKey]);
        renderWidgetEditor();
      });
      tools.querySelector('[data-action="series-up"]').addEventListener("click", () => {
        moveArrayItem(widget.series, seriesIndex, -1);
        state.expandedSeries = new Set([widgetId + ':' + Math.max(0, seriesIndex - 1)]);
        renderWidgetEditor();
        schedulePreview();
        markDirty();
      });
      tools.querySelector('[data-action="series-down"]').addEventListener("click", () => {
        moveArrayItem(widget.series, seriesIndex, 1);
        state.expandedSeries = new Set([widgetId + ':' + Math.min(widget.series.length - 1, seriesIndex + 1)]);
        renderWidgetEditor();
        schedulePreview();
        markDirty();
      });
      tools.querySelector('[data-action="series-duplicate"]').addEventListener("click", () => {
        const clone = deepClone(seriesItem);
        widget.series.splice(seriesIndex + 1, 0, clone);
        state.expandedSeries = new Set([widgetId + ':' + (seriesIndex + 1)]);
        renderWidgetEditor();
        schedulePreview();
        markDirty();
      });
      tools.querySelector('[data-action="series-remove"]').addEventListener("click", () => {
        widget.series.splice(seriesIndex, 1);
        if (widget.series.length === 0) {
          widget.series.push(defaultSeries(0));
        }
        if (widget.type === "number" && widget.series.length > 1) {
          widget.series = [widget.series[0]];
        }
        state.expandedSeries = new Set([widgetId + ':' + Math.max(0, Math.min(seriesIndex, widget.series.length - 1))]);
        renderWidgetEditor();
        schedulePreview();
        markDirty();
      });
      card.querySelectorAll("[data-field]").forEach((fieldEl) => {
        fieldEl.addEventListener("change", async (event) => {
          const field = event.target.dataset.field;
          const value = event.target.value;
          if (field === "label") seriesItem.label = value;
          if (field === "query") {
            seriesItem.query = value;
            delete seriesItem.metric;
            delete seriesItem.measurement;
            delete seriesItem.field;
          }
          if (field === "db") {
            delete seriesItem.database;
            if (value) {
              seriesItem.db = value;
              await loadMetricsForDB(value);
              populateMetricDatalist();
            } else {
              delete seriesItem.db;
            }
          }
          if (field === "aggregate") seriesItem.aggregate = value;
          if (field === "role") {
            if (value) seriesItem.role = value;
            else delete seriesItem.role;
          }
          if (field === "window") seriesItem.window = value;
          syncChartSeries(widget);
          if (field === "transform.factor") {
            seriesItem.transform = seriesItem.transform || {};
            const next = optionalNumber(value);
            if (next == null) delete seriesItem.transform.factor;
            else seriesItem.transform.factor = next;
          }
          if (field === "transform.offset") {
            seriesItem.transform = seriesItem.transform || {};
            const next = optionalNumber(value);
            if (next == null) delete seriesItem.transform.offset;
            else seriesItem.transform.offset = next;
          }
          if (field === "transform.unit") {
            seriesItem.transform = seriesItem.transform || {};
            seriesItem.transform.unit = value;
          }
          if (field === "transform.decimals") {
            seriesItem.transform = seriesItem.transform || {};
            const next = optionalNumber(value);
            if (next == null) delete seriesItem.transform.decimals;
            else seriesItem.transform.decimals = Math.max(0, Math.round(next));
          }
          if (field === "transform.format") {
            seriesItem.transform = seriesItem.transform || {};
            seriesItem.transform.format = value;
          }
          if (field === "thresholds.direction") {
            seriesItem.thresholds = seriesItem.thresholds || {};
            if (value) seriesItem.thresholds.direction = value;
            else delete seriesItem.thresholds.direction;
          }
          if (field === "thresholds.warning") {
            seriesItem.thresholds = seriesItem.thresholds || {};
            const next = optionalNumber(value);
            if (next == null) delete seriesItem.thresholds.warning;
            else seriesItem.thresholds.warning = next;
          }
          if (field === "thresholds.critical") {
            seriesItem.thresholds = seriesItem.thresholds || {};
            const next = optionalNumber(value);
            if (next == null) delete seriesItem.thresholds.critical;
            else seriesItem.thresholds.critical = next;
          }
          cleanupSeries(seriesItem);
          renderWidgets();
          schedulePreview();
          markDirty();
        });
      });
    });
  }

  function renderAll() {
    ensureSelection();
    populateMetaForm();
    renderGroups();
    renderWidgets();
    renderWidgetEditor();
  }

  function schedulePreview() {
    if (state.previewTimer != null) {
      clearTimeout(state.previewTimer);
    }
    state.previewTimer = window.setTimeout(() => {
      state.previewTimer = null;
      void renderPreview();
    }, 120);
  }

  const {
    parseDurationSeconds,
    seriesDB,
    seriesMetric,
    seriesWindow,
    effectiveSeriesLabel,
    buildInstantQueryPath,
    buildRangeQueryPath,
    seriesAggregate,
    resolveDisplayConfig,
    transformValue,
    formatDurationFromSeconds,
    formatWidgetValue,
    classifySeverity,
    applySeverityClass,
    pickSeriesColor,
    chartTheme,
    yAxisSizeForValues,
    buildUPlotData,
    chartDisplayTarget,
    rebalanceSingleNumberRows
  } = window.NANOTDB_UTILS;

  function widgetChartType(widget) {
    if (!widget) {
      return "";
    }
    if (widget.type === "aggregate_band") {
      return "aggregate_band";
    }
    if (widget.type === "line_chart" && typeof widget.presentation === "string" && widget.presentation.trim() === "aggregate_band") {
      return "aggregate_band";
    }
    return widget.type || "";
  }

  function seriesRole(series) {
    if (series && typeof series.role === "string" && series.role.trim()) {
      return series.role.trim();
    }
    const aggregate = seriesAggregate(series);
    if (aggregate === "min" || aggregate === "max" || aggregate === "avg") {
      return aggregate;
    }
    return "";
  }

  function chartSeriesStyle(item, idx, presentation) {
    if (presentation !== "aggregate_band") {
      return {
        stroke: pickSeriesColor(idx),
        width: 2,
        dash: [],
        points: { show: true, size: 5, width: 2 },
      };
    }
    if (item && item.role === "avg") {
      return {
        stroke: "#2dd4bf",
        width: 3,
        dash: [],
        points: { show: false },
      };
    }
    if (item && item.role === "min") {
      return {
        stroke: "rgba(94, 234, 212, 0.72)",
        width: 1.5,
        dash: [6, 4],
        points: { show: false },
      };
    }
    if (item && item.role === "max") {
      return {
        stroke: "rgba(45, 212, 191, 0.9)",
        width: 1.5,
        dash: [6, 4],
        points: { show: false },
      };
    }
    return {
      stroke: pickSeriesColor(idx),
      width: 2,
      dash: [],
      points: { show: false },
    };
  }

  function orderChartSeriesItems(items, presentation) {
    if (presentation !== "aggregate_band") {
      return items;
    }
    const used = new Set();
    const ordered = [];
    ["avg", "min", "max"].forEach((role) => {
      const match = items.find((item) => item && item.role === role);
      if (match) {
        ordered.push(match);
        used.add(match);
      }
    });
    items.forEach((item) => {
      if (!used.has(item)) {
        ordered.push(item);
      }
    });
    return ordered;
  }

  function aggregateBandShortcutSeries(series) {
    if (!series) {
      return [];
    }
    return ["avg", "min", "max"].map((role) => Object.assign({}, series, {
      label: role === "avg" ? "Avg" : (role === "min" ? "Min" : "Max"),
      aggregate: role,
      role,
    }));
  }

  function expandedChartSeries(widget) {
    const series = Array.isArray(widget && widget.series) ? widget.series : [];
    if (widgetChartType(widget) === "aggregate_band" && series.length === 1) {
      const item = series[0];
      if (item && item.window && !item.aggregate && !seriesRole(item)) {
        return aggregateBandShortcutSeries(item);
      }
    }
    return series;
  }

  function aggregateBandHooks(items) {
    const minIndex = items.findIndex((item) => item && item.role === "min");
    const maxIndex = items.findIndex((item) => item && item.role === "max");
    if (minIndex < 0 || maxIndex < 0) {
      return {};
    }
    return {
      hooks: {
        drawAxes: [
          (u) => {
            const xData = u.data[0] || [];
            const minData = u.data[minIndex + 1] || [];
            const maxData = u.data[maxIndex + 1] || [];
            let started = false;
            const ctx = u.ctx;
            ctx.save();
            ctx.beginPath();
            for (let i = 0; i < xData.length; i += 1) {
              const minValue = minData[i];
              const maxValue = maxData[i];
              if (!Number.isFinite(minValue) || !Number.isFinite(maxValue)) {
                continue;
              }
              const x = u.valToPos(xData[i], "x", true);
              const y = u.valToPos(maxValue, "y", true);
              if (!started) {
                ctx.moveTo(x, y);
                started = true;
              } else {
                ctx.lineTo(x, y);
              }
            }
            if (started) {
              for (let i = xData.length - 1; i >= 0; i -= 1) {
                const minValue = minData[i];
                const maxValue = maxData[i];
                if (!Number.isFinite(minValue) || !Number.isFinite(maxValue)) {
                  continue;
                }
                const x = u.valToPos(xData[i], "x", true);
                const y = u.valToPos(minValue, "y", true);
                ctx.lineTo(x, y);
              }
              ctx.closePath();
              ctx.fillStyle = "rgba(45, 212, 191, 0.24)";
              ctx.fill();
            }
            ctx.restore();
          },
        ],
      },
    };
  }

  function renderUPlotChart(plotEl, widget, seriesItems) {
    if (typeof uPlot !== "function") {
      throw new Error("uPlot not loaded");
    }
    const presentation = widgetChartType(widget);
    const items = orderChartSeriesItems(Array.isArray(seriesItems) ? seriesItems : [], presentation);
    const data = buildUPlotData(items);
    if (!data[0] || data[0].length === 0) {
      const existing = chartState.get(widget.id);
      if (existing) {
        existing.destroy();
        chartState.delete(widget.id);
      }
      plotEl.innerHTML = "";
      return false;
    }
    const seriesDefs = [{ label: "Time" }];
    items.forEach((item, idx) => {
      const style = chartSeriesStyle(item, idx, presentation);
      seriesDefs.push({
        label: item && item.label ? item.label : ("Series " + (idx + 1)),
        stroke: style.stroke,
        width: style.width,
        dash: style.dash,
        spanGaps: true,
        points: style.points,
      });
    });
    const theme = chartTheme();
    const width = Math.max(plotEl.clientWidth || 0, 280);
    const height = Math.max(plotEl.clientHeight || 0, 220);
    const axisTarget = chartDisplayTarget(widget);
    const opts = {
      width,
      height,
      padding: [8, 8, 4, 0],
      scales: { x: { time: true } },
      series: seriesDefs,
      axes: [
        { stroke: theme.muted, grid: { stroke: theme.border, width: 1 } },
        {
          stroke: theme.muted,
          size: (u, vals) => yAxisSizeForValues(axisTarget, vals),
          grid: { stroke: theme.border, width: 1 },
          values: (u, vals) => vals.map((value) => (value == null ? "" : formatWidgetValue(axisTarget, value))),
        },
      ],
      legend: { show: true, live: true, isolate: false },
    };
    if (presentation === "aggregate_band") {
      Object.assign(opts, aggregateBandHooks(items));
    }
    const existing = chartState.get(widget.id);
    if (existing) {
      existing.destroy();
      chartState.delete(widget.id);
    }
    const instance = new uPlot(opts, data, plotEl);
    chartState.set(widget.id, instance);
    return true;
  }



  async function fetchLast(db, series, lookbackSec) {
    const instantPath = buildInstantQueryPath(db, series);
    if (!instantPath) {
      const points = await fetchRange(db, series, lookbackSec || parseDurationSeconds(seriesWindow(series), 300), "");
      if (!points.length) {
        return null;
      }
      return { ts: points[points.length - 1].x, value: points[points.length - 1].y };
    }
    const payload = await fetchJSON(apiURL(instantPath));
    const item = payload.data && payload.data.result && payload.data.result[0];
    if (!item) {
      return null;
    }
    return { ts: Number(item.value[0]), value: Number(item.value[1]) };
  }

  async function fetchRange(db, series, lookbackSec, step) {
    const end = new Date();
    const start = new Date(end.getTime() - lookbackSec * 1000);
    const path = buildRangeQueryPath(db, series, start.toISOString(), end.toISOString(), step || "30s");
    if (!path) {
      return [];
    }
    const payload = await fetchJSON(apiURL(path));
    const item = payload.data && payload.data.result && payload.data.result[0];
    if (!item || !item.values) {
      return [];
    }
    return item.values.map((value) => ({ x: Number(value[0]), y: Number(value[1]) })).filter((point) => Number.isFinite(point.x) && Number.isFinite(point.y));
  }

  function resolveAggregateBandBatch(chartSeries, dashboardCfg) {
    if (!Array.isArray(chartSeries) || chartSeries.length === 0) {
      return null;
    }
    const first = chartSeries[0];
    const db = seriesDB(first, dashboardCfg);
    const query = seriesMetric(first);
    const window = seriesWindow(first);
    const aggregates = [];
    if (!db || !query || !window) {
      return null;
    }
    for (const series of chartSeries) {
      if (seriesDB(series, dashboardCfg) !== db || seriesMetric(series) !== query || seriesWindow(series) !== window) {
        return null;
      }
      const aggregate = seriesAggregate(series);
      if (!aggregate) {
        return null;
      }
      aggregates.push(aggregate);
    }
    return { db, query, window, aggregates };
  }

  async function fetchAggregateBandSeries(widget, chartSeries, lookbackSec, dashboardCfg) {
    if (widgetChartType(widget) !== "aggregate_band") {
      return null;
    }
    const batch = resolveAggregateBandBatch(chartSeries, dashboardCfg);
    if (!batch) {
      return null;
    }
    const end = new Date();
    const start = new Date(end.getTime() - lookbackSec * 1000);
    const path = "/api/v1/query_range?db=" + encodeURIComponent(batch.db) +
      "&query=" + encodeURIComponent(batch.query) +
      "&start=" + encodeURIComponent(start.toISOString()) +
      "&end=" + encodeURIComponent(end.toISOString()) +
      "&aggregate=" + encodeURIComponent(batch.aggregates.join(",")) +
      "&window=" + encodeURIComponent(batch.window);
    const payload = await fetchJSON(apiURL(path));
    const result = payload.data && payload.data.result ? payload.data.result : [];
    const valuesByAggregate = new Map();
    result.forEach((item) => {
      const aggregate = item && item.metric ? item.metric.aggregate : "";
      const values = item && item.values ? item.values : [];
      valuesByAggregate.set(aggregate, values.map((value) => ({ x: Number(value[0]), y: Number(value[1]) })).filter((point) => Number.isFinite(point.x) && Number.isFinite(point.y)));
    });
    return chartSeries.map((series, idx) => ({
      label: effectiveSeriesLabel(series, idx),
      role: seriesRole(series),
      points: (valuesByAggregate.get(seriesAggregate(series)) || []).map((point) => ({ x: point.x, y: transformValue(series, point.y) })).filter((point) => Number.isFinite(point.y)),
    }));
  }

  function createPreviewHeader(widget) {
    const header = document.createElement("div");
    header.className = "widget-head";

    const title = document.createElement("p");
    title.className = (widgetChartType(widget) === "line_chart" || widgetChartType(widget) === "aggregate_band") ? "chart-title" : "widget-label";
    title.textContent = widget.title || widget.id;

    const badges = document.createElement("div");
    badges.className = "badge-row";
    const typeBadge = document.createElement("span");
    typeBadge.className = "type-pill";
    typeBadge.textContent = widget.type;
    badges.appendChild(typeBadge);
    if (widget.auto_refresh === false) {
      const staticBadge = document.createElement("span");
      staticBadge.className = "pill";
      staticBadge.textContent = "Static";
      badges.appendChild(staticBadge);
    }

    header.appendChild(title);
    header.appendChild(badges);
    return header;
  }

  function mountPreviewError(containerEl, message) {
    const card = document.createElement("article");
    card.className = "widget-error";
    const title = document.createElement("p");
    title.className = "widget-label";
    title.textContent = "Preview Error";
    const body = document.createElement("p");
    body.className = "widget-foot";
    body.textContent = message;
    card.appendChild(title);
    card.appendChild(body);
    containerEl.appendChild(card);
  }

  async function renderPreviewWidget(widget, containerEl, dashboardCfg) {
    if (widget.type === "number") {
      const card = document.createElement("article");
      card.className = "widget-number";
      const value = document.createElement("p");
      value.className = "widget-value";
      value.textContent = "--";
      const foot = document.createElement("p");
      foot.className = "widget-foot";
      foot.textContent = "waiting for data";
      card.appendChild(createPreviewHeader(widget));
      card.appendChild(value);
      card.appendChild(foot);
      containerEl.appendChild(card);

      const series = (widget.series || [])[0];
      const db = series && seriesDB(series, dashboardCfg);
      const query = series && seriesMetric(series);
      if (!db || !query) {
        foot.textContent = "missing db/query";
        return;
      }
      const point = await fetchLast(db, series, parseDurationSeconds(widget.lookback || "5m", 300));
      if (!point) {
        foot.textContent = "no value";
        return;
      }
      value.textContent = formatWidgetValue(series || widget, point.value);
      applySeverityClass(card, classifySeverity(series || widget, point.value));
      foot.textContent = "preview updated " + new Date().toLocaleTimeString();
      return;
    }

    if (widget.type === "numbers") {
      const card = document.createElement("article");
      card.className = "widget-numbers";
      const list = document.createElement("div");
      list.className = "numbers-list";
      const foot = document.createElement("p");
      foot.className = "widget-foot";
      foot.textContent = "waiting for data";
      card.appendChild(createPreviewHeader(widget));
      card.appendChild(list);
      card.appendChild(foot);
      containerEl.appendChild(card);

      let validCount = 0;
      await Promise.all((widget.series || []).map(async (series, idx) => {
        const row = document.createElement("div");
        row.className = "numbers-row";
        const label = document.createElement("span");
        label.className = "numbers-row-label";
        label.textContent = series.label || ("Series " + (idx + 1));
        const value = document.createElement("span");
        value.className = "numbers-row-value";
        value.textContent = "--";
        row.appendChild(label);
        row.appendChild(value);
        list.appendChild(row);

        const db = seriesDB(series, dashboardCfg);
        const query = seriesMetric(series);
        if (!db || !query) {
          return;
        }
        const point = await fetchLast(db, series, parseDurationSeconds(widget.lookback || "5m", 300));
        if (!point) {
          return;
        }
        validCount += 1;
        value.textContent = formatWidgetValue(series, point.value);
        const severity = classifySeverity(series, point.value);
        if (severity !== "none") {
          row.classList.add("value-" + severity);
        }
      }));
      foot.textContent = validCount > 0 ? "preview updated " + new Date().toLocaleTimeString() : "no values";
      return;
    }

    if (widgetChartType(widget) === "line_chart" || widgetChartType(widget) === "aggregate_band") {
      const card = document.createElement("article");
      card.className = "widget-chart" + (widgetChartType(widget) === "aggregate_band" ? " widget-chart--aggregate-band" : "");
      const plot = document.createElement("div");
      plot.className = "chart-plot";
      const foot = document.createElement("p");
      foot.className = "widget-foot";
      foot.textContent = "waiting for data";
      card.appendChild(createPreviewHeader(widget));
      card.appendChild(plot);
      card.appendChild(foot);
      containerEl.appendChild(card);

      const lookbackSec = parseDurationSeconds(widget.lookback || "1h", 3600);
      const step = widget.interval || "30s";
      const chartSeries = expandedChartSeries(widget);
      let seriesItems = await fetchAggregateBandSeries(widget, chartSeries, lookbackSec, dashboardCfg);
      if (!seriesItems) {
        const fetchedItems = new Array(chartSeries.length);
        await Promise.all(chartSeries.map(async (series, idx) => {
          const db = seriesDB(series, dashboardCfg);
          const query = seriesMetric(series);
          if (!db || !query) {
            return;
          }
          const points = await fetchRange(db, series, lookbackSec, step);
          fetchedItems[idx] = {
            label: effectiveSeriesLabel(series, idx),
            role: seriesRole(series),
            points: points.map((point) => ({ x: point.x, y: transformValue(series, point.y) })).filter((point) => Number.isFinite(point.y)),
          };
        }));
        seriesItems = fetchedItems.filter(Boolean);
      }
      const hasData = renderUPlotChart(plot, widget, seriesItems.filter(Boolean));
      foot.textContent = widgetChartType(widget) === "aggregate_band" ? "" : (hasData ? "preview updated " + new Date().toLocaleTimeString() : "no points");
      return;
    }

    mountPreviewError(containerEl, "Unsupported widget type: " + widget.type);
  }

  async function renderPreview() {
    chartState.forEach((instance) => instance.destroy());
    chartState.clear();
    previewHostEl.innerHTML = "";

    const group = selectedGroup();
    if (!group) {
      previewLabelEl.textContent = "No group selected";
      previewHostEl.innerHTML = '<div class="preview-empty">Select a group to preview it.</div>';
      return;
    }
    previewLabelEl.textContent = group.label || group.id;
    if (!group.widgets || group.widgets.length === 0) {
      previewHostEl.innerHTML = '<div class="preview-empty">This group does not have any widgets yet.</div>';
      return;
    }

    const pane = document.createElement("div");
    pane.className = "widgets";
    previewHostEl.appendChild(pane);
    for (const widgetId of group.widgets) {
      const widget = state.draftConfig.widgets[widgetId];
      if (!widget) {
        mountPreviewError(pane, "Unknown widget id: " + widgetId);
        continue;
      }
      try {
        await renderPreviewWidget(Object.assign({}, widget, { id: widgetId }), pane, state.draftConfig);
      } catch (err) {
        mountPreviewError(pane, err && err.message ? err.message : String(err));
      }
    }
    requestAnimationFrame(() => rebalanceSingleNumberRows(pane));
  }

  async function validateDraft() {
    await fetchJSON(apiURL("/api/dashboard-config/validate"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(state.draftConfig),
    });
  }

  async function saveDraft() {
    return fetchJSON(apiURL("/api/dashboard-config"), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(state.draftConfig),
    });
  }

  function wireStaticControls() {
    document.getElementById("addGroupBtn").addEventListener("click", () => {
      const id = uniqueID((state.draftConfig.groups || []).map((group) => group.id), "new_group");
      state.draftConfig.groups.push({ id, label: "New Group", widgets: [] });
      state.selectedGroupId = id;
      state.selectedWidgetId = "";
      renderAll();
      schedulePreview();
      markDirty();
      const groupIDInput = groupsListEl.querySelector('details[data-group-id="' + id + '"] [data-action="group-id"]');
      if (groupIDInput) {
        groupIDInput.focus();
        groupIDInput.select();
      }
    });

    document.getElementById("addExistingWidgetBtn").addEventListener("click", () => {
      const group = selectedGroup();
      const widgetId = existingWidgetSelect.value;
      if (!group || !widgetId) {
        return;
      }
      if (!group.widgets.includes(widgetId)) {
        group.widgets.push(widgetId);
      }
      state.selectedWidgetId = widgetId;
      renderAll();
      schedulePreview();
      markDirty();
    });

    document.getElementById("addWidgetBtn").addEventListener("click", () => {
      const group = selectedGroup();
      if (!group) {
        return;
      }
      const widgetId = uniqueID(widgetIDs(), "widget");
      const widget = defaultWidget();
      widget.title = widgetId.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
      state.draftConfig.widgets[widgetId] = widget;
      group.widgets.push(widgetId);
      state.selectedWidgetId = widgetId;
      renderAll();
      schedulePreview();
      markDirty();
      const widgetIDInput = widgetEditorEl.querySelector("#widgetIdInput");
      if (widgetIDInput) {
        widgetIDInput.focus();
        widgetIDInput.select();
      }
    });

    titleInput.addEventListener("change", (event) => {
      state.draftConfig.title = event.target.value;
      markDirty();
    });

    defaultDbSelect.addEventListener("change", async (event) => {
      state.draftConfig.default_db = event.target.value;
      await loadMetricsForDB(event.target.value);
      populateMetricDatalist();
      schedulePreview();
      markDirty();
    });

    document.getElementById("previewBtn").addEventListener("click", () => {
      void renderPreview();
    });

    document.getElementById("validateBtn").addEventListener("click", async () => {
      try {
        setStatus("Validating dashboard draft...", "");
        await validateDraft();
        setStatus("Draft is valid.", "success");
      } catch (err) {
        setStatus(err.message, "error");
      }
    });

    document.getElementById("revertBtn").addEventListener("click", () => {
      if (!state.originalConfig) {
        return;
      }
      state.draftConfig = deepClone(state.originalConfig);
      normalizeConfig(state.draftConfig);
      ensureSelection();
      renderAll();
      schedulePreview();
      setBackupStatus("");
      setStatus("Reverted to server config.", "");
    });

    document.getElementById("saveBtn").addEventListener("click", async () => {
      try {
        setStatus("Saving dashboard config...", "");
        const response = await saveDraft();
        const savedConfig = response && response.config ? response.config : state.draftConfig;
        state.originalConfig = deepClone(savedConfig);
        state.draftConfig = deepClone(savedConfig);
        normalizeConfig(state.draftConfig);
        ensureSelection();
        renderAll();
        schedulePreview();
        setStatus("Saved dashboard config.", "success");
        setBackupStatus(response && response.backup_path ? "Backup: " + response.backup_path : "Saved without backup.");
      } catch (err) {
        setStatus(err.message, "error");
      }
    });
  }

  async function init() {
    try {
      await loadAggregates();
      const dashboardConfig = normalizeConfig(await fetchJSON(apiURL("/api/dashboard-config")));
      state.originalConfig = deepClone(dashboardConfig);
      state.draftConfig = deepClone(dashboardConfig);
      ensureSelection();
      wireStaticControls();
      await loadDatabasesAndMetrics();
      renderAll();
      await renderPreview();
      setStatus("Loaded live server dashboard config.", "");
    } catch (err) {
      setStatus(err && err.message ? err.message : String(err), "error");
      previewHostEl.innerHTML = '<div class="preview-empty">Failed to load editor.</div>';
    }
  }

  init();
})();