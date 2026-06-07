(function () {
  const cfg = window.NANOTDB_DASH_CONFIG || { basePath: "/dashboard", refreshSeconds: 10, apiBaseURL: "" };
  const {
    buildInstantQueryPath,
    buildRangeQueryPath,
    seriesUsesAggregateRange,
    formatNumericValue,
  } = window.NANOTDB_UTILS || {};

  function formatAxisValue(value) {
    if (value == null || !Number.isFinite(Number(value))) {
      return "";
    }
    return typeof formatNumericValue === "function" ? formatNumericValue(value, 2) : Number(value).toFixed(2);
  }

  function apiURL(path) {
    const base = typeof cfg.apiBaseURL === "string" ? cfg.apiBaseURL.replace(/\/$/, "") : "";
    return base + path;
  }

  const dbSelect = document.getElementById("dbSelect");
  const queryInput = document.getElementById("queryInput");
  const metricsOptions = document.getElementById("metricsOptions");
  const addQueryBtn = document.getElementById("addQueryBtn");
  const aggregateInput = document.getElementById("aggregateInput");
  const aggregateOptions = document.getElementById("aggregateOptions");
  const bucketWindowInput = document.getElementById("bucketWindowInput");
  const selectedQueriesEl = document.getElementById("selectedQueries");
  const windowSelect = document.getElementById("windowSelect");
  const stepSelect = document.getElementById("stepSelect");
  const autoRefreshBtn = document.getElementById("autoRefreshBtn");
  const refreshIntervalSelect = document.getElementById("refreshIntervalSelect");
  const refreshBtn = document.getElementById("refreshBtn");
  const statusEl = document.getElementById("status");
  const cards = document.getElementById("cards");
  const chartEl = document.getElementById("chart");

  const palette = ["#2dd4a4", "#46a3ff", "#f59e0b", "#f472b6", "#a78bfa", "#22d3ee"];

  const css = getComputedStyle(document.documentElement);
  const chartGrid = css.getPropertyValue("--chart-grid").trim() || "#4c5a73";
  const chartText = css.getPropertyValue("--chart-text").trim() || "#b9c6d9";
  let metricCatalog = [];
  let selectedQueries = [];
  let chartInstance = null;
  let lastSeriesByQuery = {};
  let lastQueryOrder = [];
  let autoRefreshEnabled = true;
  let autoRefreshTimer = null;
  let refreshInFlight = false;
  let aggregateCatalog = { result: [], default: "avg" };

  const initialRefreshSeconds = Math.max(2, Number(cfg.refreshSeconds || 10));

  function setStatus(msg) {
    if (!statusEl) {
      return;
    }
    statusEl.textContent = msg || "";
    statusEl.classList.toggle("is-visible", Boolean(msg));
  }

  function syncRefreshControls(updateStatus) {
    autoRefreshBtn.textContent = autoRefreshEnabled ? "⏸" : "▶";
    autoRefreshBtn.title = autoRefreshEnabled ? "Pause auto refresh" : "Resume auto refresh";
    autoRefreshBtn.setAttribute("aria-label", autoRefreshEnabled ? "Pause auto refresh" : "Resume auto refresh");
    autoRefreshBtn.setAttribute("aria-pressed", autoRefreshEnabled ? "false" : "true");
    refreshBtn.disabled = refreshInFlight;
    if (updateStatus !== false) {
      setStatus("");
    }
  }

  function restartAutoRefreshTimer() {
    if (autoRefreshTimer) {
      clearInterval(autoRefreshTimer);
      autoRefreshTimer = null;
    }
    if (!autoRefreshEnabled) {
      return;
    }
    const seconds = Math.max(2, Number(refreshIntervalSelect.value || initialRefreshSeconds));
    autoRefreshTimer = setInterval(() => {
      refreshAll().catch((err) => {
        setStatus("Refresh failed: " + err.message);
      });
    }, seconds * 1000);
  }

  async function fetchJSON(url) {
    const res = await fetch(url, { cache: "no-store" });
    if (!res.ok) {
      throw new Error("HTTP " + res.status + " for " + url);
    }
    return res.json();
  }

  function renderAggregateOptions() {
    if (!aggregateOptions) {
      return;
    }
    aggregateOptions.innerHTML = "";
    (aggregateCatalog.result || []).forEach((name) => {
      const opt = document.createElement("option");
      opt.value = name;
      aggregateOptions.appendChild(opt);
    });
    if (aggregateInput && !aggregateInput.value && aggregateCatalog.default) {
      aggregateInput.placeholder = "optional, e.g. " + aggregateCatalog.default;
    }
    if (stepSelect && aggregateCatalog.default) {
      stepSelect.title = "Blank aggregate uses server default " + aggregateCatalog.default + " buckets";
    }
  }

  async function loadAggregates() {
    const payload = await fetchJSON(apiURL("/api/v1/aggregates"));
    const data = payload && payload.data ? payload.data : {};
    aggregateCatalog = {
      result: Array.isArray(data.result) ? data.result.slice() : [],
      default: typeof data.default === "string" && data.default.trim() ? data.default.trim() : "avg",
    };
    renderAggregateOptions();
  }

  function currentAggregate() {
    return (aggregateInput.value || "").trim();
  }

  function currentBucketWindow() {
    return (bucketWindowInput.value || "").trim();
  }

  function currentAggregateModeValid() {
    const aggregate = currentAggregate();
    const windowValue = currentBucketWindow();
    return (!aggregate && !windowValue) || (aggregate && windowValue);
  }

  function activeQueryItem(item) {
    return {
      query: item && item.query ? item.query : "",
      aggregate: currentAggregate(),
      window: currentBucketWindow(),
    };
  }

  function queryItemLabel(item) {
    if (!item) {
      return "";
    }
    const query = item.query || "";
    const aggregate = item.aggregate || "";
    const windowValue = item.window || "";
    if (aggregate && windowValue) {
      return query + " [" + aggregate + " " + windowValue + "]";
    }
    return query;
  }

  function selectedChipLabel(item) {
    return item && item.query ? item.query : "";
  }

  function selectedQueryItems() {
    return selectedQueries.slice();
  }

  function renderSelectedQueries() {
    selectedQueriesEl.innerHTML = "";
    if (!selectedQueries.length) {
      const empty = document.createElement("span");
      empty.className = "selected-empty";
      empty.textContent = "No queries selected";
      selectedQueriesEl.appendChild(empty);
      return;
    }

    selectedQueries.forEach((item) => {
      const label = selectedChipLabel(item);
      const chip = document.createElement("span");
      chip.className = "metric-chip";

      const text = document.createElement("span");
      text.textContent = label;

      const removeBtn = document.createElement("button");
      removeBtn.type = "button";
      removeBtn.className = "chip-remove";
      removeBtn.setAttribute("aria-label", "Remove " + label);
      removeBtn.textContent = "x";
      removeBtn.addEventListener("click", async () => {
        selectedQueries = selectedQueries.filter((queryItem) => selectedChipLabel(queryItem) !== label);
        renderSelectedQueries();
        await refreshAll();
      });

      chip.appendChild(text);
      chip.appendChild(removeBtn);
      selectedQueriesEl.appendChild(chip);
    });
  }

  async function addQuery(rawQuery) {
    const query = (rawQuery || "").trim();
    if (!query) {
      return;
    }
    if (!currentAggregateModeValid()) {
      setStatus("Aggregate and bucket window must be set together");
      return;
    }
    const next = { query };
    if (selectedQueries.some((item) => selectedChipLabel(item) === selectedChipLabel(next))) {
      queryInput.value = "";
      return;
    }
    selectedQueries.push(next);
    queryInput.value = "";
    renderSelectedQueries();
    await refreshAll();
  }

  async function loadDatabases() {
    const payload = await fetchJSON(apiURL("/api/v1/databases"));
    const items = (payload.data && payload.data.result) || [];
    dbSelect.innerHTML = "";
    items.forEach((name) => {
      const opt = document.createElement("option");
      opt.value = name;
      opt.textContent = name;
      dbSelect.appendChild(opt);
    });
    if (items.length === 0) {
      setStatus("No databases yet. Push some samples first.");
      return;
    }
    setStatus("Loaded databases");
    await loadMetrics();
  }

  async function loadMetrics() {
    const db = dbSelect.value;
    if (!db) {
      return;
    }
    const payload = await fetchJSON(apiURL("/api/v1/metrics?db=" + encodeURIComponent(db)));
    const items = (payload.data && payload.data.result) || [];
    metricCatalog = items.slice();
    metricsOptions.innerHTML = "";
    items.forEach((metric) => {
      const opt = document.createElement("option");
      opt.value = metric;
      metricsOptions.appendChild(opt);
    });

    selectedQueries = selectedQueries.filter((item) => item && item.query);
    if (selectedQueries.length === 0) {
      selectedQueries = metricCatalog.slice(0, 3).map((metric) => ({ query: metric }));
    }
    renderSelectedQueries();
  }

  async function renderLastValues(db, queries, fromIso, toIso, step) {
    cards.innerHTML = "";
    const jobs = queries.map(async (item) => {
      const card = document.createElement("div");
      card.className = "card";
      const activeItem = activeQueryItem(item);
      const label = queryItemLabel(activeItem);
      let result = null;
      if (seriesUsesAggregateRange && seriesUsesAggregateRange(activeItem)) {
        const points = await loadSeries(db, activeItem, fromIso, toIso, step);
        const lastPoint = points[points.length - 1];
        if (lastPoint) {
          result = { value: [lastPoint.x, lastPoint.y] };
        }
      } else {
        const instantPath = buildInstantQueryPath(item.db || db, activeItem);
        if (instantPath) {
          const data = await fetchJSON(apiURL(instantPath));
          result = data.data && data.data.result && data.data.result[0];
        }
      }
      if (!result) {
        card.innerHTML =
          '<div class="metric">' + label + '</div><div class="value">-</div><div class="ts">no data</div>';
        return card;
      }
      const ts = Number(result.value[0]) * 1000;
      card.innerHTML =
        '<div class="metric">' + label + "</div>" +
        '<div class="value">' + result.value[1] + "</div>" +
        '<div class="ts">' + new Date(ts).toLocaleString() + "</div>";
      return card;
    });
    const renderedCards = await Promise.all(jobs);
    renderedCards.forEach((card) => cards.appendChild(card));
  }

  async function loadSeries(db, item, fromIso, toIso, step) {
    const path = buildRangeQueryPath(item.db || db, item, fromIso, toIso, step);
    if (!path) {
      return [];
    }
    const payload = await fetchJSON(apiURL(path));
    const result = payload.data && payload.data.result && payload.data.result[0];
    if (!result || !result.values) {
      return [];
    }
    return result.values.map((point) => ({ x: Number(point[0]), y: Number(point[1]) }));
  }

  function destroyChart() {
    if (chartInstance) {
      chartInstance.destroy();
      chartInstance = null;
    }
  }

  function buildUPlotData(seriesByQuery, queryOrder) {
    const timeSet = new Set();
    queryOrder.forEach((label) => {
      const points = seriesByQuery[label] || [];
      points.forEach((point) => timeSet.add(point.x));
    });

    const x = Array.from(timeSet).sort((a, b) => a - b);
    const data = [x];
    queryOrder.forEach((label) => {
      const byTs = new Map((seriesByQuery[label] || []).map((point) => [point.x, point.y]));
      data.push(x.map((ts) => (byTs.has(ts) ? byTs.get(ts) : null)));
    });
    return data;
  }

  // ---------------------------------------------------------------------
  // Event overlays. State is a flat array of {db, event_name_pattern,
  // label, color, event_limit}, kept in sync with the URL so links
  // are shareable. The render path reuses the same eventOverlayHooks
  // + fetchEventOverlayMarkers code the dashboard widgets use.
  // ---------------------------------------------------------------------
  const overlaysSectionEl = document.getElementById("overlaysSection");
  const overlaysListEl = document.getElementById("overlaysList");
  const overlaysSummaryCountEl = document.getElementById("overlaysSummaryCount");
  const addExplorerOverlayBtn = document.getElementById("addExplorerOverlayBtn");

  let overlays = readOverlaysFromURL();
  let internalEventsCatalog = null;
  const overlayPreviewTimers = new Map();

  function readOverlaysFromURL() {
    try {
      const params = new URLSearchParams(window.location.search);
      const raw = params.get("overlays");
      if (!raw) return [];
      const parsed = JSON.parse(raw);
      if (!Array.isArray(parsed)) return [];
      return parsed
        .filter((o) => o && typeof o === "object" && (o.event_name_pattern || "").trim())
        .map((o) => ({
          db: typeof o.db === "string" ? o.db : "",
          event_name_pattern: String(o.event_name_pattern || ""),
          label: typeof o.label === "string" ? o.label : "",
          color: typeof o.color === "string" ? o.color : "",
          event_limit: Number(o.event_limit) > 0 ? Number(o.event_limit) : 200,
        }));
    } catch (err) {
      return [];
    }
  }

  function writeOverlaysToURL() {
    const params = new URLSearchParams(window.location.search);
    const trimmed = overlays
      .filter((o) => o && (o.event_name_pattern || "").trim())
      .map((o) => {
        const out = { event_name_pattern: o.event_name_pattern };
        if (o.db) out.db = o.db;
        if (o.label) out.label = o.label;
        if (o.color) out.color = o.color;
        if (o.event_limit && o.event_limit !== 200) out.event_limit = o.event_limit;
        return out;
      });
    if (trimmed.length === 0) {
      params.delete("overlays");
    } else {
      params.set("overlays", JSON.stringify(trimmed));
    }
    const next = window.location.pathname + (params.toString() ? "?" + params.toString() : "") + window.location.hash;
    window.history.replaceState({}, "", next);
  }

  async function ensureInternalEventsCatalog() {
    if (internalEventsCatalog !== null) return;
    try {
      const res = await fetch(apiURL("/api/v1/internal-events/catalog"), { cache: "no-store" });
      if (!res.ok) {
        internalEventsCatalog = [];
        return;
      }
      const body = await res.json();
      const groups = (body.data && body.data.groups) || [];
      internalEventsCatalog = groups.map((g) => ({
        name: g.name || "",
        events: (g.events || []).filter((e) => e && e.name).map((e) => ({
          name: e.name,
          description: e.description || "",
        })),
      })).filter((g) => g.events.length > 0);
    } catch (err) {
      internalEventsCatalog = [];
    }
  }

  async function ensureEventCatalogForDB(db) {
    if (!db) return [];
    if (db === "internal") {
      await ensureInternalEventsCatalog();
      // Flatten the grouped catalog to a list when the picker is in
      // a non-internal scope.
      const list = [];
      (internalEventsCatalog || []).forEach((g) => g.events.forEach((e) => list.push(e.name)));
      return list;
    }
    try {
      const res = await fetch(apiURL("/api/v1/events/catalog?db=" + encodeURIComponent(db)), { cache: "no-store" });
      if (!res.ok) return [];
      const body = await res.json();
      return ((body.data && body.data.result) || []).map((it) => it.name).filter(Boolean);
    } catch (err) {
      return [];
    }
  }

  function explorerOverlayCandidates(db) {
    if (db === "internal" && internalEventsCatalog) {
      const out = [];
      internalEventsCatalog.forEach((g) => {
        if (g.events.length === 0) return;
        out.push({ groupHeader: g.name });
        g.events.forEach((e) => out.push({ name: e.name, description: e.description }));
      });
      return out;
    }
    // Use a per-explorer-render cache keyed by db.
    const cached = explorerOverlayCandidates.cache.get(db) || [];
    return cached.map((n) => ({ name: n }));
  }
  explorerOverlayCandidates.cache = new Map();

  // populateExplorerEventDatalist refreshes the shared #eventOptions
  // datalist with every event name we currently know about across
  // all explored databases (plus the internal-events catalog). Used
  // as the autocomplete source for the overlay event input. Mirrors
  // populateEventDatalist() in the dashboard editor.
  const eventOptionsList = document.getElementById("eventOptions");
  function populateExplorerEventDatalist() {
    if (!eventOptionsList) return;
    const all = new Set();
    explorerOverlayCandidates.cache.forEach((names) => {
      (names || []).forEach((n) => all.add(n));
    });
    (internalEventsCatalog || []).forEach((g) => {
      (g.events || []).forEach((e) => { if (e && e.name) all.add(e.name); });
    });
    const values = Array.from(all).sort();
    eventOptionsList.innerHTML = values.map((n) => '<option value="' + escapeBasic(n) + '"></option>').join("");
  }

  async function preloadOverlayCandidates() {
    const dbs = new Set();
    overlays.forEach((o) => { if (o.db) dbs.add(o.db); });
    if (dbSelect.value) dbs.add(dbSelect.value);
    for (const db of dbs) {
      if (db === "internal") {
        await ensureInternalEventsCatalog();
      } else if (!explorerOverlayCandidates.cache.has(db)) {
        const names = await ensureEventCatalogForDB(db);
        explorerOverlayCandidates.cache.set(db, names);
      }
    }
    populateExplorerEventDatalist();
  }

  function dbOptionsHTMLExplorer(selected) {
    const opts = ['<option value="">(use current chart db)</option>'];
    Array.from(explorerOverlayCandidates.cache.keys()).concat(["internal"]).filter((v, i, arr) => arr.indexOf(v) === i).sort().forEach((db) => {
      opts.push('<option value="' + escapeBasic(db) + '"' + (db === selected ? ' selected' : '') + '>' + escapeBasic(db) + '</option>');
    });
    return opts.join("");
  }

  function escapeBasic(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function renderOverlayList() {
    if (!overlaysListEl) return;
    if (overlays.length === 0) {
      overlaysListEl.innerHTML = '<p class="overlays-empty">No overlays. Add one to drop vertical markers on the chart at matching event timestamps.</p>';
      updateOverlaySummary();
      return;
    }
    overlaysListEl.innerHTML = overlays.map((ov, oi) => renderOverlayCardHTML(ov, oi)).join("");
    // Listeners are attached once via overlay delegation below (see
    // attachOverlayDelegation), so we only need to schedule the per-
    // overlay preview fetch after each render.
    overlays.forEach((ov, oi) => schedulePreviewFetch(ov, oi));
    updateOverlaySummary();
  }

  function updateOverlaySummary() {
    if (!overlaysSummaryCountEl) return;
    overlaysSummaryCountEl.textContent = overlays.length === 0 ? "" :
      "(" + overlays.length + " " + (overlays.length === 1 ? "layer" : "layers") + ")";
  }

  function renderOverlayCardHTML(overlay, oi) {
    const db = overlay.db || "";
    const currentValue = overlay.event_name_pattern || "";
    const eventControl =
      '<label class="overlay-event-control">Event' +
        '<input type="text" list="eventOptions" data-overlay-field="event_name_pattern" placeholder="e.g. nanotdb.mqtt.connected or disk.*" value="' + escapeBasic(currentValue) + '" />' +
      '</label>';
    return (
      '<div class="overlay-card" data-overlay-index="' + oi + '">' +
        '<div class="overlay-grid">' +
          '<label>Database<select data-overlay-field="db">' + dbOptionsHTMLExplorer(db) + '</select></label>' +
          eventControl +
          '<label>Label<input type="text" data-overlay-field="label" placeholder="(optional)" value="' + escapeBasic(overlay.label || "") + '" /></label>' +
          '<label>Color<input type="text" data-overlay-field="color" placeholder="e.g. #c00 or red" value="' + escapeBasic(overlay.color || "") + '" /></label>' +
          '<label>Limit<input type="number" min="1" step="1" data-overlay-field="event_limit" value="' + (overlay.event_limit || 200) + '" /></label>' +
          '<button type="button" class="secondary-btn" data-overlay-action="remove" title="Remove">✕</button>' +
        '</div>' +
        '<div class="overlay-preview" data-overlay-preview="' + oi + '">' +
          '<div class="overlay-preview-head">Recent matches</div>' +
          '<div class="overlay-preview-body" data-overlay-preview-body="' + oi + '">' +
            '<div class="overlay-preview-empty">Pick an event to preview recent matches.</div>' +
          '</div>' +
        '</div>' +
      '</div>'
    );
  }

  // overlayIndexFromEl walks up from a click target to the enclosing
  // .overlay-card and returns its data-overlay-index as a number, or
  // -1 when the click landed outside any card. Used by the delegated
  // click/input/change handlers below so listeners survive innerHTML
  // re-renders.
  function overlayIndexFromEl(el) {
    const card = el && el.closest ? el.closest('.overlay-card') : null;
    if (!card) return -1;
    const oi = Number(card.dataset.overlayIndex);
    return Number.isFinite(oi) ? oi : -1;
  }

  function attachOverlayDelegation() {
    if (!overlaysListEl) return;
    overlaysListEl.addEventListener("click", (event) => {
      const target = event.target;
      if (!target || !target.closest) return;

      // Remove-overlay button.
      const remove = target.closest('[data-overlay-action="remove"]');
      if (remove && overlaysListEl.contains(remove)) {
        const oi = overlayIndexFromEl(remove);
        if (oi < 0) return;
        overlays.splice(oi, 1);
        writeOverlaysToURL();
        renderOverlayList();
        scheduleOverlayRefresh();
        return;
      }
    });

    // Input — live updates to the event pattern text field. Driven
    // by the native datalist autocomplete; both selecting a
    // suggestion and free-form typing fire "input" on the same
    // control.
    overlaysListEl.addEventListener("input", (event) => {
      const target = event.target;
      if (!target || !target.dataset) return;
      const oi = overlayIndexFromEl(target);
      if (oi < 0) return;
      const overlay = overlays[oi];
      if (!overlay) return;
      if (target.dataset.overlayField === "event_name_pattern") {
        overlay.event_name_pattern = target.value;
        writeOverlaysToURL();
        schedulePreviewFetch(overlay, oi);
        scheduleOverlayRefresh();
      }
    });

    // Change — committed values for the db/label/color/limit fields.
    overlaysListEl.addEventListener("change", async (event) => {
      const target = event.target;
      if (!target || !target.dataset || target.dataset.overlayField == null) return;
      const oi = overlayIndexFromEl(target);
      if (oi < 0) return;
      const overlay = overlays[oi];
      if (!overlay) return;
      const field = target.dataset.overlayField;
      const value = target.value;
      if (field === "event_name_pattern") {
        overlay.event_name_pattern = value;
        schedulePreviewFetch(overlay, oi);
      } else if (field === "db") {
        overlay.db = value;
        if (value && value !== "internal" && !explorerOverlayCandidates.cache.has(value)) {
          const names = await ensureEventCatalogForDB(value);
          explorerOverlayCandidates.cache.set(value, names);
        } else if (value === "internal") {
          await ensureInternalEventsCatalog();
        }
        populateExplorerEventDatalist();
        renderOverlayList();
        schedulePreviewFetch(overlay, oi);
      } else if (field === "label") {
        overlay.label = value;
      } else if (field === "color") {
        overlay.color = value;
      } else if (field === "event_limit") {
        overlay.event_limit = Math.max(1, Math.min(1000, Number(value) || 200));
      }
      writeOverlaysToURL();
      scheduleOverlayRefresh();
    });
  }

  function schedulePreviewFetch(overlay, oi) {
    const prev = overlayPreviewTimers.get(oi);
    if (prev != null) window.clearTimeout(prev);
    const timer = window.setTimeout(() => {
      void renderOverlayPreview(overlay, oi);
    }, 300);
    overlayPreviewTimers.set(oi, timer);
  }

  async function renderOverlayPreview(overlay, oi) {
    const body = document.querySelector('[data-overlay-preview-body="' + oi + '"]');
    if (!body) return;
    const db = overlay.db || dbSelect.value || "";
    const pattern = (overlay.event_name_pattern || "").trim();
    if (!db || !pattern) {
      body.innerHTML = '<div class="overlay-preview-empty">Pick an event to preview recent matches.</div>';
      return;
    }
    body.innerHTML = '<div class="overlay-preview-empty">Loading…</div>';
    try {
      const end = new Date();
      const start = new Date(end.getTime() - 60 * 60 * 1000);
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
      body.innerHTML = events.map((evt) => {
        const t = Number(evt && evt.ts);
        const when = Number.isFinite(t) ? new Date(t / 1e6).toLocaleTimeString() : "?";
        const val = (typeof evt.int32 === "number" ? evt.int32 : (typeof evt.float32 === "number" ? evt.float32 : ""));
        const valText = val === "" ? "" : ' <span class="overlay-preview-value">' + escapeBasic(String(val)) + '</span>';
        return '<div class="overlay-preview-row">' +
          '<span class="overlay-preview-time">' + escapeBasic(when) + '</span>' +
          '<span class="overlay-preview-name codeish">' + escapeBasic(evt.name || "") + '</span>' +
          valText +
        '</div>';
      }).join("");
    } catch (err) {
      body.innerHTML = '<div class="overlay-preview-empty">Preview failed: ' + escapeBasic(err.message || String(err)) + '</div>';
    }
  }

  // Debounced trigger to re-fetch markers and redraw the chart when
  // overlays change.
  let overlayRefreshTimer = null;
  function scheduleOverlayRefresh() {
    if (overlayRefreshTimer != null) window.clearTimeout(overlayRefreshTimer);
    overlayRefreshTimer = window.setTimeout(() => {
      overlayRefreshTimer = null;
      void refreshOverlaysAndRedraw();
    }, 200);
  }

  let lastOverlayLayers = [];
  async function refreshOverlaysAndRedraw() {
    const chartDB = dbSelect.value;
    const lookbackSec = Math.max(60, Number(windowSelect.value || 3600));
    const layers = await Promise.all(overlays.map(async (ov) => {
      const db = ov.db || chartDB;
      const markers = await NANOTDB_UTILS.fetchEventOverlayMarkers(cfg.apiBaseURL || "", db, ov, lookbackSec).catch(() => []);
      return {
        label: ov.label || ov.event_name_pattern || "",
        color: ov.color || "",
        event_name_pattern: ov.event_name_pattern || "",
        markers,
      };
    }));
    lastOverlayLayers = layers;
    if (chartInstance) drawChart(lastSeriesByQuery, lastQueryOrder);
  }

  if (addExplorerOverlayBtn) {
    addExplorerOverlayBtn.addEventListener("click", () => {
      overlays.push({ db: "", event_name_pattern: "", label: "", color: "", event_limit: 200 });
      // Default to internal — that's the most common use case for
      // a new overlay (engine telemetry), and switching it to a
      // different db is one click. Operators who want a different
      // default can pre-fill via the URL.
      if (!overlays[overlays.length - 1].db) {
        overlays[overlays.length - 1].db = "internal";
      }
      writeOverlaysToURL();
      renderOverlayList();
      void preloadOverlayCandidates().then(renderOverlayList);
    });
  }

  // Attach the delegated click/input/change listeners once. The
  // overlays list is rebuilt via innerHTML on every state change, so
  // per-card addEventListener wiring would be lost the moment the
  // user clicks anything that triggers a re-render.
  attachOverlayDelegation();

  function drawChart(seriesByQuery, queryOrder) {
    lastSeriesByQuery = seriesByQuery;
    lastQueryOrder = Array.isArray(queryOrder) ? queryOrder.slice() : [];
    if (typeof uPlot !== "function") {
      throw new Error("uPlot not loaded");
    }

    const labels = lastQueryOrder.length ? lastQueryOrder.slice() : Object.keys(seriesByQuery);
    const data = buildUPlotData(seriesByQuery, labels);
    if (!data[0] || data[0].length === 0) {
      destroyChart();
      chartEl.innerHTML = '<div class="chart-empty">No data in selected range</div>';
      return;
    }

    chartEl.innerHTML = "";
    const series = [{ label: "Time" }];
    labels.forEach((label, idx) => {
      series.push({
        label,
        stroke: palette[idx % palette.length],
        width: 2,
        spanGaps: true,
        points: { show: false },
      });
    });

    const width = Math.max(chartEl.clientWidth || 0, 280);
    const height = Math.max(chartEl.clientHeight || 0, 320);
    const opts = {
      width,
      height,
      padding: window.matchMedia("(max-width: 699px)").matches ? [4, 4, 2, 2] : [8, 8, 4, 4],
      scales: { x: { time: true } },
      series,
      axes: [
        { stroke: chartText, grid: { stroke: chartGrid, width: 1 } },
        {
          stroke: chartText,
          grid: { stroke: chartGrid, width: 1 },
          values: (u, vals) => vals.map(formatAxisValue),
        },
      ],
      legend: { show: true, live: true, isolate: false },
    };

    // Drop the overlay layers in via the shared eventOverlayHooks
    // helper. Markers are pre-fetched by refreshOverlaysAndRedraw
    // (debounced after picker edits) and by refreshAll (on main
    // data refresh) so this hook only has to draw what's already
    // cached.
    if (Array.isArray(lastOverlayLayers) && lastOverlayLayers.length > 0) {
      opts.hooks = NANOTDB_UTILS.mergeUPlotHooks(opts.hooks, NANOTDB_UTILS.eventOverlayHooks(lastOverlayLayers));
    }

    destroyChart();
    chartInstance = new uPlot(opts, data, chartEl);
  }

  async function refreshAll() {
    if (refreshInFlight) {
      return;
    }
    const db = dbSelect.value;
    const queries = selectedQueryItems();
    if (!db || queries.length === 0) {
      cards.innerHTML = "";
      lastSeriesByQuery = {};
      lastQueryOrder = [];
      destroyChart();
      drawChart({}, []);
      syncRefreshControls(false);
      return;
    }

    refreshInFlight = true;
    syncRefreshControls(false);
    setStatus("Refreshing...");
    try {
      if (!currentAggregateModeValid()) {
        setStatus("Aggregate and bucket window must be set together");
        return;
      }
      const now = new Date();
      const windowSec = Number(windowSelect.value || "3600");
      const start = new Date(now.getTime() - windowSec * 1000);
      const fromIso = start.toISOString();
      const toIso = now.toISOString();
      const step = stepSelect.value || "30s";

      await renderLastValues(db, queries, fromIso, toIso, step);

      const activeQueries = queries.map((item) => activeQueryItem(item));
      const seriesResults = await Promise.all(activeQueries.map((item) => loadSeries(db, item, fromIso, toIso, step)));
      const seriesByQuery = {};
      activeQueries.forEach((item, idx) => {
        seriesByQuery[queryItemLabel(item)] = seriesResults[idx];
      });
      drawChart(seriesByQuery, activeQueries.map(queryItemLabel));
      // Refresh overlay markers alongside metric data so they share
      // the same lookback window. Don't block on overlay failures —
      // a broken overlay shouldn't tank the chart.
      void refreshOverlaysAndRedraw().catch(() => {});
      syncRefreshControls(true);
    } catch (err) {
      setStatus("Refresh failed: " + err.message);
      throw err;
    } finally {
      refreshInFlight = false;
      syncRefreshControls(false);
    }
  }

  dbSelect.addEventListener("change", async () => {
    await loadMetrics();
    await refreshAll();
  });
  addQueryBtn.addEventListener("click", async () => {
    await addQuery(queryInput.value);
  });
  queryInput.addEventListener("keydown", async (ev) => {
    if (ev.key === "Enter") {
      ev.preventDefault();
      await addQuery(queryInput.value);
    }
  });
  aggregateInput.addEventListener("change", refreshAll);
  bucketWindowInput.addEventListener("change", refreshAll);
  windowSelect.addEventListener("change", refreshAll);
  stepSelect.addEventListener("change", refreshAll);
  autoRefreshBtn.addEventListener("click", () => {
    autoRefreshEnabled = !autoRefreshEnabled;
    restartAutoRefreshTimer();
    syncRefreshControls(true);
  });
  refreshIntervalSelect.addEventListener("change", () => {
    restartAutoRefreshTimer();
    syncRefreshControls(true);
  });
  refreshBtn.addEventListener("click", refreshAll);
  window.addEventListener("resize", () => {
    if (chartInstance) {
      drawChart(lastSeriesByQuery, lastQueryOrder);
    }
  });

  async function init() {
    try {
      refreshIntervalSelect.value = String(initialRefreshSeconds);
      syncRefreshControls(true);
      await loadAggregates();
      await loadDatabases();
      // Preload overlay candidate lists so the picker has something
      // to show on first open. Auto-expands the accordion if URL
      // params already declare overlays so the operator sees them.
      if (overlays.length > 0 && overlaysSectionEl) {
        overlaysSectionEl.open = true;
      }
      await preloadOverlayCandidates();
      renderOverlayList();
      await refreshAll();
      restartAutoRefreshTimer();
    } catch (err) {
      setStatus("Failed to load dashboard: " + err.message);
    }
  }

  init();
})();
