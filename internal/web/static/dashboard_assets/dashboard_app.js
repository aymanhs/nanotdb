(function () {
  const cfg = window.NANOTDB_DASH_CONFIG || { basePath: "/dashboard", refreshSeconds: 10, apiBaseURL: "" };
  const chartState = new Map();
  const groupPanes = new Map();
  const groupTimers = new Map();
  const groupWidgetRefreshers = new Map();

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

  function parseDurationSeconds(v, fallback) {
    if (!v || typeof v !== "string") {
      return fallback;
    }
    const m = v.trim().match(/^(\d+)(s|m|h|d|w)$/i);
    if (!m) {
      return fallback;
    }
    const n = Number(m[1]);
    const u = m[2].toLowerCase();
    if (u === "s") return n;
    if (u === "m") return n * 60;
    if (u === "h") return n * 3600;
    if (u === "d") return n * 86400;
    if (u === "w") return n * 604800;
    return fallback;
  }

  function seriesDB(series, dashboardCfg) {
    return series.db || series.database || dashboardCfg.default_db || "";
  }

  function seriesMetric(series) {
    if (series.metric) {
      return series.metric;
    }
    if (series.measurement && series.field) {
      return series.measurement + "." + series.field;
    }
    return "";
  }

  function resolveDisplayConfig(target) {
    const transform = target && target.transform && typeof target.transform === "object" ? target.transform : null;
    return {
      factor: Number.isFinite(transform && transform.factor) ? Number(transform.factor) : (Number.isFinite(target && target.scale) ? Number(target.scale) : 1),
      offset: Number.isFinite(transform && transform.offset) ? Number(transform.offset) : (Number.isFinite(target && target.offset) ? Number(target.offset) : 0),
      unit: typeof (transform && transform.unit) === "string" ? transform.unit : (typeof (target && target.unit) === "string" ? target.unit : ""),
      decimals: Number.isInteger(transform && transform.decimals) && transform.decimals >= 0 ? transform.decimals : (Number.isInteger(target && target.decimals) && target.decimals >= 0 ? target.decimals : 1),
      format: typeof (transform && transform.format) === "string" ? transform.format : (typeof (target && target.format) === "string" ? target.format : ""),
    };
  }

  function transformValue(target, rawValue) {
    const n = Number(rawValue);
    if (!Number.isFinite(n)) {
      return NaN;
    }
    const cfg = resolveDisplayConfig(target);
    return n * cfg.factor + cfg.offset;
  }

  function trimTrailingZeros(numText) {
    return String(numText)
      .replace(/(\.\d*?[1-9])0+$/u, "$1")
      .replace(/\.0+$/u, "");
  }

  function formatNumericValue(value, precision) {
    const p = Number.isInteger(precision) && precision >= 0 ? precision : 0;
    return trimTrailingZeros(Number(value).toFixed(p));
  }

  function formatDurationFromSeconds(value) {
    const total = Math.max(0, Math.floor(Number(value)));
    if (!Number.isFinite(total)) {
      return "--";
    }
    const days = Math.floor(total / 86400);
    const hours = Math.floor((total % 86400) / 3600);
    const mins = Math.floor((total % 3600) / 60);
    if (days > 0) {
      return days + "d " + hours + "h " + mins + "m";
    }
    if (hours > 0) {
      return hours + "h " + mins + "m";
    }
    return mins + "m";
  }

  function formatWidgetValue(target, rawValue) {
    const cfg = resolveDisplayConfig(target);
    const value = transformValue(target, rawValue);
    if (!Number.isFinite(value)) {
      return "--";
    }

    const fixed = formatNumericValue(value, cfg.decimals);
    const format = cfg.format;
    const unit = cfg.unit;

    if (format) {
      if (format.includes("{duration}")) {
        return format.replaceAll("{duration}", formatDurationFromSeconds(value));
      }
      if (format.includes("{value}")) {
        return format.replaceAll("{value}", fixed);
      }
    }

    return unit ? fixed + unit : fixed;
  }

  function classifySeverity(target, rawValue) {
    if (!target || !target.thresholds) {
      return "none";
    }

    const value = transformValue(target, rawValue);
    if (!Number.isFinite(value)) {
      return "none";
    }

    const thresholds = target.thresholds;
    const dir = thresholds.direction === "below" ? "below" : "above";
    const hasWarning = Number.isFinite(thresholds.warning);
    const hasCritical = Number.isFinite(thresholds.critical);
    if (!hasWarning && !hasCritical) {
      return "none";
    }

    if (dir === "above") {
      if (hasCritical && value >= thresholds.critical) {
        return "critical";
      }
      if (hasWarning && value >= thresholds.warning) {
        return "warning";
      }
      return "normal";
    }

    if (hasCritical && value <= thresholds.critical) {
      return "critical";
    }
    if (hasWarning && value <= thresholds.warning) {
      return "warning";
    }
    return "normal";
  }

  function applySeverityClass(el, severity) {
    el.classList.remove("value-normal", "value-warning", "value-critical");
    if (severity === "normal" || severity === "warning" || severity === "critical") {
      el.classList.add("value-" + severity);
    }
  }

  function pickSeriesColor(idx) {
    const palette = ["#2dd4a4", "#60a5fa", "#f59e0b", "#ef4444", "#22d3ee", "#f472b6"];
    return palette[idx % palette.length];
  }

  function chartTheme() {
    const root = getComputedStyle(document.documentElement);
    return {
      text: root.getPropertyValue("--text").trim() || "#e8ecf1",
      muted: root.getPropertyValue("--muted").trim() || "#a8b5c5",
      border: root.getPropertyValue("--border").trim() || "#3a4558",
    };
  }

  function yAxisSizeForValues(target, vals) {
    let maxLen = 0;
    (vals || []).forEach((value) => {
      const label = value == null ? "" : formatWidgetValue(target, value);
      maxLen = Math.max(maxLen, String(label).length);
    });
    const isMobile = window.matchMedia("(max-width: 699px)").matches;
    const minSize = isMobile ? 40 : 80;
    const maxSize = isMobile ? 84 : 220;
    return Math.min(maxSize, Math.max(minSize, maxLen * 8 + 16));
  }

  function buildUPlotData(seriesMap) {
    const timeSet = new Set();
    Object.values(seriesMap).forEach((points) => {
      (points || []).forEach((point) => timeSet.add(point.x));
    });
    const x = Array.from(timeSet).sort((a, b) => a - b);
    const data = [x];
    Object.keys(seriesMap).forEach((label) => {
      const byTs = new Map((seriesMap[label] || []).map((point) => [point.x, point.y]));
      data.push(x.map((ts) => (byTs.has(ts) ? byTs.get(ts) : null)));
    });
    return data;
  }

  function chartDisplayTarget(widget) {
    if (widget && Array.isArray(widget.series) && widget.series.length > 0) {
      return widget.series[0];
    }
    return widget;
  }

  function renderUPlotChart(plotEl, widget, seriesMap) {
    if (typeof uPlot !== "function") {
      throw new Error("uPlot not loaded");
    }

    const data = buildUPlotData(seriesMap);
    if (!data[0] || data[0].length === 0) {
      const existing = chartState.get(widget.id);
      if (existing) {
        existing.destroy();
        chartState.delete(widget.id);
      }
      plotEl.innerHTML = "";
      return false;
    }

    const labels = Object.keys(seriesMap);
    const seriesDefs = [{ label: "Time" }];
    labels.forEach((label, idx) => {
      seriesDefs.push({
        label,
        stroke: pickSeriesColor(idx),
        width: 2,
        spanGaps: true,
        points: { show: true, size: 5, width: 2 },
      });
    });

    const theme = chartTheme();
    const width = Math.max(plotEl.clientWidth || 0, 280);
    const height = Math.max(plotEl.clientHeight || 0, 220);
    const axisTarget = chartDisplayTarget(widget);
    let instance = chartState.get(widget.id);

    if (!instance) {
      const isMobile = window.matchMedia("(max-width: 699px)").matches;
      const opts = {
        width,
        height,
        padding: isMobile ? [4, 4, 2, 2] : [8, 8, 4, 4],
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
      instance = new uPlot(opts, data, plotEl);
      chartState.set(widget.id, instance);
    } else {
      instance.setSize({ width, height });
      instance.setData(data);
    }

    return true;
  }

  function rebalanceSingleNumberRows(containerEl) {
    if (!containerEl) {
      return;
    }
    const cards = Array.from(containerEl.children || []).filter((card) => card.classList.contains("widget-number"));
    cards.forEach((card) => card.classList.remove("widget-number--full"));
    const rows = new Map();
    cards.forEach((card) => {
      const top = Math.round(card.getBoundingClientRect().top);
      if (!rows.has(top)) {
        rows.set(top, []);
      }
      rows.get(top).push(card);
    });
    rows.forEach((rowCards) => {
      if (rowCards.length === 1) {
        rowCards[0].classList.add("widget-number--full");
      }
    });
  }

  async function fetchLast(db, metric) {
  const payload = await fetchJSON(apiURL("/api/v1/query?db=" + encodeURIComponent(db) + "&query=" + encodeURIComponent(metric)));
    const item = payload.data && payload.data.result && payload.data.result[0];
    if (!item) {
      return null;
    }
    return { ts: Number(item.value[0]), value: Number(item.value[1]) };
  }

  async function fetchRange(db, metric, lookbackSec, step) {
    const end = new Date();
    const start = new Date(end.getTime() - lookbackSec * 1000);
    const url = apiURL("/api/v1/query_range?db=" + encodeURIComponent(db) +
      "&query=" + encodeURIComponent(metric) +
      "&start=" + encodeURIComponent(start.toISOString()) +
      "&end=" + encodeURIComponent(end.toISOString()) +
      "&step=" + encodeURIComponent(step || "30s"));
    const payload = await fetchJSON(url);
    const item = payload.data && payload.data.result && payload.data.result[0];
    if (!item || !item.values) {
      return [];
    }
    return item.values.map((v) => ({ x: Number(v[0]), y: Number(v[1]) })).filter((p) => Number.isFinite(p.x) && Number.isFinite(p.y));
  }

  function createWidgetRefresher(run, refreshMs, controls) {
    let inFlight = false;
    let paused = false;
    let timerId = null;

    function updateControls() {
      if (controls.refreshBtn) {
        controls.refreshBtn.disabled = inFlight;
      }
      if (controls.pauseBtn) {
        controls.pauseBtn.textContent = paused ? "Resume" : "Pause";
        controls.pauseBtn.setAttribute("aria-pressed", paused ? "true" : "false");
      }
    }

    async function tick() {
      if (inFlight) {
        return;
      }
      inFlight = true;
      updateControls();
      try {
        await run();
      } finally {
        inFlight = false;
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
      timerId = setInterval(tick, refreshMs);
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
    controls.pauseBtn.addEventListener("click", () => setPaused(!paused));
    updateControls();

    return { start, stop, setPaused };
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
    refreshBtn.className = "widget-control-btn";
    refreshBtn.textContent = "Refresh";

    const pauseBtn = document.createElement("button");
    pauseBtn.type = "button";
    pauseBtn.className = "widget-control-btn";
    pauseBtn.textContent = "Pause";

    controls.appendChild(refreshBtn);
    controls.appendChild(pauseBtn);
    header.appendChild(title);
    header.appendChild(controls);
    return { header, refreshBtn, pauseBtn };
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

  function createChartWidget(widget, containerEl) {
    const card = document.createElement("article");
    card.className = "widget-chart";
    const header = createWidgetHeader(widget.title || widget.id, "chart-title");
    const plot = document.createElement("div");
    plot.className = "chart-plot";
    const foot = document.createElement("p");
    foot.className = "widget-foot";
    foot.textContent = "waiting for data";
    card.appendChild(header.header);
    card.appendChild(plot);
    card.appendChild(foot);
    containerEl.appendChild(card);
    return { plot, foot, refreshBtn: header.refreshBtn, pauseBtn: header.pauseBtn };
  }

  function buildWidget(widget, containerEl, dashboardCfg) {
    const refreshMs = Math.max((widget.refresh_sec || 10) * 1000, 5000);

    if (widget.type === "number") {
      const els = createNumberWidget(widget, containerEl);
      const refresh = async () => {
        const series = (widget.series || [])[0];
        const db = series && seriesDB(series, dashboardCfg);
        const metric = series && seriesMetric(series);
        if (!db || !metric) {
          els.value.textContent = "--";
          els.foot.textContent = "missing db/metric";
          applySeverityClass(els.card, "none");
          return;
        }
        const point = await fetchLast(db, metric);
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
      return createWidgetRefresher(refresh, refreshMs, { refreshBtn: els.refreshBtn, pauseBtn: els.pauseBtn });
    }

    if (widget.type === "numbers") {
      const els = createNumbersWidget(widget, containerEl);
      const refresh = async () => {
        let validCount = 0;
        await Promise.all(els.items.map(async (item) => {
          const db = seriesDB(item.series, dashboardCfg);
          const metric = seriesMetric(item.series);
          item.row.classList.remove("value-normal", "value-warning", "value-critical");
          if (!db || !metric) {
            item.value.textContent = "--";
            return;
          }
          const point = await fetchLast(db, metric);
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
      return createWidgetRefresher(refresh, refreshMs, { refreshBtn: els.refreshBtn, pauseBtn: els.pauseBtn });
    }

    if (widget.type === "line_chart") {
      const els = createChartWidget(widget, containerEl);
      const refresh = async () => {
        const lookbackSec = parseDurationSeconds(widget.lookback || "1h", 3600);
        const step = widget.interval || "30s";
        const seriesMap = {};
        await Promise.all((widget.series || []).map(async (series, idx) => {
          const db = seriesDB(series, dashboardCfg);
          const metric = seriesMetric(series);
          if (!db || !metric) {
            return;
          }
          const key = series.label || metric || ("Series " + (idx + 1));
          const points = await fetchRange(db, metric, lookbackSec, step);
          seriesMap[key] = points.map((p) => ({ x: p.x, y: transformValue(series, p.y) })).filter((p) => Number.isFinite(p.y));
        }));
        const hasData = renderUPlotChart(els.plot, widget, seriesMap);
        els.foot.textContent = hasData ? "updated " + new Date().toLocaleTimeString() : "no points";
      };
      return createWidgetRefresher(refresh, refreshMs, { refreshBtn: els.refreshBtn, pauseBtn: els.pauseBtn });
    }

    mountError(containerEl, "unsupported widget type: " + widget.type);
    return null;
  }

  function activateGroup(groupId) {
    groupTimers.forEach((timers) => timers.forEach((refresher) => refresher.stop()));
    groupTimers.clear();
    groupPanes.forEach((pane, gid) => {
      pane.hidden = gid !== groupId;
    });
    document.querySelectorAll(".group-tab, .accordion-header").forEach((el) => {
      el.classList.toggle("active", el.dataset.groupId === groupId);
    });
    const refreshers = groupWidgetRefreshers.get(groupId) || [];
    refreshers.forEach((refresher) => refresher.start());
    groupTimers.set(groupId, refreshers);
    const pane = groupPanes.get(groupId);
    if (pane) {
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

  window.addEventListener("resize", () => {
    groupPanes.forEach((pane) => {
      if (!pane.hidden) {
        requestAnimationFrame(() => rebalanceSingleNumberRows(pane));
      }
    });
  });

  loadDashboard().catch((err) => {
    const host = document.getElementById("widgets");
    host.innerHTML = "";
    mountError(host, err && err.message ? err.message : String(err));
  });
})();
