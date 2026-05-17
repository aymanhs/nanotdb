(function () {
  function fmtTS(tsSec) {
    return new Date(Number(tsSec) * 1000).toLocaleString();
  }

  async function fetchJSON(url) {
    const res = await fetch(url, { cache: "no-store" });
    if (!res.ok) {
      throw new Error("HTTP " + res.status + " for " + url);
    }
    return res.json();
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

  function formatValue(v, decimals) {
    const n = Number(v);
    if (!Number.isFinite(n)) {
      return String(v);
    }
    const d = Number.isInteger(decimals) ? decimals : 2;
    return n.toFixed(d);
  }

  function drawChart(canvas, seriesMap) {
    const ctx = canvas.getContext("2d");
    const dpr = window.devicePixelRatio || 1;
    const width = canvas.clientWidth || 800;
    const height = canvas.clientHeight || 320;
    canvas.width = Math.floor(width * dpr);
    canvas.height = Math.floor(height * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, width, height);

    const palette = ["#2dd4a4", "#46a3ff", "#f59e0b", "#f472b6", "#a78bfa", "#22d3ee"];

    const all = [];
    Object.values(seriesMap).forEach((arr) => arr.forEach((p) => all.push(p)));
    if (all.length === 0) {
      ctx.fillStyle = "#9fb0c6";
      ctx.fillText("No data", 16, 24);
      return;
    }

    const minX = Math.min.apply(null, all.map((p) => p.x));
    const maxX = Math.max.apply(null, all.map((p) => p.x));
    const minY = Math.min.apply(null, all.map((p) => p.y));
    const maxY = Math.max.apply(null, all.map((p) => p.y));

    const pad = { l: 52, r: 12, t: 12, b: 24 };
    const plotW = width - pad.l - pad.r;
    const plotH = height - pad.t - pad.b;

    const sx = (x) => (maxX === minX ? pad.l + plotW / 2 : pad.l + ((x - minX) / (maxX - minX)) * plotW);
    const sy = (y) => (maxY === minY ? pad.t + plotH / 2 : pad.t + (1 - (y - minY) / (maxY - minY)) * plotH);

    ctx.strokeStyle = "#4c5a73";
    ctx.beginPath();
    ctx.moveTo(pad.l, pad.t);
    ctx.lineTo(pad.l, pad.t + plotH);
    ctx.lineTo(pad.l + plotW, pad.t + plotH);
    ctx.stroke();

    const names = Object.keys(seriesMap);
    names.forEach((name, idx) => {
      const points = seriesMap[name] || [];
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

      const ly = 18 + idx * 14;
      ctx.fillStyle = palette[idx % palette.length];
      ctx.fillRect(width - 200, ly - 7, 9, 9);
      ctx.fillStyle = "#c2cedf";
      ctx.font = "12px sans-serif";
      ctx.fillText(name, width - 186, ly);
    });
  }

  async function fetchLast(db, metric) {
    const payload = await fetchJSON("/api/v1/query?db=" + encodeURIComponent(db) + "&query=" + encodeURIComponent(metric));
    const item = payload.data && payload.data.result && payload.data.result[0];
    if (!item) {
      return null;
    }
    return { ts: item.value[0], value: item.value[1] };
  }

  async function fetchRange(db, metric, lookbackSec, step) {
    const end = new Date();
    const start = new Date(end.getTime() - lookbackSec * 1000);
    const url =
      "/api/v1/query_range?db=" + encodeURIComponent(db) +
      "&query=" + encodeURIComponent(metric) +
      "&start=" + encodeURIComponent(start.toISOString()) +
      "&end=" + encodeURIComponent(end.toISOString()) +
      "&step=" + encodeURIComponent(step || "30s");
    const payload = await fetchJSON(url);
    const item = payload.data && payload.data.result && payload.data.result[0];
    if (!item || !item.values) {
      return [];
    }
    return item.values.map((v) => ({ x: Number(v[0]), y: Number(v[1]) }));
  }

  function widgetHeader(title) {
    const h = document.createElement("h3");
    h.textContent = title;
    return h;
  }

  function widgetFoot() {
    const p = document.createElement("p");
    p.className = "foot";
    p.textContent = "waiting for data";
    return p;
  }

  function mountError(pane, msg) {
    const c = document.createElement("article");
    c.className = "widget widget-error";
    c.appendChild(widgetHeader("Config Error"));
    const p = document.createElement("p");
    p.textContent = msg;
    c.appendChild(p);
    pane.appendChild(c);
  }

  function buildNumberWidget(cfg, pane, dashboardCfg) {
    const card = document.createElement("article");
    card.className = "widget widget-number";
    card.appendChild(widgetHeader(cfg.title || cfg.id));

    const valueEl = document.createElement("p");
    valueEl.className = "value";
    valueEl.textContent = "--";

    const foot = widgetFoot();
    card.appendChild(valueEl);
    card.appendChild(foot);
    pane.appendChild(card);

    const series = (cfg.series || [])[0];
    if (!series) {
      foot.textContent = "missing series";
      return { refresh: async () => {} };
    }

    const db = seriesDB(series, dashboardCfg);
    const metric = seriesMetric(series);
    if (!db || !metric) {
      foot.textContent = "missing db/metric";
      return { refresh: async () => {} };
    }

    return {
      refresh: async () => {
        const point = await fetchLast(db, metric);
        if (!point) {
          valueEl.textContent = "-";
          foot.textContent = "no data";
          return;
        }
        valueEl.textContent = formatValue(point.value, cfg.decimals);
        foot.textContent = fmtTS(point.ts);
      },
    };
  }

  function buildNumbersWidget(cfg, pane, dashboardCfg) {
    const card = document.createElement("article");
    card.className = "widget widget-numbers";
    card.appendChild(widgetHeader(cfg.title || cfg.id));

    const list = document.createElement("div");
    list.className = "numbers-list";
    const rows = [];

    (cfg.series || []).forEach((series, idx) => {
      const row = document.createElement("div");
      row.className = "row";
      const l = document.createElement("span");
      l.className = "label";
      l.textContent = series.label || ("Series " + (idx + 1));
      const v = document.createElement("span");
      v.className = "num";
      v.textContent = "--";
      row.appendChild(l);
      row.appendChild(v);
      list.appendChild(row);
      rows.push({ series, value: v });
    });

    const foot = widgetFoot();
    card.appendChild(list);
    card.appendChild(foot);
    pane.appendChild(card);

    return {
      refresh: async () => {
        await Promise.all(rows.map(async (r) => {
          const db = seriesDB(r.series, dashboardCfg);
          const metric = seriesMetric(r.series);
          if (!db || !metric) {
            r.value.textContent = "cfg";
            return;
          }
          const point = await fetchLast(db, metric);
          r.value.textContent = point ? formatValue(point.value, cfg.decimals) : "-";
        }));
        foot.textContent = "updated " + new Date().toLocaleTimeString();
      },
    };
  }

  function buildChartWidget(cfg, pane, dashboardCfg) {
    const card = document.createElement("article");
    card.className = "widget widget-chart";
    card.appendChild(widgetHeader(cfg.title || cfg.id));

    const canvas = document.createElement("canvas");
    canvas.className = "chart";
    const foot = widgetFoot();

    card.appendChild(canvas);
    card.appendChild(foot);
    pane.appendChild(card);

    return {
      refresh: async () => {
        const lookbackSec = parseDurationSeconds(cfg.lookback || "1h", 3600);
        const step = cfg.interval || "30s";
        const seriesMap = {};

        await Promise.all((cfg.series || []).map(async (s, idx) => {
          const db = seriesDB(s, dashboardCfg);
          const metric = seriesMetric(s);
          if (!db || !metric) {
            return;
          }
          const key = s.label || metric || ("Series " + (idx + 1));
          seriesMap[key] = await fetchRange(db, metric, lookbackSec, step);
        }));

        drawChart(canvas, seriesMap);
        foot.textContent = "updated " + new Date().toLocaleTimeString();
      },
    };
  }

  function buildWidget(widget, pane, dashboardCfg) {
    if (widget.type === "number") return buildNumberWidget(widget, pane, dashboardCfg);
    if (widget.type === "numbers") return buildNumbersWidget(widget, pane, dashboardCfg);
    if (widget.type === "line_chart") return buildChartWidget(widget, pane, dashboardCfg);
    mountError(pane, "unsupported widget type: " + widget.type);
    return { refresh: async () => {} };
  }

  async function loadDashboard() {
    const cfg = await fetchJSON("/api/dashboard-config");
    document.getElementById("dashboard-title").textContent = cfg.title || "Dashboard";

    const nav = document.getElementById("group-nav");
    const widgetsHost = document.getElementById("widgets");
    nav.innerHTML = "";
    widgetsHost.innerHTML = "";

    const groups = cfg.groups || [];
    const widgets = cfg.widgets || {};
    if (!groups.length) {
      mountError(widgetsHost, "No groups configured");
      return;
    }

    const paneById = new Map();
    const refreshersById = new Map();

    function activate(groupId) {
      paneById.forEach((pane, id) => {
        pane.hidden = id !== groupId;
      });
      nav.querySelectorAll(".group-tab").forEach((tab) => {
        tab.classList.toggle("active", tab.dataset.groupId === groupId);
      });
    }

    for (const group of groups) {
      const tab = document.createElement("button");
      tab.type = "button";
      tab.className = "group-tab";
      tab.dataset.groupId = group.id;
      tab.textContent = group.label || group.id;
      nav.appendChild(tab);

      const pane = document.createElement("section");
      pane.className = "group-pane";
      pane.dataset.groupId = group.id;
      pane.hidden = true;
      widgetsHost.appendChild(pane);
      paneById.set(group.id, pane);

      const refreshers = [];
      for (const wid of group.widgets || []) {
        const w = widgets[wid];
        if (!w) {
          mountError(pane, "unknown widget id: " + wid);
          continue;
        }
        const widget = Object.assign({}, w, { id: w.id || wid });
        const refresher = buildWidget(widget, pane, cfg);
        refreshers.push({
          run: refresher.refresh,
          refreshMs: Math.max(5000, (widget.refresh_sec || 15) * 1000),
          timerId: null,
        });
      }
      refreshersById.set(group.id, refreshers);

      tab.addEventListener("click", async () => {
        activate(group.id);
        const list = refreshersById.get(group.id) || [];
        await Promise.all(list.map((r) => r.run()));
      });
    }

    const first = groups[0].id;
    activate(first);
    const firstRefreshers = refreshersById.get(first) || [];
    await Promise.all(firstRefreshers.map((r) => r.run()));

    refreshersById.forEach((list, groupId) => {
      list.forEach((r) => {
        r.timerId = setInterval(async () => {
          const pane = paneById.get(groupId);
          if (pane && !pane.hidden) {
            await r.run();
          }
        }, r.refreshMs);
      });
    });
  }

  loadDashboard().catch((err) => {
    const host = document.getElementById("widgets");
    host.innerHTML = "";
    mountError(host, err.message || String(err));
  });
})();
