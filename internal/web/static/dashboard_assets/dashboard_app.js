(function () {
  const cfg = JSON.parse(document.getElementById("dashboardConfig").textContent) || { basePath: "/dashboard", refreshSeconds: 10, apiBaseURL: "" };
  const chartState = new Map();
  const groupPanes = new Map();
  const groupTimers = new Map();
  const groupWidgetRefreshers = new Map();
  let activeGroupId = "";
  let dashboardRefreshPaused = false;

  function apiURL(path) {
    const base = typeof cfg.apiBaseURL === "string" ? cfg.apiBaseURL.replace(/\/$/, "") : "";
    return base + path;
  }

  function fetchJSON(url) {
    return fetch(url, { cache: "no-store" }).then((res) => {
      if (!res.ok) {
        throw new Error("HTTP " + res.status + " for " + url);
      }
      return res.json();
    });
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
      // Event-backed series render as scatter points (no connecting line):
      // events are sparse occurrences, not a continuous signal, so a line
      // would imply intermediate values that don't exist.
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
    // Overlays decorate a chart — they don't stand alone. If the metric
    // and event-backed series produced no points, drop the render even
    // if overlays have markers; there's no Y-axis to position them on.
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
    const isMobile = window.matchMedia("(max-width: 699px)").matches;
    const opts = {
      width,
      height,
      padding: isMobile ? [8, 12, 8, 12] : [8, 8, 4, 0],
      scales: { x: { time: true } },
      series: seriesDefs,
      axes: [
        { 
          stroke: theme.muted, 
          grid: { stroke: theme.border, width: 1 },
          font: isMobile ? "10px sans-serif" : "12px sans-serif",
        },
        {
          stroke: theme.muted,
          size: (u, vals) => {
            const s = yAxisSizeForValues(axisTarget, vals);
            return isMobile ? Math.max(s, 50) : s;
          },
          grid: { stroke: theme.border, width: 1 },
          font: isMobile ? "10px sans-serif" : "12px sans-serif",
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

  // Overlay rendering + helpers moved to common_assets/dashboard_utils.js
  // so the explorer page can share them. Pull the shared symbols into
  // the dashboard's local scope so the rest of this file keeps using
  // them by their old names without changes.
  const mergeUPlotHooks = NANOTDB_UTILS.mergeUPlotHooks;
  const eventOverlayHooks = NANOTDB_UTILS.eventOverlayHooks;
  const isValidCssColorBasic = NANOTDB_UTILS.isValidCssColorBasic;
  const overlayDefaultColor = NANOTDB_UTILS.overlayDefaultColor;

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
    const url = apiURL(path);
    const payload = await fetchJSON(url);
    const item = payload.data && payload.data.result && payload.data.result[0];
    if (!item || !item.values) {
      return [];
    }
    return item.values.map((v) => ({ x: Number(v[0]), y: Number(v[1]) })).filter((p) => Number.isFinite(p.x) && Number.isFinite(p.y));
  }

  // fetchEventSeriesPoints turns a chart series whose `event_name_pattern`
  // is set into an array of {x, y} chart points. Only numeric-typed
  // events (int32/float32) contribute; none-typed events are silently
  // skipped — they have no value to plot. x is in seconds (uPlot's time
  // scale), y goes through the same transformValue pipeline metric
  // points use so units/decimals/scale composability stays consistent.
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
        // none-typed event; can't plot.
        continue;
      }
      const y = transformValue(series, raw);
      if (!Number.isFinite(y)) {
        continue;
      }
      // ts arrives as Unix nanoseconds. uPlot's time scale uses seconds.
      out.push({ x: ts / 1e9, y });
    }
    return out;
  }

  // fetchEventOverlayMarkers delegates to the shared utils
  // implementation. Kept as a thin wrapper so the dashboard
  // continues to call it with (db, overlay, lookbackSec) without
  // passing apiBase at every call site.
  async function fetchEventOverlayMarkers(db, overlay, lookbackSec) {
    return NANOTDB_UTILS.fetchEventOverlayMarkers(
      (typeof cfg !== "undefined" && cfg && cfg.apiBaseURL) || "",
      db,
      overlay,
      lookbackSec
    );
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

  function widgetAutoRefreshEnabled(widget) {
    if (widget && typeof widget.auto_refresh === "boolean") {
      return widget.auto_refresh;
    }
    return true;
  }

  function createWidgetRefresher(run, refreshMs, controls, options) {
    const autoRefresh = !options || options.autoRefresh !== false;
    const onError = options && typeof options.onError === "function" ? options.onError : null;
    const onSuccess = options && typeof options.onSuccess === "function" ? options.onSuccess : null;
    const widgetEl = options && options.widgetEl ? options.widgetEl : null;
    let inFlight = false;
    let paused = false;
    let timerId = null;

    function setRefreshing(active) {
      if (widgetEl) {
        widgetEl.classList.toggle("widget-refreshing", Boolean(active));
      }
    }

    function updateControls() {
      if (controls.refreshBtn) {
        controls.refreshBtn.disabled = inFlight;
      }
      if (controls.pauseBtn) {
        controls.pauseBtn.textContent = paused ? "Resume" : "Pause";
        controls.pauseBtn.setAttribute("aria-pressed", paused ? "true" : "false");
        controls.pauseBtn.disabled = !autoRefresh;
      }
    }

    async function tick() {
      if (inFlight) {
        return;
      }
      inFlight = true;
      setRefreshing(true);
      updateControls();
      try {
        await run();
        if (onSuccess) {
          onSuccess();
        }
      } catch (err) {
        if (onError) {
          onError(err);
          return;
        }
        throw err;
      } finally {
        inFlight = false;
        setRefreshing(false);
        updateControls();
      }
    }

    function stop() {
      if (timerId != null) {
        clearInterval(timerId);
        timerId = null;
      }
    }

    function start() {
      if (paused || timerId != null) {
        return;
      }
      void tick();
      if (autoRefresh) {
        timerId = setInterval(tick, refreshMs);
      }
    }

    function setPaused(nextPaused) {
      paused = Boolean(nextPaused);
      if (paused) {
        stop();
      } else {
        start();
      }
      updateControls();
    }

    controls.refreshBtn.addEventListener("click", () => void tick());
    controls.pauseBtn.addEventListener("click", () => {
      if (autoRefresh) {
        setPaused(!paused);
      }
    });
    updateControls();

    return { start, stop, setPaused, refreshNow: tick };
  }

  function createWidgetHeader(titleText, titleClassName) {
    const header = document.createElement("div");
    header.className = "widget-head";

    const title = document.createElement("p");
    title.className = titleClassName;
    title.textContent = titleText;

    const controls = document.createElement("div");
    controls.className = "widget-controls";

    const refreshBtn = document.createElement("button");
    refreshBtn.type = "button";
    refreshBtn.className = "widget-control-btn widget-control-btn--icon";
    refreshBtn.textContent = "↻";
    refreshBtn.title = "Refresh widget";
    refreshBtn.setAttribute("aria-label", "Refresh widget");

    const pauseBtn = document.createElement("button");
    pauseBtn.type = "button";
    pauseBtn.className = "widget-control-btn widget-control-btn--icon";
    pauseBtn.textContent = "⏸";
    pauseBtn.title = "Pause widget auto refresh";
    pauseBtn.setAttribute("aria-label", "Pause widget auto refresh");

    controls.appendChild(refreshBtn);
    controls.appendChild(pauseBtn);
    header.appendChild(title);
    header.appendChild(controls);
    return { header, controls, refreshBtn, pauseBtn };
  }

  function activeRefreshers() {
    return groupWidgetRefreshers.get(activeGroupId) || [];
  }

  function syncDashboardRefreshControls() {
    const pauseBtn = document.getElementById("dashboardAutoRefreshBtn");
    const refreshBtn = document.getElementById("dashboardRefreshAllBtn");
    if (pauseBtn) {
      pauseBtn.textContent = dashboardRefreshPaused ? "▶" : "⏸";
      pauseBtn.setAttribute("aria-pressed", dashboardRefreshPaused ? "true" : "false");
      pauseBtn.title = dashboardRefreshPaused ? "Resume visible group auto refresh" : "Pause visible group auto refresh";
      pauseBtn.setAttribute("aria-label", pauseBtn.title);
    }
    if (refreshBtn) {
      refreshBtn.disabled = activeRefreshers().length === 0;
      refreshBtn.title = "Refresh visible group";
      refreshBtn.setAttribute("aria-label", "Refresh visible group");
    }
  }

  function setDashboardRefreshPaused(nextPaused) {
    dashboardRefreshPaused = Boolean(nextPaused);
    activeRefreshers().forEach((refresher) => refresher.setPaused(dashboardRefreshPaused));
    syncDashboardRefreshControls();
  }

  async function refreshActiveGroupNow() {
    await Promise.all(activeRefreshers().map((refresher) => refresher.refreshNow()));
    syncDashboardRefreshControls();
  }

  function charLookbackChoices(currentLookback) {
    const values = ["15m", "1h", "6h", "12h", "24h", "7d"];
    if (currentLookback && !values.includes(currentLookback)) {
      values.push(currentLookback);
    }
    return values.sort((left, right) => parseDurationSeconds(left, 0) - parseDurationSeconds(right, 0));
  }

  function formatEventTimestamp(unixNs) {
    if (!Number.isInteger(unixNs)) {
      return "--";
    }
    const ms = Math.floor(unixNs / 1000000);
    const date = new Date(ms);
    return date.toLocaleTimeString();
  }

  function formatEventPayloadSummary(payload) {
    if (!payload || typeof payload !== "object") {
      return "";
    }
    if (typeof payload.latency_ms === "number") {
      return Math.round(payload.latency_ms) + " ms";
    }
    if (typeof payload.value === "number") {
      return String(payload.value);
    }
    return "";
  }

  function extractEventValue(evt) {
    if (!evt || typeof evt !== "object") {
      return "";
    }
    if (evt.value !== undefined && evt.value !== null) {
      return String(evt.value);
    }
    if (typeof evt.int32 === "number") {
      return String(evt.int32);
    }
    if (typeof evt.float32 === "number") {
      return String(evt.float32);
    }
    if (evt.v !== undefined && evt.v !== null) {
      return String(evt.v);
    }
    return "";
  }

  function mountError(containerEl, message) {
    const card = document.createElement("article");
    card.className = "widget-error";

    const title = document.createElement("p");
    title.className = "widget-label";
    title.textContent = "Config Error";

    const body = document.createElement("p");
    body.className = "widget-foot";
    body.textContent = message;

    card.appendChild(title);
    card.appendChild(body);
    containerEl.appendChild(card);
  }

  function formatRefreshError(err) {
    const message = err && err.message ? String(err.message) : String(err || "refresh failed");
    return message.length > 160 ? message.slice(0, 157) + "..." : message;
  }

  function markWidgetRefreshError(card, foot, err) {
    if (card) {
      card.classList.add("widget-refresh-error");
    }
    if (foot) {
      foot.textContent = "refresh failed: " + formatRefreshError(err);
    }
  }

  function clearWidgetRefreshError(card) {
    if (card) {
      card.classList.remove("widget-refresh-error");
    }
  }

  function createNumberWidget(widget, containerEl) {
    const card = document.createElement("article");
    card.className = "widget-number";
    const header = createWidgetHeader(widget.title || widget.id, "widget-label");
    const value = document.createElement("p");
    value.className = "widget-value";
    value.textContent = "--";
    const foot = document.createElement("p");
    foot.className = "widget-foot";
    foot.textContent = "waiting for data";
    card.appendChild(header.header);
    card.appendChild(value);
    card.appendChild(foot);
    containerEl.appendChild(card);
    return { card, value, foot, refreshBtn: header.refreshBtn, pauseBtn: header.pauseBtn };
  }

  function createNumbersWidget(widget, containerEl) {
    const card = document.createElement("article");
    card.className = "widget-numbers";
    const header = createWidgetHeader(widget.title || widget.id, "widget-label");
    const values = document.createElement("div");
    values.className = "numbers-list";
    const items = [];

    (widget.series || []).forEach((series, idx) => {
      const row = document.createElement("div");
      row.className = "numbers-row";
      const rowLabel = document.createElement("span");
      rowLabel.className = "numbers-row-label";
      rowLabel.textContent = (series && series.label) || ("Series " + (idx + 1));
      const rowValue = document.createElement("span");
      rowValue.className = "numbers-row-value";
      rowValue.textContent = "--";
      row.appendChild(rowLabel);
      row.appendChild(rowValue);
      values.appendChild(row);
      items.push({ row, series, value: rowValue });
    });

    const foot = document.createElement("p");
    foot.className = "widget-foot";
    foot.textContent = "waiting for data";
    card.appendChild(header.header);
    card.appendChild(values);
    card.appendChild(foot);
    containerEl.appendChild(card);
    return { card, items, foot, refreshBtn: header.refreshBtn, pauseBtn: header.pauseBtn };
  }

  function createChartWidget(widget, containerEl, currentLookback) {
    const card = document.createElement("article");
    card.className = "widget-chart" + (widgetChartType(widget) === "aggregate_band" ? " widget-chart--aggregate-band" : "");
    const header = createWidgetHeader(widget.title || widget.id, "chart-title");
    const lookbackSelect = document.createElement("select");
    lookbackSelect.className = "widget-control-select widget-lookback-select";
    lookbackSelect.setAttribute("aria-label", "Chart lookback");
    charLookbackChoices(currentLookback).forEach((value) => {
      const option = document.createElement("option");
      option.value = value;
      option.textContent = value;
      if (value === currentLookback) {
        option.selected = true;
      }
      lookbackSelect.appendChild(option);
    });
    header.controls.insertBefore(lookbackSelect, header.refreshBtn);
    const plot = document.createElement("div");
    plot.className = "chart-plot";
    const foot = document.createElement("p");
    foot.className = "widget-foot";
    foot.textContent = "waiting for data";
    card.appendChild(header.header);
    card.appendChild(plot);
    card.appendChild(foot);
    containerEl.appendChild(card);
    return { card, plot, foot, refreshBtn: header.refreshBtn, pauseBtn: header.pauseBtn, lookbackSelect };
  }

  function createEventLogWidget(widget, containerEl) {
    const card = document.createElement("article");
    card.className = "widget-eventlog";
    const header = createWidgetHeader(widget.title || widget.id, "widget-label");
    const events = document.createElement("div");
    events.className = "eventlog-rows";
    const foot = document.createElement("p");
    foot.className = "widget-foot";
    foot.textContent = "waiting for events";
    card.appendChild(header.header);
    card.appendChild(events);
    card.appendChild(foot);
    containerEl.appendChild(card);
    return { card, events, foot, refreshBtn: header.refreshBtn, pauseBtn: header.pauseBtn };
  }

  function buildWidget(widget, containerEl, dashboardCfg) {
    const refreshMs = Math.max((widget.refresh_sec || 10) * 1000, 5000);
    const autoRefresh = widgetAutoRefreshEnabled(widget);

    if (widget.type === "number") {
      const els = createNumberWidget(widget, containerEl);
      const refresh = async () => {
        const series = (widget.series || [])[0];
        const db = series && seriesDB(series, dashboardCfg);
          const query = series && seriesMetric(series);
          if (!db || !query) {
          els.value.textContent = "--";
            els.foot.textContent = "missing db/query";
          applySeverityClass(els.card, "none");
          return;
        }
          const point = await fetchLast(db, series, parseDurationSeconds(widget.lookback || series.window || "5m", 300));
        if (!point) {
          els.value.textContent = "--";
          els.foot.textContent = "no value";
          applySeverityClass(els.card, "none");
          return;
        }
        els.value.textContent = formatWidgetValue(series || widget, point.value);
        applySeverityClass(els.card, classifySeverity(series || widget, point.value));
        els.foot.textContent = "updated " + new Date().toLocaleTimeString();
      };
      return createWidgetRefresher(refresh, refreshMs, { refreshBtn: els.refreshBtn, pauseBtn: els.pauseBtn }, {
        autoRefresh,
        widgetEl: els.card,
        onError: (err) => {
          applySeverityClass(els.card, "none");
          markWidgetRefreshError(els.card, els.foot, err);
        },
        onSuccess: () => clearWidgetRefreshError(els.card),
      });
    }

    if (widget.type === "numbers") {
      const els = createNumbersWidget(widget, containerEl);
      const refresh = async () => {
        let validCount = 0;
        await Promise.all(els.items.map(async (item) => {
          const db = seriesDB(item.series, dashboardCfg);
          const query = seriesMetric(item.series);
          item.row.classList.remove("value-normal", "value-warning", "value-critical");
          if (!db || !query) {
            item.value.textContent = "--";
            return;
          }
          const point = await fetchLast(db, item.series, parseDurationSeconds(widget.lookback || item.series.window || "5m", 300));
          if (!point) {
            item.value.textContent = "--";
            return;
          }
          validCount += 1;
          item.value.textContent = formatWidgetValue(item.series, point.value);
          const severity = classifySeverity(item.series, point.value);
          if (severity !== "none") {
            item.row.classList.add("value-" + severity);
          }
        }));
        els.foot.textContent = validCount > 0 ? "updated " + new Date().toLocaleTimeString() : "no values";
      };
      return createWidgetRefresher(refresh, refreshMs, { refreshBtn: els.refreshBtn, pauseBtn: els.pauseBtn }, {
        autoRefresh,
        widgetEl: els.card,
        onError: (err) => markWidgetRefreshError(els.card, els.foot, err),
        onSuccess: () => clearWidgetRefreshError(els.card),
      });
    }

    if (widgetChartType(widget) === "line_chart" || widgetChartType(widget) === "aggregate_band") {
      let currentLookback = widget.lookback || "1h";
      const els = createChartWidget(widget, containerEl, currentLookback);
      const refresh = async () => {
        const lookbackSec = parseDurationSeconds(currentLookback, 3600);
        const step = widget.interval || "30s";
        const chartSeries = expandedChartSeries(widget);
        let filteredItems = await fetchAggregateBandSeries(widget, chartSeries, lookbackSec, dashboardCfg);
        if (!filteredItems) {
          const seriesItems = new Array(chartSeries.length);
          await Promise.all(chartSeries.map(async (series, idx) => {
            const db = seriesDB(series, dashboardCfg);
            const isEventBacked = !!(series && (series.event_name_pattern || "").trim());
            if (isEventBacked) {
              // Each int32/float32 event becomes one scatter point.
              const points = await fetchEventSeriesPoints(db, series, lookbackSec);
              seriesItems[idx] = {
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
            seriesItems[idx] = {
              label: effectiveSeriesLabel(series, idx),
              role: seriesRole(series),
              points: points.map((p) => ({ x: p.x, y: transformValue(series, p.y) })).filter((p) => Number.isFinite(p.y)),
            };
          }));
          filteredItems = seriesItems.filter(Boolean);
        }

        // Pull event_overlays in parallel with the series fetch above.
        // Overlays only apply to line_chart (validated server-side); the
        // aggregate_band branch never reaches here with overlays defined.
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

        const hasData = renderUPlotChart(els.plot, widget, filteredItems, overlayLayers);
        if (widgetChartType(widget) === "aggregate_band") {
          els.foot.textContent = "";
        } else {
          const overlayMarkerCount = overlayLayers.reduce((acc, layer) => acc + (layer.markers ? layer.markers.length : 0), 0);
          let statusText = hasData ? "updated " + new Date().toLocaleTimeString() + " · " + currentLookback : "no points for " + currentLookback;
          if (overlayMarkerCount > 0) {
            statusText += " · " + overlayMarkerCount + " event" + (overlayMarkerCount === 1 ? "" : "s") + " overlaid";
          }
          els.foot.textContent = statusText;
        }
      };
      const refresher = createWidgetRefresher(refresh, refreshMs, { refreshBtn: els.refreshBtn, pauseBtn: els.pauseBtn }, {
        autoRefresh,
        widgetEl: els.plot.parentElement,
        onError: (err) => markWidgetRefreshError(els.plot.parentElement, els.foot, err),
        onSuccess: () => clearWidgetRefreshError(els.plot.parentElement),
      });
      els.lookbackSelect.addEventListener("change", () => {
        currentLookback = els.lookbackSelect.value || currentLookback;
        void refresher.refreshNow();
      });
      return refresher;
    }

    if (widget.type === "event_log") {
      const els = createEventLogWidget(widget, containerEl);
      const refresh = async () => {
        // Multi-series: every series with a non-empty event_name_pattern
        // contributes events to the same scrolling feed, merged by ts
        // descending. Lets one widget show "nanotdb.*" alongside
        // "drip.*" (or any other split the operator wants) without
        // stacking two widgets side by side.
        const series = (widget.series || []).filter(
          (s) => s && (s.event_name_pattern || "").trim() !== ""
        );
        if (series.length === 0) {
          els.events.textContent = "";
          els.foot.textContent = "missing db/event_name_pattern";
          return;
        }
        const lookbackSec = parseDurationSeconds(widget.lookback || "1h", 3600);
        const end = new Date();
        const start = new Date(end.getTime() - lookbackSec * 1000);
        const fetched = await Promise.all(series.map(async (s) => {
          const db = seriesDB(s, dashboardCfg);
          if (!db) return [];
          const pattern = (s.event_name_pattern || "").trim();
          const limit = s.event_limit ? s.event_limit : 10;
          const url = apiURL(`/api/v1/events?db=${encodeURIComponent(db)}&name=${encodeURIComponent(pattern)}&start=${encodeURIComponent(start.toISOString())}&end=${encodeURIComponent(end.toISOString())}&limit=${limit}`);
          try {
            const payload = await fetchJSON(url);
            return (payload.data && payload.data.result) ? payload.data.result : [];
          } catch (err) {
            // Per-series errors don't kill the widget; other series
            // still render. The error itself is surfaced via the
            // outer createWidgetRefresher onError when ALL series
            // fail; partial failures are quietly skipped here.
            return [];
          }
        }));
        // Flatten, sort newest-first, cap the merged feed at the sum
        // of per-series caps so the widget never grows unbounded.
        const merged = [];
        fetched.forEach((evts) => evts.forEach((e) => merged.push(e)));
        merged.sort((a, b) => (b.ts || b.T || 0) - (a.ts || a.T || 0));
        const totalCap = series.reduce((sum, s) => sum + (s.event_limit ? s.event_limit : 10), 0);
        const events = merged.slice(0, totalCap);
        els.events.innerHTML = "";
        if (events.length === 0) {
          els.foot.textContent = "no events";
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
          timeCell.textContent = formatEventTimestamp(evt.ts || evt.T);
          row.appendChild(nameCell);
          row.appendChild(timeCell);
          els.events.appendChild(row);
        });
        els.foot.textContent = "updated " + new Date().toLocaleTimeString();
      };
      return createWidgetRefresher(refresh, refreshMs, { refreshBtn: els.refreshBtn, pauseBtn: els.pauseBtn }, {
        autoRefresh,
        widgetEl: els.card,
        onError: (err) => {
          els.events.innerHTML = "";
          markWidgetRefreshError(els.card, els.foot, err);
        },
        onSuccess: () => clearWidgetRefreshError(els.card),
      });
    }

    mountError(containerEl, "unsupported widget type: " + widget.type);
    return null;
  }

  function activateGroup(groupId) {
    groupTimers.forEach((timers) => timers.forEach((refresher) => refresher.stop()));
    groupTimers.clear();
    groupPanes.forEach((pane, gid) => {
      pane.hidden = gid !== groupId;
      pane.classList.remove("widgets-stage-in");
    });
    document.querySelectorAll(".group-tab, .accordion-header").forEach((el) => {
      el.classList.toggle("active", el.dataset.groupId === groupId);
    });
    activeGroupId = groupId;
    const refreshers = groupWidgetRefreshers.get(groupId) || [];
    refreshers.forEach((refresher) => {
      refresher.setPaused(dashboardRefreshPaused);
      if (!dashboardRefreshPaused) {
        refresher.start();
      }
    });
    groupTimers.set(groupId, refreshers);
    syncDashboardRefreshControls();
    const pane = groupPanes.get(groupId);
    if (pane) {
      void pane.offsetWidth;
      pane.classList.add("widgets-stage-in");
      requestAnimationFrame(() => rebalanceSingleNumberRows(pane));
    }
  }

  async function loadDashboard() {
  const dashboardCfg = await fetchJSON(apiURL("/api/dashboard-config"));
  document.getElementById("dashboard-title").textContent = dashboardCfg.title || "NanoTDB Dashboard";

    const navEl = document.getElementById("group-nav");
    const containerEl = document.getElementById("widgets");
    navEl.innerHTML = "";
    containerEl.innerHTML = "";
    groupPanes.clear();
    groupTimers.clear();
    groupWidgetRefreshers.clear();
    activeGroupId = "";

    const groups = dashboardCfg.groups || [];
    const widgetDefs = dashboardCfg.widgets || {};
    if (groups.length === 0) {
      mountError(containerEl, "No groups configured");
      return;
    }

    groups.forEach((group) => {
      const section = document.createElement("section");
      section.className = "group-pane";

      const accHeader = document.createElement("button");
      accHeader.type = "button";
      accHeader.className = "accordion-header";
      accHeader.dataset.groupId = group.id;
      accHeader.textContent = group.label || group.id;
      accHeader.addEventListener("click", () => activateGroup(group.id));
      section.appendChild(accHeader);

      const pane = document.createElement("div");
      pane.className = "widgets";
      pane.hidden = true;
      section.appendChild(pane);
      containerEl.appendChild(section);
      groupPanes.set(group.id, pane);

      const refreshers = [];
      const missing = [];
      (group.widgets || []).forEach((widgetId) => {
        const widget = widgetDefs[widgetId];
        if (!widget) {
          missing.push(widgetId);
          return;
        }
        const refresher = buildWidget(Object.assign({}, widget, { id: widget.id || widgetId }), pane, dashboardCfg);
        if (refresher) {
          refreshers.push(refresher);
        }
      });
      if (missing.length > 0) {
        mountError(pane, "Unknown widget ids: " + missing.join(", "));
      }
      groupWidgetRefreshers.set(group.id, refreshers);

      const tab = document.createElement("button");
      tab.type = "button";
      tab.className = "group-tab";
      tab.dataset.groupId = group.id;
      tab.textContent = group.label || group.id;
      tab.addEventListener("click", () => activateGroup(group.id));
      navEl.appendChild(tab);
    });

    activateGroup(groups[0].id);
  }

  function wireDashboardControls() {
    const pauseBtn = document.getElementById("dashboardAutoRefreshBtn");
    const refreshBtn = document.getElementById("dashboardRefreshAllBtn");
    if (pauseBtn) {
      pauseBtn.addEventListener("click", () => {
        setDashboardRefreshPaused(!dashboardRefreshPaused);
      });
    }
    if (refreshBtn) {
      refreshBtn.addEventListener("click", () => {
        void refreshActiveGroupNow();
      });
    }
    syncDashboardRefreshControls();
  }

  window.addEventListener("resize", () => {
    groupPanes.forEach((pane) => {
      if (!pane.hidden) {
        requestAnimationFrame(() => rebalanceSingleNumberRows(pane));
      }
    });
  });

  wireDashboardControls();

  loadDashboard().catch((err) => {
    const host = document.getElementById("widgets");
    host.innerHTML = "";
    mountError(host, err && err.message ? err.message : String(err));
  });
})();
