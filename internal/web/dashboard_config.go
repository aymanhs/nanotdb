package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type DashboardConfigDocument struct {
	Title     string                     `json:"title"`
	DefaultDB string                     `json:"default_db,omitempty"`
	Groups    []DashboardGroup           `json:"groups"`
	Widgets   map[string]DashboardWidget `json:"widgets"`
}

type DashboardGroup struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Widgets []string `json:"widgets,omitempty"`
}

type DashboardWidget struct {
	Type         string            `json:"type"`
	Title        string            `json:"title,omitempty"`
	RefreshSec   int               `json:"refresh_sec,omitempty"`
	AutoRefresh  *bool             `json:"auto_refresh,omitempty"`
	Lookback     string            `json:"lookback,omitempty"`
	Interval     string            `json:"interval,omitempty"`
	Presentation string            `json:"presentation,omitempty"`
	Series       []DashboardSeries `json:"series,omitempty"`

	// EventOverlays renders vertical markers at event timestamps on top
	// of a line_chart's metric series. Each overlay is queried
	// independently via GET /api/v1/events; the hover surface carries
	// the event name, timestamp, value (if any), and payload. Only
	// valid on line_chart widgets.
	EventOverlays []DashboardEventOverlay `json:"event_overlays,omitempty"`
}

// DashboardEventOverlay configures one vertical-marker layer over a
// metric chart. event_name_pattern accepts the same exact-or-wildcard
// shape as event_log series. db falls back to the dashboard's
// default_db. color is a CSS-color string (hex like "#c00" or named
// like "red"); when empty the renderer picks a default.
type DashboardEventOverlay struct {
	Label            string `json:"label,omitempty"`
	DB               string `json:"db,omitempty"`
	Database         string `json:"database,omitempty"`
	EventNamePattern string `json:"event_name_pattern,omitempty"`
	Color            string `json:"color,omitempty"`
	EventLimit       int    `json:"event_limit,omitempty"`
}

type DashboardSeries struct {
	Label            string               `json:"label,omitempty"`
	Role             string               `json:"role,omitempty"`
	DB               string               `json:"db,omitempty"`
	Database         string               `json:"database,omitempty"`
	Query            string               `json:"query,omitempty"`
	Metric           string               `json:"metric,omitempty"`
	Measurement      string               `json:"measurement,omitempty"`
	Field            string               `json:"field,omitempty"`
	Aggregate        string               `json:"aggregate,omitempty"`
	Window           string               `json:"window,omitempty"`
	EventNamePattern string               `json:"event_name_pattern,omitempty"`
	EventLimit       int                  `json:"event_limit,omitempty"`
	Transform        *DashboardTransform  `json:"transform,omitempty"`
	Thresholds       *DashboardThresholds `json:"thresholds,omitempty"`
	Scale            *float64             `json:"scale,omitempty"`
	Offset           *float64             `json:"offset,omitempty"`
	Unit             string               `json:"unit,omitempty"`
	Decimals         *int                 `json:"decimals,omitempty"`
	Format           string               `json:"format,omitempty"`
}

