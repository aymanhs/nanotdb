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
      const label = value == null ? "" : formatWidgetValue(target, value);
      maxLen = Math.max(maxLen, String(label).length);
    });
    const isMobile = window.matchMedia("(max-width: 699px)").matches;
    const minSize = isMobile ? 36 : 64;
    const maxSize = isMobile ? 84 : 220;
    return Math.min(maxSize, Math.max(minSize, maxLen * 8 + 16));
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
    classifySeverity,
    applySeverityClass,
    pickSeriesColor,
    chartTheme,
    yAxisSizeForValues,
    buildUPlotData,
    chartDisplayTarget,
    rebalanceSingleNumberRows
  };
})();
