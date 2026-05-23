(function () {
  const cfg = JSON.parse(document.getElementById("dashboardConfig").textContent) || { basePath: "/dashboard", refreshSeconds: 10, apiBaseURL: "" };
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

  const {
    parseDurationSeconds,
    seriesDB,
    seriesMetric,
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

  function renderUPlotChart(plotEl, widget, seriesItems) {
    if (typeof uPlot !== "function") {
      throw new Error("uPlot not loaded");
    }

    const items = Array.isArray(seriesItems) ? seriesItems : [];
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
      seriesDefs.push({
        label: item && item.label ? item.label : ("Series " + (idx + 1)),
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
    return { header, controls, refreshBtn, pauseBtn };
  }

  function chartLookbackChoices(currentLookback) {
    const values = ["15m", "1h", "6h", "12h", "24h", "7d"];
    if (currentLookback && !values.includes(currentLookback)) {
      values.push(currentLookback);
    }
    return values.sort((left, right) => parseDurationSeconds(left, 0) - parseDurationSeconds(right, 0));
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
    card.className = "widget-chart";
    const header = createWidgetHeader(widget.title || widget.id, "chart-title");
    const lookbackSelect = document.createElement("select");
    lookbackSelect.className = "widget-control-select widget-lookback-select";
    lookbackSelect.setAttribute("aria-label", "Chart lookback");
    chartLookbackChoices(currentLookback).forEach((value) => {
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

  function buildWidget(widget, containerEl, dashboardCfg) {
    const refreshMs = Math.max((widget.refresh_sec || 10) * 1000, 5000);
    const autoRefresh = widgetAutoRefreshEnabled(widget);

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
      return createWidgetRefresher(refresh, refreshMs, { refreshBtn: els.refreshBtn, pauseBtn: els.pauseBtn }, {
        autoRefresh,
        widgetEl: els.card,
        onError: (err) => markWidgetRefreshError(els.card, els.foot, err),
        onSuccess: () => clearWidgetRefreshError(els.card),
      });
    }

    if (widget.type === "line_chart") {
      let currentLookback = widget.lookback || "1h";
      const els = createChartWidget(widget, containerEl, currentLookback);
      const refresh = async () => {
        const lookbackSec = parseDurationSeconds(currentLookback, 3600);
        const step = widget.interval || "30s";
        const seriesItems = new Array((widget.series || []).length);
        await Promise.all((widget.series || []).map(async (series, idx) => {
          const db = seriesDB(series, dashboardCfg);
          const metric = seriesMetric(series);
          if (!db || !metric) {
            return;
          }
          const points = await fetchRange(db, metric, lookbackSec, step);
          seriesItems[idx] = {
            label: series.label || metric || ("Series " + (idx + 1)),
            points: points.map((p) => ({ x: p.x, y: transformValue(series, p.y) })).filter((p) => Number.isFinite(p.y)),
          };
        }));
        const hasData = renderUPlotChart(els.plot, widget, seriesItems.filter(Boolean));
        els.foot.textContent = hasData ? "updated " + new Date().toLocaleTimeString() + " · " + currentLookback : "no points for " + currentLookback;
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
    const refreshers = groupWidgetRefreshers.get(groupId) || [];
    refreshers.forEach((refresher) => refresher.start());
    groupTimers.set(groupId, refreshers);
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