type DashboardTransform struct {
	Factor   *float64 `json:"factor,omitempty"`
	Offset   *float64 `json:"offset,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Decimals *int     `json:"decimals,omitempty"`
	Format   string   `json:"format,omitempty"`
}

type DashboardThresholds struct {
	Direction string   `json:"direction,omitempty"`
	Warning   *float64 `json:"warning,omitempty"`
	Critical  *float64 `json:"critical,omitempty"`
}

type dashboardMutationResponse struct {
	OK         bool                     `json:"ok"`
	Errors     []string                 `json:"errors,omitempty"`
	BackupPath string                   `json:"backup_path,omitempty"`
	Config     *DashboardConfigDocument `json:"config,omitempty"`
	Payload    json.RawMessage          `json:"payload,omitempty"`
}

func readDashboardConfigRequest(req *http.Request) (DashboardConfigDocument, []string) {
	defer req.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		return DashboardConfigDocument{}, []string{"failed to read request body"}
	}
	var cfg DashboardConfigDocument
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return DashboardConfigDocument{}, []string{"invalid dashboard config JSON: " + err.Error()}
	}
	normalizeDashboardConfig(&cfg)
	if errs := validateDashboardConfig(cfg); len(errs) > 0 {
		return DashboardConfigDocument{}, errs
	}
	return cfg, nil
}

func normalizeDashboardConfig(cfg *DashboardConfigDocument) {
	if cfg.Groups == nil {
		cfg.Groups = []DashboardGroup{}
	}
	if cfg.Widgets == nil {
		cfg.Widgets = map[string]DashboardWidget{}
	}
	for idx, group := range cfg.Groups {
		if group.Widgets == nil {
			group.Widgets = []string{}
		}
		cfg.Groups[idx] = group
	}
	for id, widget := range cfg.Widgets {
		if widget.Series == nil {
			widget.Series = []DashboardSeries{}
		}
		for idx, series := range widget.Series {
			series.Window = effectiveDashboardSeriesAggregateWindow(widget, series)
			widget.Series[idx] = series
		}
		cfg.Widgets[id] = widget
	}
}

func effectiveDashboardWidgetType(widget DashboardWidget) string {
	widgetType := strings.TrimSpace(widget.Type)
	if widgetType == "line_chart" && strings.TrimSpace(widget.Presentation) == "aggregate_band" {
		return "aggregate_band"
	}
	return widgetType
}

func validateDashboardConfig(cfg DashboardConfigDocument) []string {
	var errs []string
	if strings.TrimSpace(cfg.Title) == "" {
		errs = append(errs, "title is required")
	}
	if len(cfg.Groups) == 0 {
		errs = append(errs, "at least one group is required")
	}

	groupIDs := make(map[string]struct{}, len(cfg.Groups))
	for idx, group := range cfg.Groups {
		id := strings.TrimSpace(group.ID)
		if id == "" {
			errs = append(errs, fmt.Sprintf("groups[%d].id is required", idx))
			continue
		}
		if _, exists := groupIDs[id]; exists {
			errs = append(errs, fmt.Sprintf("duplicate group id %q", id))
			continue
		}
		groupIDs[id] = struct{}{}
	}

	widgetIDs := make([]string, 0, len(cfg.Widgets))
	for widgetID := range cfg.Widgets {
		widgetIDs = append(widgetIDs, widgetID)
	}
	sort.Strings(widgetIDs)
	for _, widgetID := range widgetIDs {
		if strings.TrimSpace(widgetID) == "" {
			errs = append(errs, "widgets map contains an empty widget id")
			continue
		}
		widget := cfg.Widgets[widgetID]
		lineChartLabels := make(map[string]int)
		widgetType := effectiveDashboardWidgetType(widget)
		switch widgetType {
		case "number", "numbers":
			if len(widget.Series) == 0 {
				errs = append(errs, fmt.Sprintf("widget %q must define at least one series", widgetID))
			}
		case "line_chart", "aggregate_band":
			if len(widget.Series) == 0 {
				errs = append(errs, fmt.Sprintf("widget %q must define at least one series", widgetID))
			}
			if widgetType == "line_chart" && strings.TrimSpace(widget.Presentation) != "" && strings.TrimSpace(widget.Presentation) != "aggregate_band" {
				errs = append(errs, fmt.Sprintf("widget %q has unsupported presentation %q", widgetID, strings.TrimSpace(widget.Presentation)))
			}
			if !isValidDashboardDuration(widget.Lookback) {
				errs = append(errs, fmt.Sprintf("widget %q has invalid lookback %q", widgetID, widget.Lookback))
			}
			if !isValidDashboardDuration(widget.Interval) {
				errs = append(errs, fmt.Sprintf("widget %q has invalid interval %q", widgetID, widget.Interval))
			}
		case "event_log":
			if len(widget.Series) == 0 {
				errs = append(errs, fmt.Sprintf("widget %q must define at least one series", widgetID))
			}
			if !isValidDashboardDuration(widget.Lookback) {
				errs = append(errs, fmt.Sprintf("widget %q has invalid lookback %q", widgetID, widget.Lookback))
			}
		default:
			errs = append(errs, fmt.Sprintf("widget %q has unsupported type %q", widgetID, widget.Type))
		}
		if widget.RefreshSec < 0 {
			errs = append(errs, fmt.Sprintf("widget %q has invalid refresh_sec %d", widgetID, widget.RefreshSec))
		}

		// event_overlays are only meaningful on line_chart. They live at
		// the widget level (one query per overlay) so they can be
		// composited over any number of metric series in the same chart.
		if len(widget.EventOverlays) > 0 {
			if widgetType != "line_chart" {
				errs = append(errs, fmt.Sprintf("widget %q event_overlays only supported on line_chart widgets", widgetID))
			}
			seenOverlayLabels := make(map[string]int, len(widget.EventOverlays))
			for oi, ov := range widget.EventOverlays {
				pattern := strings.TrimSpace(ov.EventNamePattern)
				if pattern == "" {
					errs = append(errs, fmt.Sprintf("widget %q event_overlays[%d] must define event_name_pattern", widgetID, oi))
				}
				if ov.EventLimit < 0 {
					errs = append(errs, fmt.Sprintf("widget %q event_overlays[%d] has invalid event_limit %d", widgetID, oi, ov.EventLimit))
				}
				if color := strings.TrimSpace(ov.Color); color != "" && !isValidOverlayColor(color) {
					errs = append(errs, fmt.Sprintf("widget %q event_overlays[%d] has invalid color %q (use a hex like \"#c00\" or a CSS named color)", widgetID, oi, color))
				}
				label := strings.TrimSpace(ov.Label)
				if label == "" {
					label = pattern
				}
				if prev, ok := seenOverlayLabels[label]; ok && label != "" {
					errs = append(errs, fmt.Sprintf("widget %q event_overlays has duplicate label %q at [%d] and [%d]", widgetID, label, prev, oi))
				} else if label != "" {
					seenOverlayLabels[label] = oi
				}
			}
		}

		aggregateBandRoles := make(map[string]int)
		aggregateBandKey := ""
		aggregateBandShortcut := usesAggregateBandShortcut(widget)
		for idx, series := range widget.Series {
			query := strings.TrimSpace(series.Query)
			metric := strings.TrimSpace(series.Metric)
			measurement := strings.TrimSpace(series.Measurement)
			field := strings.TrimSpace(series.Field)
			aggregate := strings.TrimSpace(series.Aggregate)
			window := effectiveDashboardSeriesAggregateWindow(widget, series)
			eventNamePattern := strings.TrimSpace(series.EventNamePattern)

			seriesIsEventBacked := eventNamePattern != ""
			switch {
			case widgetType == "event_log":
				// Event_log series must be event-backed.
				if !seriesIsEventBacked {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] must define event_name_pattern", widgetID, idx))
				}
				if query != "" || metric != "" || measurement != "" || field != "" {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] cannot mix event_name_pattern with metric fields (query, metric, measurement, field)", widgetID, idx))
				}
			case widgetType == "line_chart" && seriesIsEventBacked:
				// Event-backed line-chart series: plot each event as one
				// scatter point. Cannot mix with metric fields. Aggregate
				// + window are not supported on event-backed series in
				// v1 (numeric-aggregate-over-events is designed but not
				// built — see EVENTS.md).
				if query != "" || metric != "" || measurement != "" || field != "" {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] cannot mix event_name_pattern with metric fields (query, metric, measurement, field)", widgetID, idx))
				}
				if aggregate != "" || window != "" {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] event-backed line-chart series does not support aggregate/window (use a metric series for aggregation)", widgetID, idx))
				}
			default:
				// Metric widget validation
				if query == "" && metric == "" && (measurement == "" || field == "") {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] must define query, metric, or measurement+field", widgetID, idx))
				}
				// Reject event fields for non-event-capable widgets.
				if seriesIsEventBacked {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] cannot use event_name_pattern in non-event widgets", widgetID, idx))
				}
				if (aggregate == "") != (window == "") && !(aggregateBandShortcut && idx == 0 && aggregate == "" && window != "") {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] aggregate and window must be set together", widgetID, idx))
				}
				if aggregate != "" && !isSupportedDashboardAggregate(aggregate) && !(widgetType == "aggregate_band" && aggregateBandShortcut && idx == 0) {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] has unsupported aggregate %q", widgetID, idx, aggregate))
				}
				if window != "" && !isValidDashboardDuration(window) {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] has invalid window %q", widgetID, idx, window))
				}
			}
			if widgetType == "line_chart" || widgetType == "aggregate_band" {
				label := effectiveDashboardSeriesLabel(series, idx)
				if prevIdx, exists := lineChartLabels[label]; exists {
					errs = append(errs, fmt.Sprintf("widget %q has duplicate line chart label %q at series[%d] and series[%d]", widgetID, label, prevIdx, idx))
				} else {
					lineChartLabels[label] = idx
				}
			}
			if series.Thresholds != nil {
				direction := strings.TrimSpace(series.Thresholds.Direction)
				hasWarning := series.Thresholds.Warning != nil
				hasCritical := series.Thresholds.Critical != nil
				if (hasWarning || hasCritical) && direction != "above" && direction != "below" {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] thresholds.direction must be above or below", widgetID, idx))
				}
			}
			if widgetType == "aggregate_band" {
				if aggregateBandShortcut {
					if idx == 0 {
						aggregateBandKey = effectiveDashboardSeriesSourceKey(cfg.DefaultDB, widget, series)
					}
					continue
				}
				role := effectiveDashboardSeriesRole(series)
				if role == "" {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] must define role min, max, or avg for aggregate_band presentation", widgetID, idx))
					continue
				}
				if role != "min" && role != "max" && role != "avg" {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] has unsupported aggregate_band role %q", widgetID, idx, role))
				}
				if prevIdx, exists := aggregateBandRoles[role]; exists {
					errs = append(errs, fmt.Sprintf("widget %q has duplicate aggregate_band role %q at series[%d] and series[%d]", widgetID, role, prevIdx, idx))
				} else {
					aggregateBandRoles[role] = idx
				}
				key := effectiveDashboardSeriesSourceKey(cfg.DefaultDB, widget, series)
				if aggregateBandKey == "" {
					aggregateBandKey = key
				} else if key != aggregateBandKey {
					errs = append(errs, fmt.Sprintf("widget %q aggregate_band series[%d] must use the same source query, database, and window as the other band series", widgetID, idx))
				}
			}
		}
		if widgetType == "aggregate_band" {
			if aggregateBandShortcut {
				if strings.TrimSpace(widget.Interval) == "" {
					errs = append(errs, fmt.Sprintf("widget %q aggregate_band shortcut series[0] requires window", widgetID))
				}
			} else {
				for _, role := range []string{"min", "max", "avg"} {
					if _, exists := aggregateBandRoles[role]; !exists {
						errs = append(errs, fmt.Sprintf("widget %q aggregate_band presentation requires a %s series", widgetID, role))
					}
				}
			}
		}
	}

	for idx, group := range cfg.Groups {
		for _, widgetID := range group.Widgets {
			if _, exists := cfg.Widgets[widgetID]; !exists {
				errs = append(errs, fmt.Sprintf("groups[%d] references unknown widget %q", idx, widgetID))
			}
		}
	}

	return errs
}

func effectiveDashboardSeriesLabel(series DashboardSeries, idx int) string {
	if label := strings.TrimSpace(series.Label); label != "" {
		return label
	}
	if role := effectiveDashboardSeriesRole(series); role != "" {
		switch role {
		case "avg":
			return "Avg"
		case "min":
			return "Min"
		case "max":
			return "Max"
		default:
			return role
		}
	}
	if query := strings.TrimSpace(series.Query); query != "" {
		return query
	}
	if metric := strings.TrimSpace(series.Metric); metric != "" {
		return metric
	}
	measurement := strings.TrimSpace(series.Measurement)
	field := strings.TrimSpace(series.Field)
	if measurement != "" || field != "" {
		return measurement + "." + field
	}
	return fmt.Sprintf("Series %d", idx+1)
}

func effectiveDashboardSeriesQuery(series DashboardSeries) string {
	if query := strings.TrimSpace(series.Query); query != "" {
		return query
	}
	if metric := strings.TrimSpace(series.Metric); metric != "" {
		return metric
	}
	measurement := strings.TrimSpace(series.Measurement)
	field := strings.TrimSpace(series.Field)
	if measurement != "" && field != "" {
		return measurement + "." + field
	}
	return ""
}

func effectiveDashboardSeriesRole(series DashboardSeries) string {
	if role := strings.TrimSpace(series.Role); role != "" {
		return role
	}
	aggregate := strings.ToLower(strings.TrimSpace(series.Aggregate))
	if aggregate == "min" || aggregate == "max" || aggregate == "avg" {
		return aggregate
	}
	return ""
}

func effectiveDashboardSeriesSourceKey(defaultDB string, widget DashboardWidget, series DashboardSeries) string {
	db := strings.TrimSpace(series.DB)
	if db == "" {
		db = strings.TrimSpace(series.Database)
	}
	if db == "" {
		db = strings.TrimSpace(defaultDB)
	}
	return db + "|" + effectiveDashboardSeriesQuery(series) + "|" + effectiveDashboardSeriesAggregateWindow(widget, series)
}

func usesAggregateBandShortcut(widget DashboardWidget) bool {
	if effectiveDashboardWidgetType(widget) != "aggregate_band" || len(widget.Series) != 1 {
		return false
	}
	series := widget.Series[0]
	return strings.TrimSpace(series.Aggregate) == "" && effectiveDashboardSeriesRole(series) == ""
}

func effectiveDashboardSeriesAggregateWindow(widget DashboardWidget, series DashboardSeries) string {
	widgetType := effectiveDashboardWidgetType(widget)
	interval := strings.TrimSpace(widget.Interval)
	if widgetType == "aggregate_band" {
		if interval != "" {
			return interval
		}
		return strings.TrimSpace(series.Window)
	}
	if widgetType == "line_chart" && strings.TrimSpace(series.Aggregate) != "" {
		if interval != "" {
			return interval
		}
		return strings.TrimSpace(series.Window)
	}
	return strings.TrimSpace(series.Window)
}

func isSupportedDashboardAggregate(value string) bool {
	name := strings.TrimSpace(value)
	if name == "" {
		return false
	}
	for _, aggregate := range engine.SupportedAggregates() {
		if aggregate == name {
			return true
		}
	}
	return false
}

func isValidDashboardDuration(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if len(value) < 2 {
		return false
	}
	unit := value[len(value)-1]
	if !strings.ContainsRune("smhdwSMHDW", rune(unit)) {
		return false
	}
	for _, ch := range value[:len(value)-1] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// isValidOverlayColor lightly validates a CSS color string. Accepts:
//   - hex form: "#rgb", "#rgba", "#rrggbb", "#rrggbbaa"
//   - simple named colors: 1..32 ASCII letters
//
// We deliberately do NOT enumerate every CSS named color — the browser
// is the authority. If it doesn't recognize the value it will ignore the
// stroke; the dashboard still renders correctly. We just want to catch
// obvious garbage (whitespace, slashes, SQL fragments) before it lands
// in a saved dashboard.json.
func isValidOverlayColor(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if value[0] == '#' {
		hex := value[1:]
		switch len(hex) {
		case 3, 4, 6, 8:
		default:
			return false
		}
		for _, ch := range hex {
			isDigit := ch >= '0' && ch <= '9'
			isUpper := ch >= 'A' && ch <= 'F'
			isLower := ch >= 'a' && ch <= 'f'
			if !isDigit && !isUpper && !isLower {
				return false
			}
		}
		return true
	}
	// Named color: bounded length, ASCII letters only.
	if len(value) > 32 {
		return false
	}
	for _, ch := range value {
		isUpper := ch >= 'A' && ch <= 'Z'
		isLower := ch >= 'a' && ch <= 'z'
		if !isUpper && !isLower {
			return false
		}
	}
	return true
}

func saveDashboardConfig(path string, cfg DashboardConfigDocument) (string, []byte, error) {
	normalizeDashboardConfig(&cfg)
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", nil, err
	}
	payload = append(payload, '\n')

	backupPath, err := backupExistingDashboard(path)
	if err != nil {
		return "", nil, err
	}
	if err := writeFileAtomic(path, payload); err != nil {
		return "", nil, err
	}
	return backupPath, payload, nil
}

func backupExistingDashboard(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	backupDir := filepath.Join(filepath.Dir(path), "dashboard_backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", err
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	backupPath := filepath.Join(backupDir, base+"."+stamp+ext)
	if err := os.WriteFile(backupPath, raw, 0o644); err != nil {
		return "", err
	}
	return backupPath, nil
}

func writeFileAtomic(path string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, bytes.NewReader(payload)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
