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
	Type        string            `json:"type"`
	Title       string            `json:"title,omitempty"`
	RefreshSec  int               `json:"refresh_sec,omitempty"`
	AutoRefresh *bool             `json:"auto_refresh,omitempty"`
	Lookback    string            `json:"lookback,omitempty"`
	Interval    string            `json:"interval,omitempty"`
	Series      []DashboardSeries `json:"series,omitempty"`
}

type DashboardSeries struct {
	Label       string               `json:"label,omitempty"`
	DB          string               `json:"db,omitempty"`
	Database    string               `json:"database,omitempty"`
	Metric      string               `json:"metric,omitempty"`
	Measurement string               `json:"measurement,omitempty"`
	Field       string               `json:"field,omitempty"`
	Transform   *DashboardTransform  `json:"transform,omitempty"`
	Thresholds  *DashboardThresholds `json:"thresholds,omitempty"`
	Scale       *float64             `json:"scale,omitempty"`
	Offset      *float64             `json:"offset,omitempty"`
	Unit        string               `json:"unit,omitempty"`
	Decimals    *int                 `json:"decimals,omitempty"`
	Format      string               `json:"format,omitempty"`
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

func readDashboardConfigRequestFromBody(body io.Reader) (DashboardConfigDocument, []string) {
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
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
		cfg.Widgets[id] = widget
	}
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
		switch widget.Type {
		case "number", "numbers":
			if len(widget.Series) == 0 {
				errs = append(errs, fmt.Sprintf("widget %q must define at least one series", widgetID))
			}
		case "line_chart":
			if len(widget.Series) == 0 {
				errs = append(errs, fmt.Sprintf("widget %q must define at least one series", widgetID))
			}
			if !isValidDashboardDuration(widget.Lookback) {
				errs = append(errs, fmt.Sprintf("widget %q has invalid lookback %q", widgetID, widget.Lookback))
			}
			if !isValidDashboardDuration(widget.Interval) {
				errs = append(errs, fmt.Sprintf("widget %q has invalid interval %q", widgetID, widget.Interval))
			}
		default:
			errs = append(errs, fmt.Sprintf("widget %q has unsupported type %q", widgetID, widget.Type))
		}
		if widget.RefreshSec < 0 {
			errs = append(errs, fmt.Sprintf("widget %q has invalid refresh_sec %d", widgetID, widget.RefreshSec))
		}
		for idx, series := range widget.Series {
			metric := strings.TrimSpace(series.Metric)
			measurement := strings.TrimSpace(series.Measurement)
			field := strings.TrimSpace(series.Field)
			if metric == "" && (measurement == "" || field == "") {
				errs = append(errs, fmt.Sprintf("widget %q series[%d] must define metric or measurement+field", widgetID, idx))
			}
			if series.Thresholds != nil {
				direction := strings.TrimSpace(series.Thresholds.Direction)
				hasWarning := series.Thresholds.Warning != nil
				hasCritical := series.Thresholds.Critical != nil
				if (hasWarning || hasCritical) && direction != "above" && direction != "below" {
					errs = append(errs, fmt.Sprintf("widget %q series[%d] thresholds.direction must be above or below", widgetID, idx))
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
