(function () {
  const cfg = window.NANOTDB_DASH_CONFIG || { basePath: "/dashboard", refreshSeconds: 10, apiBaseURL: "" };

  function apiURL(path) {
    const base = typeof cfg.apiBaseURL === "string" ? cfg.apiBaseURL.replace(/\/$/, "") : "";
    return base + path;
  }

  const dbSelect = document.getElementById("dbSelect");
  const metricInput = document.getElementById("metricInput");
  const metricsOptions = document.getElementById("metricsOptions");
  const addMetricBtn = document.getElementById("addMetricBtn");
  const selectedMetricsEl = document.getElementById("selectedMetrics");
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
  let selectedMetricNames = [];
  let chartInstance = null;
  let lastSeriesByMetric = {};
  let lastMetricOrder = [];
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

  function selectedMetrics() {
    return selectedMetricNames.slice();
  }

  function renderSelectedMetrics() {
    selectedMetricsEl.innerHTML = "";
    if (!selectedMetricNames.length) {
      const empty = document.createElement("span");
      empty.className = "selected-empty";
      empty.textContent = "No metrics selected";
      selectedMetricsEl.appendChild(empty);
      return;
    }

    selectedMetricNames.forEach((name) => {
      const chip = document.createElement("span");
      chip.className = "metric-chip";

      const text = document.createElement("span");
      text.textContent = name;

      const removeBtn = document.createElement("button");
      removeBtn.type = "button";
      removeBtn.className = "chip-remove";
      removeBtn.setAttribute("aria-label", "Remove " + name);
      removeBtn.textContent = "x";
      removeBtn.addEventListener("click", async () => {
        selectedMetricNames = selectedMetricNames.filter((m) => m !== name);
        renderSelectedMetrics();
        await refreshAll();
      });

      chip.appendChild(text);
      chip.appendChild(removeBtn);
      selectedMetricsEl.appendChild(chip);
    });
  }

  async function addMetric(name) {
    const metric = (name || "").trim();
    if (!metric) {
      return;
    }
    if (!metricCatalog.includes(metric)) {
      setStatus("Unknown metric: " + metric);
      return;
    }
    if (selectedMetricNames.includes(metric)) {
      metricInput.value = "";
      return;
    }
    selectedMetricNames.push(metric);
    metricInput.value = "";
    renderSelectedMetrics();
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
    items.forEach((m) => {
      const opt = document.createElement("option");
      opt.value = m;
      metricsOptions.appendChild(opt);
    });

    selectedMetricNames = selectedMetricNames.filter((m) => metricCatalog.includes(m));
    if (selectedMetricNames.length === 0) {
      selectedMetricNames = metricCatalog.slice(0, 3);
    }
    renderSelectedMetrics();
  }

  async function renderLastValues(db, metrics) {
    cards.innerHTML = "";
    const jobs = metrics.map(async (metric) => {
      const data = await fetchJSON(
        apiURL("/api/v1/query?db=" + encodeURIComponent(db) + "&query=" + encodeURIComponent(metric))
      );
      const result = data.data && data.data.result && data.data.result[0];
      const card = document.createElement("div");
      card.className = "card";
      if (!result) {
        card.innerHTML =
          '<div class="metric">' + metric + '</div><div class="value">-</div><div class="ts">no data</div>';
        return card;
      }
      const ts = Number(result.value[0]) * 1000;
      card.innerHTML =
        '<div class="metric">' + metric + "</div>" +
        '<div class="value">' + result.value[1] + "</div>" +
        '<div class="ts">' + new Date(ts).toLocaleString() + "</div>";
      return card;
    });
    const renderedCards = await Promise.all(jobs);
    renderedCards.forEach((card) => cards.appendChild(card));
  }

  async function loadSeries(db, metric, fromIso, toIso, step) {
    const url = apiURL(
      "/api/v1/query_range?db=" + encodeURIComponent(db) +
        "&query=" + encodeURIComponent(metric) +
        "&start=" + encodeURIComponent(fromIso) +
        "&end=" + encodeURIComponent(toIso) +
        "&step=" + encodeURIComponent(step)
    );
    const payload = await fetchJSON(url);
    const result = payload.data && payload.data.result && payload.data.result[0];
    if (!result || !result.values) {
      return [];
    }
    return result.values.map((p) => ({ x: Number(p[0]), y: Number(p[1]) }));
  }

  function destroyChart() {
    if (chartInstance) {
      chartInstance.destroy();
      chartInstance = null;
    }
  }

  function buildUPlotData(seriesByMetric, metricOrder) {
    const timeSet = new Set();
    metricOrder.forEach((metric) => {
      const points = seriesByMetric[metric] || [];
      points.forEach((point) => timeSet.add(point.x));
    });

    const x = Array.from(timeSet).sort((a, b) => a - b);
    const data = [x];
    metricOrder.forEach((metric) => {
      const byTs = new Map((seriesByMetric[metric] || []).map((point) => [point.x, point.y]));
      data.push(x.map((ts) => (byTs.has(ts) ? byTs.get(ts) : null)));
    });
    return data;
  }

  function drawChart(seriesByMetric, metricOrder) {
    lastSeriesByMetric = seriesByMetric;
    lastMetricOrder = Array.isArray(metricOrder) ? metricOrder.slice() : [];
    if (typeof uPlot !== "function") {
      throw new Error("uPlot not loaded");
    }

    const labels = lastMetricOrder.length ? lastMetricOrder.slice() : Object.keys(seriesByMetric);
    const data = buildUPlotData(seriesByMetric, labels);
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
    const metrics = selectedMetrics();
    if (!db || metrics.length === 0) {
      cards.innerHTML = "";
      lastSeriesByMetric = {};
      lastMetricOrder = [];
      destroyChart();
      drawChart({}, []);
      syncRefreshControls(false);
      return;
    }

    refreshInFlight = true;
    syncRefreshControls(false);
    setStatus("Refreshing...");
    try {
      await renderLastValues(db, metrics);

      const now = new Date();
      const windowSec = Number(windowSelect.value || "3600");
      const start = new Date(now.getTime() - windowSec * 1000);
      const fromIso = start.toISOString();
      const toIso = now.toISOString();
      const step = stepSelect.value || "30s";

      const seriesResults = await Promise.all(metrics.map((metric) => loadSeries(db, metric, fromIso, toIso, step)));
      const seriesByMetric = {};
      metrics.forEach((metric, idx) => {
        seriesByMetric[metric] = seriesResults[idx];
      });
      drawChart(seriesByMetric, metrics);
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
  addMetricBtn.addEventListener("click", async () => {
    await addMetric(metricInput.value);
  });
  metricInput.addEventListener("keydown", async (ev) => {
    if (ev.key === "Enter") {
      ev.preventDefault();
      await addMetric(metricInput.value);
    }
  });
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
      drawChart(lastSeriesByMetric, lastMetricOrder);
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
