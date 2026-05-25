(function () {
  const cfg = window.NANOTDB_DASH_CONFIG || { basePath: "/dashboard", refreshSeconds: 10, apiBaseURL: "" };
  const {
    buildInstantQueryPath,
    buildRangeQueryPath,
    seriesUsesAggregateRange,
  } = window.NANOTDB_UTILS || {};

  function apiURL(path) {
    const base = typeof cfg.apiBaseURL === "string" ? cfg.apiBaseURL.replace(/\/$/, "") : "";
    return base + path;
  }

  const dbSelect = document.getElementById("dbSelect");
  const queryInput = document.getElementById("queryInput");
  const metricsOptions = document.getElementById("metricsOptions");
  const addQueryBtn = document.getElementById("addQueryBtn");
  const aggregateInput = document.getElementById("aggregateInput");
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
    const res = await fetch(url);
    if (!res.ok) {
      throw new Error("HTTP " + res.status + " for " + url);
    }
    return res.json();
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
          values: (u, vals) => vals.map((value) => (value == null ? "" : Number(value).toFixed(2))),
        },
      ],
      legend: { show: true, live: true, isolate: false },
    };

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
      await loadDatabases();
      await refreshAll();
      restartAutoRefreshTimer();
    } catch (err) {
      setStatus("Failed to load dashboard: " + err.message);
    }
  }

  init();
})();
