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

  function setStatus(msg) {
    statusEl.textContent = msg;
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
        cards.appendChild(card);
        return;
      }
      const ts = Number(result.value[0]) * 1000;
      card.innerHTML =
        '<div class="metric">' + metric + "</div>" +
        '<div class="value">' + result.value[1] + "</div>" +
        '<div class="ts">' + new Date(ts).toLocaleString() + "</div>";
      cards.appendChild(card);
    });
    await Promise.all(jobs);
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

  function buildUPlotData(seriesByMetric) {
    const timeSet = new Set();
    Object.values(seriesByMetric).forEach((points) => {
      (points || []).forEach((point) => timeSet.add(point.x));
    });

    const x = Array.from(timeSet).sort((a, b) => a - b);
    const data = [x];
    Object.keys(seriesByMetric).forEach((metric) => {
      const byTs = new Map((seriesByMetric[metric] || []).map((point) => [point.x, point.y]));
      data.push(x.map((ts) => (byTs.has(ts) ? byTs.get(ts) : null)));
    });
    return data;
  }

  function drawChart(seriesByMetric) {
    lastSeriesByMetric = seriesByMetric;
    if (typeof uPlot !== "function") {
      throw new Error("uPlot not loaded");
    }

    const data = buildUPlotData(seriesByMetric);
    if (!data[0] || data[0].length === 0) {
      destroyChart();
      chartEl.innerHTML = '<div class="chart-empty">No data in selected range</div>';
      return;
    }

    chartEl.innerHTML = "";
    const labels = Object.keys(seriesByMetric);
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
    const db = dbSelect.value;
    const metrics = selectedMetrics();
    if (!db || metrics.length === 0) {
      cards.innerHTML = "";
      lastSeriesByMetric = {};
      destroyChart();
      drawChart({});
      return;
    }

    setStatus("Refreshing...");
    await renderLastValues(db, metrics);

    const now = new Date();
    const windowSec = Number(windowSelect.value || "3600");
    const start = new Date(now.getTime() - windowSec * 1000);
    const fromIso = start.toISOString();
    const toIso = now.toISOString();
    const step = stepSelect.value || "30s";

    const seriesByMetric = {};
    await Promise.all(
      metrics.map(async (m) => {
        seriesByMetric[m] = await loadSeries(db, m, fromIso, toIso, step);
      })
    );
    drawChart(seriesByMetric);
    setStatus("Updated at " + new Date().toLocaleTimeString());
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
  refreshBtn.addEventListener("click", refreshAll);
  window.addEventListener("resize", () => {
    if (chartInstance) {
      drawChart(lastSeriesByMetric);
    }
  });

  async function init() {
    try {
      await loadDatabases();
      await refreshAll();
      setInterval(refreshAll, Math.max(2, Number(cfg.refreshSeconds || 10)) * 1000);
    } catch (err) {
      setStatus("Failed to load dashboard: " + err.message);
    }
  }

  init();
})();
