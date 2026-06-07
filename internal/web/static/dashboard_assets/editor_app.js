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
    eventsByDB: new Map(),
    // Per-db internal-events catalog (when the db is "internal"),
    // shaped as [{group, events: [{name, description, value_type}]}].
    // Populated by loadEventsForDB. Currently unused by the overlay
    // editor (which uses native datalist autocomplete) but kept for
    // future use by other UI surfaces.
    internalEventsCatalog: null,
    // Debounce timers for per-overlay "recent matches" preview
    // fetches. Keyed by overlay index within the currently selected
    // widget; reset on widget switch since the editor re-renders the
    // whole card every time.
    overlayPreviewTimers: new Map(),
    overlayPreviewCache: new Map(),
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
  const eventOptions = document.getElementById("eventOptions");
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

  async function loadEventsForDB(db) {
    if (!db || state.eventsByDB.has(db)) {
      return;
    }
    try {
      const payload = await fetchJSON(apiURL("/api/v1/events/catalog?db=" + encodeURIComponent(db)));
      const items = (payload.data && payload.data.result) || [];
      const names = items.map((it) => it.name).filter(Boolean);
      // For the engine's `internal` db, also fold in every event name
      // the internal-events registry knows about, even if it hasn't
      // actually been emitted yet (and so isn't in the per-db events
      // catalog). Lets operators picker-complete "nanotdb.partition.*"
      // before the first partition has sealed.
      if (db === "internal") {
        try {
          const ie = await fetchJSON(apiURL("/api/v1/internal-events/catalog"));
          const groups = (ie.data && ie.data.groups) || [];
          // Cache the grouped catalog for the overlay picker's
          // group-headers rendering. We keep the raw [{name,
          // events:[...]}] shape so the picker can avoid an extra
          // fetch when the overlay db is "internal".
          state.internalEventsCatalog = groups.map((g) => ({
            name: g.name || "",
            events: (g.events || [])
              .filter((e) => e && e.name)
              .map((e) => ({
                name: e.name,
                value_type: e.value_type,
                description: e.description || "",
              })),
          })).filter((g) => g.events.length > 0);
          groups.forEach((g) => {
            (g.events || []).forEach((e) => {
              if (e && e.name) {
                names.push(e.name);
              }
            });
          });
        } catch (err) {
          // Older nanotdb without the endpoint — fall back to the
          // events-catalog list we already have.
        }
      }
      const dedup = Array.from(new Set(names)).sort();
      state.eventsByDB.set(db, dedup);
    } catch (err) {
      state.eventsByDB.set(db, []);
    }
  }

  function populateEventDatalist() {
    if (!eventOptions) {
      return;
    }
    const all = new Set();
    state.eventsByDB.forEach((names) => {
      (names || []).forEach((n) => all.add(n));
    });
    const values = Array.from(all).sort();
    eventOptions.innerHTML = values.map((n) => '<option value="' + escapeHTML(n) + '"></option>').join("");
  }

  async function loadDatabasesAndMetrics() {
    // include_internal=true so widgets editing the internal-events
    // feed can pick "internal" from the db dropdown. The public
    // /api/v1/databases route hides it by default to keep ordinary
    // dashboards from displaying engine telemetry as a user db, but
    // the editor needs the full list.
    const payload = await fetchJSON(apiURL("/api/v1/databases?include_internal=true"));
    const databases = ((payload.data && payload.data.result) || []).slice().sort();
    state.databases = databases;
    await Promise.all(databases.map((db) => loadMetricsForDB(db)));
    await Promise.all(databases.map((db) => loadEventsForDB(db)));
    populateMetricDatalist();
    populateEventDatalist();
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
    // Show the user-visible widget title in the dropdown (with the
    // auto-generated ID as fallback). Widget IDs are hidden from the
    // editor UI, so titles are the only stable label users recognise.
    const entries = widgetIDs().map((id) => {
      const widget = state.draftConfig.widgets[id] || {};
      return { id, label: widget.title || id };
    });
    entries.sort((a, b) => a.label.localeCompare(b.label));
    entries.forEach(({ id, label }) => {
      if (!group || group.widgets.includes(id)) {
        return;
      }
      options.push('<option value="' + escapeHTML(id) + '">' + escapeHTML(label) + '</option>');
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

  // ---------------------------------------------------------------------
  // Event-overlay picker — renders the overlay card with a real
  // dropdown of event names scoped to the chosen db, plus a custom-
  // pattern toggle for wildcards. Also renders an inline "recent
  // matches" preview that auto-refreshes 300ms after each change.
  // ---------------------------------------------------------------------

  // ---------------------------------------------------------------------
  // Event-overlay card rendering. Uses the same native datalist
  // autocomplete pattern as the metric query field: one text input
  // wired to the global #eventOptions datalist. Operators get type-
  // ahead suggestions for known event names and can still type a
  // wildcard pattern (e.g. `disk.*`) for the custom-pattern case.
  // The card also renders an inline "recent matches" preview that
  // auto-refreshes 300ms after each change.
  // ---------------------------------------------------------------------

  // renderOverlayCardHTML returns the full HTML for one overlay
  // card. Handlers for [data-overlay-field] inputs are wired by the
  // overlay-binding section of renderWidgetEditor.
  function renderOverlayCardHTML(overlay, oi, draftCfg) {
    const overlayDB = overlay.db || overlay.database || (draftCfg && draftCfg.default_db) || "";
    const currentValue = overlay && overlay.event_name_pattern || "";

    const previewHTML =
      '<div class="overlay-preview" data-overlay-preview="' + oi + '">' +
        '<div class="overlay-preview-head">Recent matches</div>' +
        '<div class="overlay-preview-body" data-overlay-preview-body="' + oi + '">' +
          '<div class="overlay-preview-empty">Pick an event to preview recent matches.</div>' +
        '</div>' +
      '</div>';

    return (
      '<div class="series-card overlay-card" data-overlay-index="' + oi + '">' +
        '<div class="series-body">' +
          '<div class="overlay-grid">' +
            '<label>Database<select data-overlay-field="db">' + dbOptionsHTML(overlayDB, true) + '</select></label>' +
            '<label class="overlay-event-control">Event' +
              '<input type="text" list="eventOptions" data-overlay-field="event_name_pattern" placeholder="e.g. nanotdb.mqtt.connected or disk.*" value="' + escapeHTML(currentValue) + '" />' +
            '</label>' +
            '<label>Label<input type="text" data-overlay-field="label" placeholder="(optional)" value="' + escapeHTML(overlay.label || "") + '" /></label>' +
            '<label>Color<input type="text" data-overlay-field="color" placeholder="e.g. #c00 or red" value="' + escapeHTML(overlay.color || "") + '" /></label>' +
            '<label>Limit<input type="number" min="1" step="1" data-overlay-field="event_limit" value="' + escapeHTML(overlay.event_limit || 200) + '" /></label>' +
          '</div>' +
          previewHTML +
          '<div class="series-tools">' +
            '<button type="button" class="editor-btn icon-btn" data-overlay-action="remove" title="Remove overlay" aria-label="Remove overlay">✕</button>' +
          '</div>' +
        '</div>' +
      '</div>'
    );
  }

  // attachOverlayPickerHandlers is intentionally empty: the simple
  // datalist input is wired via the standard [data-overlay-field]
  // change/input listeners in renderWidgetEditor. Kept as a no-op so
  // older call sites don't need to be renamed.
  function attachOverlayPickerHandlers(_card, _overlay, _oi) {}

  // schedulePreviewFetch debounces a /api/v1/events query for the
  // given overlay's current (db, pattern) and renders the result in
  // the overlay-preview body. 300ms debounce per spec.
  function schedulePreviewFetch(overlay, oi) {
    const prev = state.overlayPreviewTimers.get(oi);
    if (prev != null) {
      window.clearTimeout(prev);
    }
    const timer = window.setTimeout(() => {
      void renderOverlayPreview(overlay, oi);
    }, 300);
    state.overlayPreviewTimers.set(oi, timer);
  }

  async function renderOverlayPreview(overlay, oi) {
    const body = document.querySelector('[data-overlay-preview-body="' + oi + '"]');
    if (!body) return;
    const db = overlay.db || overlay.database || (state.draftConfig && state.draftConfig.default_db) || "";
    const pattern = (overlay && overlay.event_name_pattern || "").trim();
    if (!db || !pattern) {
      body.innerHTML = '<div class="overlay-preview-empty">Pick an event to preview recent matches.</div>';
      return;
    }
    body.innerHTML = '<div class="overlay-preview-empty">Loading…</div>';
    try {
      const end = new Date();
      const start = new Date(end.getTime() - 60 * 60 * 1000); // last 1h
      const url = apiURL(
        "/api/v1/events?db=" + encodeURIComponent(db) +
        "&name=" + encodeURIComponent(pattern) +
        "&start=" + encodeURIComponent(start.toISOString()) +
        "&end=" + encodeURIComponent(end.toISOString()) +
        "&limit=5"
      );
      const payload = await fetchJSON(url);
      const events = (payload.data && payload.data.result) || [];
      if (events.length === 0) {
        body.innerHTML = '<div class="overlay-preview-empty">No matches in the last hour.</div>';
        return;
      }
      const rows = events.map((evt) => {
        const t = Number(evt && evt.ts);
        const when = Number.isFinite(t) ? new Date(t / 1e6).toLocaleTimeString() : "?";
        const val = (typeof evt.int32 === "number" ? evt.int32 : (typeof evt.float32 === "number" ? evt.float32 : ""));
        const valText = val === "" ? "" : ' <span class="overlay-preview-value">' + escapeHTML(String(val)) + '</span>';
        return '<div class="overlay-preview-row">' +
          '<span class="overlay-preview-time">' + escapeHTML(when) + '</span>' +
          '<span class="overlay-preview-name codeish">' + escapeHTML(evt.name || "") + '</span>' +
          valText +
        '</div>';
      }).join("");
      body.innerHTML = rows;
    } catch (err) {
      body.innerHTML = '<div class="overlay-preview-empty">Preview failed: ' + escapeHTML(err.message || String(err)) + '</div>';
    }
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
    const isEventLog = widgetType === "event_log";
    const usesAggregateBandShortcut = isAggregateBand && Array.isArray(widget.series) && widget.series.length === 1;
    const isSingleNumber = widgetType === "number";
    const series = Array.isArray(widget.series) ? widget.series : [];
    const openSeriesKey = Array.from(state.expandedSeries).find((key) => key.startsWith(widgetId + ':')) || (widgetId + ':0');
    const seriesMarkup = series.map((item, idx) => {
      const transform = item.transform || {};
      const thresholds = item.thresholds || {};
      const seriesKey = widgetId + ':' + idx;
      // For line_chart only, a series may be event-backed (each event = one
      // scatter point) instead of metric-backed. The mode is implicit in the
      // shape of the series JSON: if event_name_pattern is non-empty, the
      // series is event-backed. A user-facing radio toggle switches between
      // the two field sets and clears the inactive fields so saved JSON stays
      // strictly one source per series (validator rejects mixed shapes).
      const isEventBacked = !isEventLog && !isAggregateBand && isLineChart && !!(item.event_name_pattern || "").trim();
      const showEventFields = isEventLog || isEventBacked;
      const seriesTitle = isAggregateBand
        ? (usesAggregateBandShortcut
          ? (item.query || item.metric || item.label || "Band Source")
          : (item.aggregate ? item.aggregate.toUpperCase() : (item.label || ("Series " + (idx + 1)))))
        : (showEventFields
          ? (item.label || item.event_name_pattern || ("Series " + (idx + 1)))
          : (item.label || ("Series " + (idx + 1))));
      // Source toggle is line_chart-only. Aggregate_band and event_log are
      // never user-switchable here.
      // Source mode is line_chart-only (aggregate_band and event_log
      // are never user-switchable here). Rendered as a labeled select
      // inside the primary grid so it lines up with DB/Query/Label and
      // styles consistently with the other controls.
      const sourceSelect = isLineChart && !isAggregateBand
        ? ('<label title="Pick whether this series is sourced from a metric (time-bucketed aggregate) or from individual events (one point per event).">Source<select data-field="source-mode">' +
            '<option value="metric"' + (isEventBacked ? '' : ' selected') + '>Metric</option>' +
            '<option value="event"' + (isEventBacked ? ' selected' : '') + '>Event</option>' +
          '</select></label>')
        : '';
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
            (showEventFields ?
            '<div class="series-grid-primary">' +
              sourceSelect +
              '<label>Database<select data-field="db">' + dbOptionsHTML(item.db || item.database || "", true) + '</select></label>' +
              '<label>Event Name Pattern<input type="text" list="eventOptions" data-field="event_name_pattern" placeholder="e.g. disk.sd_write_probe.*" value="' + escapeHTML(item.event_name_pattern || "") + '" title="Supports wildcards with *. Suggestions come from the catalog." /></label>' +
              '<label>Limit<input type="number" min="1" step="1" data-field="event_limit" value="' + escapeHTML(item.event_limit || (isEventBacked ? 1000 : 10)) + '" /></label>' +
              (isEventBacked ? '<label>Label<input type="text" data-field="label" value="' + escapeHTML(item.label || "") + '" /></label>' : '') +
            '</div>' :
            '<div class="series-grid-primary">' +
              sourceSelect +
              '<label>Database<select data-field="db">' + dbOptionsHTML(item.db || item.database || "", true) + '</select></label>' +
              '<label>Query<input type="text" list="metricOptions" data-field="query" value="' + escapeHTML(item.query || item.metric || "") + '" /></label>' +
              (isAggregateBand ? '' : '<label>Label<input type="text" data-field="label" value="' + escapeHTML(item.label || "") + '" /></label>') +
              (isAggregateBand && !usesAggregateBandShortcut ? '<label>Aggregate<input type="text" data-field="aggregate" placeholder="e.g. avg" value="' + escapeHTML(item.aggregate || "") + '" /></label>' : '') +
              (!isAggregateBand ? '<label>Aggregate<input type="text" list="aggregateOptions" data-field="aggregate" placeholder="e.g. ' + escapeHTML(aggregateCatalog.default || 'avg') + '" value="' + escapeHTML(item.aggregate || "") + '" /></label>' : '') +
              '<label>Factor<input type="number" step="any" data-field="transform.factor" value="' + escapeHTML(transform.factor == null ? "" : transform.factor) + '" /></label>' +
              '<label>Offset<input type="number" step="any" data-field="transform.offset" value="' + escapeHTML(transform.offset == null ? "" : transform.offset) + '" /></label>' +
              '<label>Unit<input type="text" data-field="transform.unit" value="' + escapeHTML(transform.unit || "") + '" /></label>' +
              '<label>Decimals<input type="number" step="1" min="0" data-field="transform.decimals" value="' + escapeHTML(transform.decimals == null ? "" : transform.decimals) + '" /></label>' +
              '<label>Format<input type="text" data-field="transform.format" placeholder="e.g. {value} or {duration}" value="' + escapeHTML(transform.format || "") + '" title="Format template. Use {value} for the raw/scaled metric, or {duration} for a human-readable duration (e.g. 5d 4h 30m)." /></label>' +
            '</div>') +
            (isEventLog || isLineChart ? '' :
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

    // event_overlays only render on line_chart widgets (not aggregate_band,
    // not event_log). The editor section is shown alongside series and
    // accepts pattern, db, color, label, limit per overlay row.
    const overlays = Array.isArray(widget.event_overlays) ? widget.event_overlays : [];
    const overlaysSection = (effectiveWidgetType === "line_chart" && !isAggregateBand) ? (
      '<section class="editor-section">' +
        '<div class="pane-head">' +
          '<h3>Event Overlays</h3>' +
          '<button type="button" id="addOverlayBtn" class="editor-btn">Add Overlay</button>' +
        '</div>' +
        (overlays.length === 0
          ? '<p class="editor-note">No overlays. Add one to drop vertical markers on this chart at the timestamps of matching events.</p>'
          : overlays.map((ov, oi) => renderOverlayCardHTML(ov, oi, state.draftConfig)).join("")) +
      '</section>'
    ) : '';

    widgetEditorEl.innerHTML =
      '<div class="editor-form">' +
        '<section class="editor-section">' +
          '<div class="widget-top-grid">' +
            '<label>Title<input id="widgetTitleInput" type="text" value="' + escapeHTML(widget.title || "") + '" /></label>' +
            '<label>Type<select id="widgetTypeSelect">' +
              '<option value="number"' + (widgetType === "number" ? ' selected' : '') + '>Single Number</option>' +
              '<option value="numbers"' + (widgetType === "numbers" ? ' selected' : '') + '>Snapshot</option>' +
              '<option value="line_chart"' + (widgetType === "line_chart" ? ' selected' : '') + '>Line Chart</option>' +
              '<option value="aggregate_band"' + (widgetType === "aggregate_band" ? ' selected' : '') + '>Aggregate Band</option>' +
              '<option value="event_log"' + (widgetType === "event_log" ? ' selected' : '') + '>Event Log</option>' +
            '</select></label>' +
            '<label>Refresh Sec<input id="widgetRefreshInput" type="number" min="0" step="1" value="' + escapeHTML(widget.refresh_sec == null ? (cfg.refreshSeconds || 10) : widget.refresh_sec) + '" /></label>' +
            (isLineChart || isEventLog ? '<label>Lookback<input id="widgetLookbackInput" type="text" value="' + escapeHTML(widget.lookback || "1h") + '" /></label>' : '') +
            (isLineChart ? '<label>' + (isAggregateBand ? 'Band / Interval' : 'Interval') + '<input id="widgetIntervalInput" type="text" value="' + escapeHTML(widget.interval || "30s") + '" /></label>' : '') +
            '<label class="toggle-field" title="Toggle automatic refresh"><input id="widgetAutoRefreshInput" type="checkbox"' + (widget.auto_refresh !== false ? ' checked' : '') + ' /><span class="toggle-switch" aria-hidden="true"></span><span class="toggle-label">Auto refresh</span></label>' +
          '</div>' +
          (isAggregateBand ? '<p class="editor-note">Aggregate band charts use widget interval as the band window. With one series, just choose the metric. Add more series only if you want manual min/avg/max aggregate control.</p>' : '') +
          (effectiveWidgetType === "line_chart" ? '<p class="editor-note">Line-chart aggregate buckets use widget interval for every series. Per-series windows are not used. Event-backed series ignore aggregate/window — each event becomes one scatter point.</p>' : '') +
          (isSingleNumber ? '<p class="editor-note">Single number widgets use only the first series.</p>' : '') +
        '</section>' +
        '<section class="editor-section">' +
          '<div class="pane-head">' +
            '<h3>Series</h3>' +
            '<button type="button" id="addSeriesBtn" class="editor-btn">Add Series</button>' +
          '</div>' +
          (seriesMarkup || '<div class="widget-editor-empty">Add at least one series.</div>') +
        '</section>' +
        overlaysSection +
      '</div>';

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
      } else if (widget.type === "event_log") {
        widget.lookback = widget.lookback || "1h";
        delete widget.interval;
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
    if (isLineChart || isEventLog) {
      widgetEditorEl.querySelector("#widgetLookbackInput").addEventListener("change", (event) => {
        widget.lookback = event.target.value;
        schedulePreview();
        markDirty();
      });
    }
    if (isLineChart) {
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
    // Series cards are <details class="series-card">. Overlay cards reuse
    // the series-card class for styling but are <div>s with their own
    // wiring further down; the :not(.overlay-card) guard keeps this loop
    // from trying to wire a .series-summary that overlay cards do not have.
    widgetEditorEl.querySelectorAll("details.series-card:not(.overlay-card)").forEach((card) => {
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
          // Event-backed line-chart series fields. event_log series go through
          // the same handlers since the field names are identical; the
          // source-mode toggle only fires on line_chart series.
          if (field === "event_name_pattern") {
            seriesItem.event_name_pattern = value;
            // Event-backed series cannot also carry metric-shape fields
            // (validator rejects mixed shapes — keep saved JSON clean).
            delete seriesItem.query;
            delete seriesItem.metric;
            delete seriesItem.measurement;
            delete seriesItem.field;
            delete seriesItem.aggregate;
            delete seriesItem.window;
          }
          if (field === "event_limit") {
            const next = optionalNumber(value);
            if (next == null || next <= 0) {
              delete seriesItem.event_limit;
            } else {
              seriesItem.event_limit = Math.max(1, Math.round(next));
            }
          }
          if (field === "source-mode") {
            // Radio toggle: switch a line_chart series between metric and
            // event sourcing. Clear the inactive fields so the saved JSON
            // is always strictly one shape.
            if (value === "event") {
              if (!seriesItem.event_name_pattern) {
                seriesItem.event_name_pattern = "";
              }
              delete seriesItem.query;
              delete seriesItem.metric;
              delete seriesItem.measurement;
              delete seriesItem.field;
              delete seriesItem.aggregate;
              delete seriesItem.window;
            } else {
              delete seriesItem.event_name_pattern;
              delete seriesItem.event_limit;
            }
            renderWidgetEditor();
            syncChartSeries(widget);
            schedulePreview();
            markDirty();
            return;
          }
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

    // event_overlays widget-level handlers. addOverlayBtn is only present
    // when the current widget renders the overlays section (line_chart
    // only — aggregate_band is excluded). The per-overlay field changes
    // mutate widget.event_overlays in place; removing the last overlay
    // drops the field entirely so saved JSON stays minimal.
    const addOverlayBtn = widgetEditorEl.querySelector("#addOverlayBtn");
    if (addOverlayBtn) {
      addOverlayBtn.addEventListener("click", () => {
        if (!Array.isArray(widget.event_overlays)) {
          widget.event_overlays = [];
        }
        widget.event_overlays.push({ event_name_pattern: "" });
        renderWidgetEditor();
        schedulePreview();
        markDirty();
      });
    }
    widgetEditorEl.querySelectorAll(".overlay-card").forEach((card) => {
      const overlayIdx = Number(card.dataset.overlayIndex);
      const overlay = (widget.event_overlays || [])[overlayIdx];
      if (!overlay) {
        return;
      }
      const removeBtn = card.querySelector('[data-overlay-action="remove"]');
      if (removeBtn) {
        removeBtn.addEventListener("click", () => {
          widget.event_overlays.splice(overlayIdx, 1);
          if (widget.event_overlays.length === 0) {
            delete widget.event_overlays;
          }
          renderWidgetEditor();
          schedulePreview();
          markDirty();
        });
      }
      card.querySelectorAll("[data-overlay-field]").forEach((fieldEl) => {
        fieldEl.addEventListener("change", async (event) => {
          const field = event.target.dataset.overlayField;
          const value = event.target.value;
          if (field === "event_name_pattern") {
            overlay.event_name_pattern = value;
            schedulePreviewFetch(overlay, overlayIdx);
          } else if (field === "db") {
            delete overlay.database;
            if (value) {
              overlay.db = value;
              await loadMetricsForDB(value);
              await loadEventsForDB(value);
              populateMetricDatalist();
              populateEventDatalist();
              // Redraw so the db dropdown reflects the new selection
              // and the inline preview re-fetches under the new db.
              renderWidgetEditor();
              schedulePreviewFetch(overlay, overlayIdx);
            } else {
              delete overlay.db;
              renderWidgetEditor();
            }
          } else if (field === "label") {
            if (value) overlay.label = value; else delete overlay.label;
          } else if (field === "color") {
            if (value) overlay.color = value; else delete overlay.color;
          } else if (field === "event_limit") {
            const next = optionalNumber(value);
            if (next == null || next <= 0) {
              delete overlay.event_limit;
            } else {
              overlay.event_limit = Math.max(1, Math.round(next));
            }
          }
          schedulePreview();
          markDirty();
        });
      });
      // Live updates for the event_name_pattern text input: keep the
      // preview, draft, and dashboard in sync on every keystroke so
      // operators see recent matches without losing focus on blur.
      const patternEl = card.querySelector('[data-overlay-field="event_name_pattern"]');
      if (patternEl) {
        patternEl.addEventListener("input", () => {
          overlay.event_name_pattern = patternEl.value;
          schedulePreviewFetch(overlay, overlayIdx);
          schedulePreview();
          markDirty();
        });
      }
      // No-op kept so removing the old picker doesn't break call sites.
      attachOverlayPickerHandlers(card, overlay, overlayIdx);
      // Kick a preview fetch so the recent-matches list populates on
      // first render without a click.
      schedulePreviewFetch(overlay, overlayIdx);
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
    formatTransformedValue,
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
      if (item && item.role === "event") {
        return {
          stroke: item.color || pickSeriesColor(idx),
          width: 0,
          dash: [],
          points: { show: true, size: 8, width: 2 },
        };
      }
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

  function renderUPlotChart(plotEl, widget, seriesItems, overlayLayers) {
    if (typeof uPlot !== "function") {
      throw new Error("uPlot not loaded");
    }
    const presentation = widgetChartType(widget);
    const items = orderChartSeriesItems(Array.isArray(seriesItems) ? seriesItems : [], presentation);
    const overlays = Array.isArray(overlayLayers) ? overlayLayers : [];
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
          values: (u, vals) => vals.map((value) => (value == null ? "" : formatTransformedValue(axisTarget, value))),
        },
      ],
      legend: { show: true, live: true, isolate: false },
    };
    if (presentation === "aggregate_band") {
      Object.assign(opts, aggregateBandHooks(items));
    }
    if (presentation === "line_chart" && overlays.length > 0) {
      opts.hooks = mergeUPlotHooks(opts.hooks, eventOverlayHooks(overlays));
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

  // Overlay rendering helpers live in common_assets/dashboard_utils.js
  // so the dashboard, editor, and explorer all share one path.
  const mergeUPlotHooks = NANOTDB_UTILS.mergeUPlotHooks;
  const eventOverlayHooks = NANOTDB_UTILS.eventOverlayHooks;
  const isValidCssColorBasic = NANOTDB_UTILS.isValidCssColorBasic;
  const overlayDefaultColor = NANOTDB_UTILS.overlayDefaultColor;

  // fetchEventSeriesPoints / fetchEventOverlayMarkers: editor-side mirror
  // of dashboard_app.js's helpers, used by the preview pane so the
  // editor renders event-backed series + overlays the same way the live
  // dashboard does.
  async function fetchEventSeriesPoints(db, series, lookbackSec) {
    const pattern = series && (series.event_name_pattern || "").trim();
    if (!db || !pattern) {
      return [];
    }
    const end = new Date();
    const start = new Date(end.getTime() - lookbackSec * 1000);
    const limit = (series && series.event_limit) ? Math.max(1, Math.min(1000, Number(series.event_limit) || 1000)) : 1000;
    const eventsURL = apiURL(
      "/api/v1/events?db=" + encodeURIComponent(db) +
      "&name=" + encodeURIComponent(pattern) +
      "&start=" + encodeURIComponent(start.toISOString()) +
      "&end=" + encodeURIComponent(end.toISOString()) +
      "&limit=" + limit
    );
    const payload = await fetchJSON(eventsURL);
    const events = (payload.data && payload.data.result) ? payload.data.result : [];
    const out = [];
    for (const evt of events) {
      const ts = Number(evt && evt.ts);
      if (!Number.isFinite(ts)) {
        continue;
      }
      let raw;
      if (typeof evt.int32 === "number") {
        raw = evt.int32;
      } else if (typeof evt.float32 === "number") {
        raw = evt.float32;
      } else {
        continue;
      }
      const y = transformValue(series, raw);
      if (!Number.isFinite(y)) {
        continue;
      }
      out.push({ x: ts / 1e9, y });
    }
    return out;
  }

  async function fetchEventOverlayMarkers(db, overlay, lookbackSec) {
    return NANOTDB_UTILS.fetchEventOverlayMarkers(
      (typeof cfg !== "undefined" && cfg && cfg.apiBaseURL) || "",
      db,
      overlay,
      lookbackSec
    );
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
          const isEventBacked = !!(series && (series.event_name_pattern || "").trim());
          if (isEventBacked) {
            const points = await fetchEventSeriesPoints(db, series, lookbackSec);
            fetchedItems[idx] = {
              label: effectiveSeriesLabel(series, idx),
              role: "event",
              points,
            };
            return;
          }
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
      // Pull event_overlays in parallel so the preview matches what
      // dashboard_app.js will render for the same dashboard JSON.
      let overlayLayers = [];
      if (widgetChartType(widget) === "line_chart" && Array.isArray(widget.event_overlays) && widget.event_overlays.length > 0) {
        overlayLayers = await Promise.all(widget.event_overlays.map(async (overlay) => {
          const overlayDB = (overlay && (overlay.db || overlay.database) || "").trim() || (dashboardCfg && dashboardCfg.default_db) || "";
          const markers = await fetchEventOverlayMarkers(overlayDB, overlay, lookbackSec);
          return {
            label: (overlay && overlay.label) || (overlay && overlay.event_name_pattern) || "",
            color: (overlay && overlay.color) || "",
            markers,
          };
        }));
      }
      const hasData = renderUPlotChart(plot, widget, seriesItems.filter(Boolean), overlayLayers);
      foot.textContent = widgetChartType(widget) === "aggregate_band" ? "" : (hasData ? "preview updated " + new Date().toLocaleTimeString() : "no points");
      return;
    }

    if (widget.type === "event_log") {
      const card = document.createElement("article");
      card.className = "widget-eventlog";
      const eventsEl = document.createElement("div");
      eventsEl.className = "eventlog-rows";
      const foot = document.createElement("p");
      foot.className = "widget-foot";
      foot.textContent = "waiting for data";
      card.appendChild(createPreviewHeader(widget));
      card.appendChild(eventsEl);
      card.appendChild(foot);
      containerEl.appendChild(card);

      // Match the dashboard widget's multi-series shape: every
      // series with a non-empty event_name_pattern contributes to one
      // merged feed. Error messages distinguish "no pattern set
      // anywhere" from "pattern set but db is empty" so the operator
      // knows which field to fix.
      const seriesAll = widget.series || [];
      const seriesWithPattern = seriesAll.filter((s) => s && (s.event_name_pattern || "").trim() !== "");
      if (seriesWithPattern.length === 0) {
        foot.textContent = "missing event_name_pattern on every series";
        return;
      }
      const seriesWithDB = seriesWithPattern.filter((s) => seriesDB(s, dashboardCfg));
      if (seriesWithDB.length === 0) {
        foot.textContent = "missing db (set per-series or default_db on the dashboard)";
        return;
      }
      const lookbackSec = parseDurationSeconds(widget.lookback || "1h", 3600);
      const end = new Date();
      const start = new Date(end.getTime() - lookbackSec * 1000);
      const fetched = await Promise.all(seriesWithDB.map(async (s) => {
        const db = seriesDB(s, dashboardCfg);
        const pattern = (s.event_name_pattern || "").trim();
        const limit = s.event_limit ? s.event_limit : 10;
        const url = apiURL(`/api/v1/events?db=${encodeURIComponent(db)}&name=${encodeURIComponent(pattern)}&start=${encodeURIComponent(start.toISOString())}&end=${encodeURIComponent(end.toISOString())}&limit=${limit}`);
        try {
          const payload = await fetchJSON(url);
          return (payload.data && payload.data.result) ? payload.data.result : [];
        } catch (err) {
          return [];
        }
      }));
      const merged = [];
      fetched.forEach((evts) => evts.forEach((e) => merged.push(e)));
      merged.sort((a, b) => (b.ts || b.T || 0) - (a.ts || a.T || 0));
      const totalCap = seriesWithDB.reduce((sum, s) => sum + (s.event_limit ? s.event_limit : 10), 0);
      const events = merged.slice(0, totalCap);
      if (events.length === 0) {
        foot.textContent = "no events";
        return;
      }
      events.forEach((evt) => {
        const row = document.createElement("div");
        row.className = "eventlog-row";
        const nameCell = document.createElement("span");
        nameCell.className = "eventlog-cell eventlog-name";
        nameCell.textContent = evt.name || evt.N || "?";
        const timeCell = document.createElement("span");
        timeCell.className = "eventlog-cell eventlog-time";
        const tsNs = evt.ts || evt.T;
        timeCell.textContent = Number.isInteger(tsNs) ? new Date(Math.floor(tsNs / 1000000)).toLocaleTimeString() : "--";
        row.appendChild(nameCell);
        row.appendChild(timeCell);
        eventsEl.appendChild(row);
      });
      foot.textContent = "preview updated " + new Date().toLocaleTimeString();
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

  // flushEditorInputsToState walks every text input currently bound to a
  // series or overlay field and copies its DOM value back into
  // state.draftConfig. The per-field input/change listeners normally do
  // this on every keystroke, but if a save is triggered before the
  // listener fires (or never fires — e.g. some browsers when picking a
  // datalist option) the JSON we send would be stale. Calling this
  // immediately before validate/save guarantees the request reflects
  // what the user sees in the editor.
  function flushEditorInputsToState() {
    const widget = selectedWidget();
    if (!widget) return;
    if (Array.isArray(widget.event_overlays)) {
      widgetEditorEl.querySelectorAll(".overlay-card").forEach((card) => {
        const overlayIdx = Number(card.dataset.overlayIndex);
        const overlay = widget.event_overlays[overlayIdx];
        if (!overlay) return;
        const patternEl = card.querySelector('[data-overlay-field="event_name_pattern"]');
        if (patternEl) {
          overlay.event_name_pattern = patternEl.value;
        }
      });
    }
  }

  async function validateDraft() {
    flushEditorInputsToState();
    await fetchJSON(apiURL("/api/dashboard-config/validate"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(state.draftConfig),
    });
  }

  async function saveDraft() {
    flushEditorInputsToState();
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
      // Focus the new group's label input so the user can rename it
      // immediately. Group IDs are auto-generated and hidden.
      const groupLabelInput = groupsListEl.querySelector('details[data-group-id="' + id + '"] [data-action="group-label"]');
      if (groupLabelInput) {
        groupLabelInput.focus();
        groupLabelInput.select();
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
      // Focus the new widget's Title input so the user can rename it
      // immediately. Widget IDs are auto-generated and hidden.
      const widgetTitleInput = widgetEditorEl.querySelector("#widgetTitleInput");
      if (widgetTitleInput) {
        widgetTitleInput.focus();
        widgetTitleInput.select();
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