(function () {
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

  function seriesQuery(series) {
    if (series && typeof series.query === "string" && series.query.trim()) {
      return series.query.trim();
    }
    if (series && series.metric) {
      return series.metric;
    }
    if (series && series.measurement && series.field) {
      return series.measurement + "." + series.field;
    }
    return "";
  }

  function seriesMetric(series) {
    return seriesQuery(series);
  }

  function seriesAggregate(series) {
    return series && typeof series.aggregate === "string" ? series.aggregate.trim() : "";
  }

  function seriesWindow(series) {
    return series && typeof series.window === "string" ? series.window.trim() : "";
  }

  function seriesUsesAggregateRange(series) {
    return Boolean(seriesAggregate(series) && seriesWindow(series));
  }

  function effectiveSeriesLabel(series, idx) {
    if (series && typeof series.label === "string" && series.label.trim()) {
      return series.label.trim();
    }
    if (series && typeof series.role === "string" && series.role.trim()) {
      const role = series.role.trim();
      if (role === "avg") return "Avg";
      if (role === "min") return "Min";
      if (role === "max") return "Max";
      return role;
    }
    const query = seriesQuery(series);
    if (query) {
      const aggregate = seriesAggregate(series);
      const window = seriesWindow(series);
      if (aggregate && window) {
        return query + " [" + aggregate + " " + window + "]";
      }
      return query;
    }
    return "Series " + (idx + 1);
  }

  function buildInstantQueryPath(db, series) {
    const query = seriesQuery(series);
    if (!db || !query || seriesUsesAggregateRange(series)) {
      return "";
    }
    return "/api/v1/query?db=" + encodeURIComponent(db) + "&query=" + encodeURIComponent(query);
  }

  function buildRangeQueryPath(db, series, startISO, endISO, step) {
    const query = seriesQuery(series);
    if (!db || !query || !startISO) {
      return "";
    }
    let path = "/api/v1/query_range?db=" + encodeURIComponent(db) +
      "&query=" + encodeURIComponent(query) +
      "&start=" + encodeURIComponent(startISO);
    if (endISO) {
      path += "&end=" + encodeURIComponent(endISO);
    }
    if (seriesUsesAggregateRange(series)) {
      path += "&aggregate=" + encodeURIComponent(seriesAggregate(series));
      path += "&window=" + encodeURIComponent(seriesWindow(series));
      return path;
    }
    if (step) {
      path += "&step=" + encodeURIComponent(step);
    }
    return path;
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
    const display = resolveDisplayConfig(target);
    return n * display.factor + display.offset;
  }

  function trimTrailingZeros(text) {
    return String(text).replace(/(\.\d*?[1-9])0+$/u, "$1").replace(/\.0+$/u, "");
  }

  function formatNumericValue(value, precision) {
    return trimTrailingZeros(Number(value).toFixed(Number.isInteger(precision) && precision >= 0 ? precision : 0));
  }

  function formatDurationFromSeconds(value) {
    const total = Math.max(0, Math.floor(Number(value)));
    if (!Number.isFinite(total)) {
      return "--";
    }
    const days = Math.floor(total / 86400);
    const hours = Math.floor((total % 86400) / 3600);
    const mins = Math.floor((total % 3600) / 60);
    const secs = total % 60;
    if (days > 0) return days + "d " + hours + "h " + mins + "m";
    if (hours > 0) return hours + "h " + mins + "m" + (secs > 0 ? " " + secs + "s" : "");
    if (mins > 0) return mins + "m" + (secs > 0 ? " " + secs + "s" : "");
    return secs + "s";
  }

  function formatWidgetValue(target, rawValue) {
    const display = resolveDisplayConfig(target);
    const value = transformValue(target, rawValue);
    if (!Number.isFinite(value)) {
      return "--";
    }
    const fixed = formatNumericValue(value, display.decimals);
    if (display.format) {
      if (display.format.includes("{duration}")) {
        return display.format.replaceAll("{duration}", formatDurationFromSeconds(value));
      }
      if (display.format.includes("{value}")) {
        return display.format.replaceAll("{value}", fixed);
      }
    }
    return display.unit ? fixed + display.unit : fixed;
  }

  function formatTransformedValue(target, transformedValue) {
    const display = resolveDisplayConfig(target);
    if (!Number.isFinite(transformedValue)) {
      return "--";
    }
    const fixed = formatNumericValue(transformedValue, display.decimals);
    if (display.format) {
      if (display.format.includes("{duration}")) {
        return display.format.replaceAll("{duration}", formatDurationFromSeconds(transformedValue));
      }
      if (display.format.includes("{value}")) {
        return display.format.replaceAll("{value}", fixed);
      }
    }
    return display.unit ? fixed + display.unit : fixed;
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
    const direction = thresholds.direction === "below" ? "below" : "above";
    const hasWarning = Number.isFinite(thresholds.warning);
    const hasCritical = Number.isFinite(thresholds.critical);
    if (!hasWarning && !hasCritical) {
      return "none";
    }
    if (direction === "above") {
      if (hasCritical && value >= thresholds.critical) return "critical";
      if (hasWarning && value >= thresholds.warning) return "warning";
      return "normal";
    }
    if (hasCritical && value <= thresholds.critical) return "critical";
    if (hasWarning && value <= thresholds.warning) return "warning";
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
      const label = value == null ? "" : formatTransformedValue(target, value);
      maxLen = Math.max(maxLen, String(label).length);
    });
    const isMobile = window.matchMedia("(max-width: 699px)").matches;
    const minSize = isMobile ? 36 : 64;
    const maxSize = isMobile ? 56 : 220;
    const charWidth = isMobile ? 6 : 8;
    return Math.min(maxSize, Math.max(minSize, maxLen * charWidth + 12));
  }

  function buildUPlotData(seriesMap) {
    const seriesItems = Array.isArray(seriesMap)
      ? seriesMap
      : Object.keys(seriesMap || {}).map((label) => ({ label, points: seriesMap[label] || [] }));
    const timeSet = new Set();
    seriesItems.forEach((item) => {
      (item && item.points ? item.points : []).forEach((point) => timeSet.add(point.x));
    });
    const x = Array.from(timeSet).sort((a, b) => a - b);
    const data = [x];
    seriesItems.forEach((item) => {
      const byTs = new Map(((item && item.points) || []).map((point) => [point.x, point.y]));
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

  // ---------------------------------------------------------------------
  // Event overlay helpers — shared between the dashboard and the explorer.
  //
  // An "overlay" is a layer of vertical markers drawn on a uPlot
  // line chart at the timestamps of matching events. Each layer is
  // {db, event_name_pattern, color, label, event_limit}, and the
  // renderer turns it into {markers: [{x, ts, name, value, payload}]}
  // by calling fetchEventOverlayMarkers below.
  //
  // The drawing path (eventOverlayHooks) is now shared so both the
  // dashboard's saved widgets and the explorer's ad-hoc charts use
  // the same rendering + hover behavior.
  // ---------------------------------------------------------------------

  function isValidCssColorBasic(value) {
    if (!value || typeof value !== "string") {
      return false;
    }
    const v = value.trim();
    if (v.length === 0) {
      return false;
    }
    if (v[0] === "#") {
      const hex = v.slice(1);
      if (hex.length !== 3 && hex.length !== 4 && hex.length !== 6 && hex.length !== 8) {
        return false;
      }
      return /^[0-9a-fA-F]+$/.test(hex);
    }
    return v.length <= 32 && /^[A-Za-z]+$/.test(v);
  }

  function overlayDefaultColor(key) {
    const palette = ["#fb923c", "#a78bfa", "#22d3ee", "#f87171", "#f472b6", "#facc15"];
    let h = 0;
    const s = String(key || "");
    for (let i = 0; i < s.length; i++) {
      h = (h * 31 + s.charCodeAt(i)) >>> 0;
    }
    return palette[h % palette.length];
  }

  function formatEventValueForOverlay(evt) {
    if (!evt) return "";
    if (typeof evt.int32 === "number") return String(evt.int32);
    if (typeof evt.float32 === "number") return String(evt.float32);
    if (evt.value !== undefined && evt.value !== null) return String(evt.value);
    return "";
  }

  function formatEventPayloadForOverlay(payload) {
    if (!payload || typeof payload !== "object") return "";
    const entries = Object.entries(payload).slice(0, 3);
    if (entries.length === 0) return "";
    const summary = entries.map(([k, v]) => {
      let val = v;
      if (typeof v === "object") {
        try { val = JSON.stringify(v); } catch (e) { val = String(v); }
      }
      return k + "=" + String(val);
    }).join(" ");
    return summary.length > 80 ? summary.slice(0, 77) + "..." : summary;
  }

  // mergeUPlotHooks composes hook objects without losing arrays from
  // either side. Hooks like draw/setCursor can come from multiple
  // sources (aggregate bands, overlays); concat rather than overwrite.
  function mergeUPlotHooks(a, b) {
    const out = {};
    const keys = new Set(Object.keys(a || {}).concat(Object.keys(b || {})));
    keys.forEach((k) => {
      const aHooks = (a && a[k]) || [];
      const bHooks = (b && b[k]) || [];
      out[k] = aHooks.concat(bHooks);
    });
    return out;
  }

  // ensureOverlayTooltip attaches a single hover-tooltip element to
  // the chart's parent the first time it's needed. Subsequent
  // overlays on the same chart reuse it.
  function ensureOverlayTooltip(u) {
    const parent = u.root && u.root.parentNode;
    if (!parent) return null;
    let tip = parent.querySelector(".overlay-tooltip");
    if (!tip) {
      tip = document.createElement("div");
      tip.className = "overlay-tooltip";
      tip.style.position = "absolute";
      tip.style.pointerEvents = "none";
      tip.style.display = "none";
      tip.style.zIndex = "5";
      // Keep position:relative on parent so absolute positioning
      // lands inside the chart card. uPlot's own root is positioned;
      // its parent usually is not.
      const cs = window.getComputedStyle(parent);
      if (cs.position === "static") {
        parent.style.position = "relative";
      }
      parent.appendChild(tip);
    }
    return tip;
  }

  // eventOverlayHooks returns uPlot hook callbacks that:
  //   - draw a vertical dashed marker per event in each overlay
  //     layer (the existing behavior), and
  //   - on cursor movement, hit-test against every marker within a
  //     hoverPx threshold and pop a tooltip showing
  //     {name, time, value, payload-snippet}.
  //
  // The tooltip is a single shared DOM node per chart, created
  // lazily by ensureOverlayTooltip.
  function eventOverlayHooks(overlays) {
    const hoverPx = 6; // marker hit-test radius in pixels

    return {
      draw: [
        (u) => {
          const ctx = u.ctx;
          const plotTop = u.bbox.top;
          const plotBottom = u.bbox.top + u.bbox.height;
          ctx.save();
          for (const layer of overlays) {
            const stroke = isValidCssColorBasic(layer.color) ? layer.color : overlayDefaultColor(layer.label);
            ctx.strokeStyle = stroke;
            ctx.lineWidth = 1;
            ctx.setLineDash([4, 3]);
            for (const m of (layer.markers || [])) {
              const xPx = u.valToPos(m.x, "x", true);
              if (!Number.isFinite(xPx) || xPx < u.bbox.left || xPx > u.bbox.left + u.bbox.width) {
                continue;
              }
              ctx.beginPath();
              ctx.moveTo(Math.round(xPx) + 0.5, plotTop);
              ctx.lineTo(Math.round(xPx) + 0.5, plotBottom);
              ctx.stroke();
            }
          }
          ctx.restore();
        },
      ],
      setCursor: [
        (u) => {
          const tip = ensureOverlayTooltip(u);
          if (!tip) return;
          const cx = u.cursor && u.cursor.left;
          const cy = u.cursor && u.cursor.top;
          if (cx == null || cx < 0) {
            tip.style.display = "none";
            return;
          }
          // Find the marker (across all layers) whose x position is
          // closest to the cursor, within the hover threshold.
          // Markers from later layers win on ties; ordering is
          // overlay-author-determined.
          let best = null;
          let bestDist = hoverPx + 1;
          let bestStroke = "";
          let bestLabel = "";
          for (const layer of overlays) {
            const stroke = isValidCssColorBasic(layer.color) ? layer.color : overlayDefaultColor(layer.label);
            for (const m of (layer.markers || [])) {
              const xPx = u.valToPos(m.x, "x", true);
              if (!Number.isFinite(xPx)) continue;
              const dist = Math.abs(xPx - (cx + u.bbox.left));
              if (dist <= bestDist) {
                best = m;
                bestDist = dist;
                bestStroke = stroke;
                bestLabel = layer.label || layer.event_name_pattern || "";
              }
            }
          }
          if (!best) {
            tip.style.display = "none";
            return;
          }
          const timeText = best.ts ? new Date(best.ts / 1e6).toLocaleString() : "";
          const valueText = best.valueText || formatEventValueForOverlay(best);
          const payloadText = formatEventPayloadForOverlay(best.payload);
          const swatch = '<span class="overlay-tooltip-swatch" style="background:' + bestStroke + '"></span>';
          tip.innerHTML =
            '<div class="overlay-tooltip-head">' + swatch +
              '<span class="overlay-tooltip-name">' + escapeBasic(best.name || bestLabel) + '</span>' +
            '</div>' +
            (timeText ? '<div class="overlay-tooltip-row">' + escapeBasic(timeText) + '</div>' : '') +
            (valueText ? '<div class="overlay-tooltip-row">value: ' + escapeBasic(valueText) + '</div>' : '') +
            (payloadText ? '<div class="overlay-tooltip-row overlay-tooltip-payload">' + escapeBasic(payloadText) + '</div>' : '');
          // Position to the right of the cursor by default; flip
          // when near the right edge so the tooltip stays visible.
          const tipWidth = 240;
          const cursorAbsX = cx + u.bbox.left;
          let leftPx = cursorAbsX + 10;
          if (cursorAbsX + tipWidth > u.bbox.left + u.bbox.width) {
            leftPx = cursorAbsX - tipWidth - 10;
          }
          tip.style.left = leftPx + "px";
          tip.style.top = (cy + u.bbox.top - 10) + "px";
          tip.style.display = "block";
        },
      ],
    };
  }

  // escapeBasic is a tiny safe-text escaper for tooltip content. We
  // do not load a templating layer in the static pages, so this is
  // adequate for the small fixed set of fields rendered above.
  function escapeBasic(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  // fetchEventOverlayMarkers takes a {db, event_name_pattern, ...}
  // descriptor and returns an array of markers in the shape the draw
  // hook expects. Used by both dashboard widgets and the explorer.
  async function fetchEventOverlayMarkers(apiBase, db, overlay, lookbackSec) {
    const pattern = overlay && (overlay.event_name_pattern || "").trim();
    if (!db || !pattern) {
      return [];
    }
    const end = new Date();
    const start = new Date(end.getTime() - lookbackSec * 1000);
    const limit = overlay && overlay.event_limit
      ? Math.max(1, Math.min(1000, Number(overlay.event_limit) || 200))
      : 200;
    const base = typeof apiBase === "string" ? apiBase.replace(/\/$/, "") : "";
    const url = base +
      "/api/v1/events?db=" + encodeURIComponent(db) +
      "&name=" + encodeURIComponent(pattern) +
      "&start=" + encodeURIComponent(start.toISOString()) +
      "&end=" + encodeURIComponent(end.toISOString()) +
      "&limit=" + limit;
    const res = await fetch(url, { cache: "no-store" });
    if (!res.ok) {
      throw new Error("HTTP " + res.status + " for " + url);
    }
    const payload = await res.json();
    const events = (payload.data && payload.data.result) ? payload.data.result : [];
    const out = [];
    for (const evt of events) {
      const ts = Number(evt && evt.ts);
      if (!Number.isFinite(ts)) {
        continue;
      }
      out.push({
        x: ts / 1e9,
        ts,
        name: evt.name || "",
        int32: typeof evt.int32 === "number" ? evt.int32 : undefined,
        float32: typeof evt.float32 === "number" ? evt.float32 : undefined,
        value: evt.value !== undefined ? evt.value : undefined,
        valueText: formatEventValueForOverlay(evt),
        payload: evt.payload || null,
        color: overlay.color || "",
        label: overlay.label || pattern,
      });
    }
    return out;
  }

  window.NANOTDB_UTILS = {
    parseDurationSeconds,
    seriesDB,
    seriesQuery,
    seriesMetric,
    seriesAggregate,
    seriesWindow,
    seriesUsesAggregateRange,
    effectiveSeriesLabel,
    buildInstantQueryPath,
    buildRangeQueryPath,
    resolveDisplayConfig,
    transformValue,
    trimTrailingZeros,
    formatNumericValue,
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
    rebalanceSingleNumberRows,
    // Overlay helpers, also exported so the dashboard, explorer, and
    // editor preview all share one implementation.
    isValidCssColorBasic,
    overlayDefaultColor,
    mergeUPlotHooks,
    eventOverlayHooks,
    fetchEventOverlayMarkers,
    formatEventValueForOverlay,
    formatEventPayloadForOverlay
  };
})();
