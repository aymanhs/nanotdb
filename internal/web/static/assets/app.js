(function () {
  const cfg = window.NANOTDB_DASH_CONFIG || { basePath: "/dashboard", refreshSeconds: 10 };

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
  const canvas = document.getElementById("chart");
  const ctx = canvas.getContext("2d");

  const palette = ["#2dd4a4", "#46a3ff", "#f59e0b", "#f472b6", "#a78bfa", "#22d3ee"];

  const css = getComputedStyle(document.documentElement);
  const chartGrid = css.getPropertyValue("--chart-grid").trim() || "#4c5a73";
  const chartText = css.getPropertyValue("--chart-text").trim() || "#b9c6d9";
  let metricCatalog = [];
  let selectedMetricNames = [];

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
    const payload = await fetchJSON("/api/v1/databases");
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
    const payload = await fetchJSON("/api/v1/metrics?db=" + encodeURIComponent(db));
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
        "/api/v1/query?db=" + encodeURIComponent(db) + "&query=" + encodeURIComponent(metric)
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
    const url =
      "/api/v1/query_range?db=" + encodeURIComponent(db) +
      "&query=" + encodeURIComponent(metric) +
      "&start=" + encodeURIComponent(fromIso) +
      "&end=" + encodeURIComponent(toIso) +
      "&step=" + encodeURIComponent(step);
    const payload = await fetchJSON(url);
    const result = payload.data && payload.data.result && payload.data.result[0];
    if (!result || !result.values) {
      return [];
    }
    return result.values.map((p) => ({ x: Number(p[0]), y: Number(p[1]) }));
  }

  function drawChart(seriesByMetric) {
    const dpr = window.devicePixelRatio || 1;
    const width = canvas.clientWidth || 800;
    const height = canvas.clientHeight || 420;
    canvas.width = Math.floor(width * dpr);
    canvas.height = Math.floor(height * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);

    ctx.clearRect(0, 0, width, height);

    const all = [];
    Object.values(seriesByMetric).forEach((arr) => arr.forEach((p) => all.push(p)));
    if (all.length === 0) {
      ctx.fillStyle = chartText;
      ctx.fillText("No data in selected range", 20, 30);
      return;
    }

    const minX = Math.min(...all.map((p) => p.x));
    const maxX = Math.max(...all.map((p) => p.x));
    const minY = Math.min(...all.map((p) => p.y));
    const maxY = Math.max(...all.map((p) => p.y));

    const pad = { l: 52, r: 12, t: 12, b: 24 };
    const plotW = width - pad.l - pad.r;
    const plotH = height - pad.t - pad.b;

    function sx(x) {
      if (maxX === minX) return pad.l + plotW / 2;
      return pad.l + ((x - minX) / (maxX - minX)) * plotW;
    }
    function sy(y) {
      if (maxY === minY) return pad.t + plotH / 2;
      return pad.t + (1 - (y - minY) / (maxY - minY)) * plotH;
    }

    ctx.strokeStyle = chartGrid;
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(pad.l, pad.t);
    ctx.lineTo(pad.l, pad.t + plotH);
    ctx.lineTo(pad.l + plotW, pad.t + plotH);
    ctx.stroke();

    const metrics = Object.keys(seriesByMetric);
    metrics.forEach((metric, idx) => {
      const points = seriesByMetric[metric];
      if (!points.length) return;
      ctx.strokeStyle = palette[idx % palette.length];
      ctx.lineWidth = 2;
      ctx.beginPath();
      points.forEach((p, i) => {
        const x = sx(p.x);
        const y = sy(p.y);
        if (i === 0) ctx.moveTo(x, y);
        else ctx.lineTo(x, y);
      });
      ctx.stroke();

      const legendY = 18 + idx * 16;
      ctx.fillStyle = palette[idx % palette.length];
      ctx.fillRect(width - 190, legendY - 8, 10, 10);
      ctx.fillStyle = chartText;
      ctx.font = "12px sans-serif";
      ctx.fillText(metric, width - 176, legendY);
    });

    ctx.fillStyle = chartText;
    ctx.font = "12px sans-serif";
    ctx.fillText(minY.toFixed(2), 6, pad.t + plotH);
    ctx.fillText(maxY.toFixed(2), 6, pad.t + 10);
  }

  async function refreshAll() {
    const db = dbSelect.value;
    const metrics = selectedMetrics();
    if (!db || metrics.length === 0) {
      cards.innerHTML = "";
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
