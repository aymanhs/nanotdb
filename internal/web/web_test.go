package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegister_ServesIndexOnRootAndDashboard(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), t.TempDir())

	for _, p := range []string{"/", "/dashboard", "/dashboard/", "/dashboard/edit", "/dashboard/edit/", "/engine", "/engine/"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("path %s status mismatch: got=%d want=200", p, rec.Code)
		}
	}
}

func TestRegister_ServesAssets(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/dashboard/assets/dashboard_app.js", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("asset status mismatch: got=%d want=200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("expected non-empty asset body")
	}
	exploreReq := httptest.NewRequest(http.MethodGet, "/explore/assets/app.js", nil)
	exploreRec := httptest.NewRecorder()
	mux.ServeHTTP(exploreRec, exploreReq)
	if exploreRec.Code != http.StatusOK {
		t.Fatalf("explore asset status mismatch: got=%d want=200", exploreRec.Code)
	}
	engineReq := httptest.NewRequest(http.MethodGet, "/engine/assets/engine_app.js", nil)
	engineRec := httptest.NewRecorder()
	mux.ServeHTTP(engineRec, engineReq)
	if engineRec.Code != http.StatusOK {
		t.Fatalf("engine asset status mismatch: got=%d want=200", engineRec.Code)
	}
	commonReq := httptest.NewRequest(http.MethodGet, "/assets/common.css", nil)
	commonRec := httptest.NewRecorder()
	mux.ServeHTTP(commonRec, commonReq)
	if commonRec.Code != http.StatusOK {
		t.Fatalf("common asset status mismatch: got=%d want=200", commonRec.Code)
	}
}

func TestRegister_ServesDashboardAssetsWithRegressionFixes(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), t.TempDir())

	dashReq := httptest.NewRequest(http.MethodGet, "/dashboard/assets/dashboard_app.js", nil)
	dashRec := httptest.NewRecorder()
	mux.ServeHTTP(dashRec, dashReq)
	if dashRec.Code != http.StatusOK {
		t.Fatalf("dashboard asset status mismatch: got=%d want=200", dashRec.Code)
	}
	dashJS := dashRec.Body.String()
	if strings.Contains(dashJS, "function rebalanceSingleNumberRows(") {
		t.Fatalf("dashboard asset should use shared rebalanceSingleNumberRows helper without redeclaring it")
	}
	if !strings.Contains(dashJS, "widget-refresh-error") {
		t.Fatalf("dashboard asset should include widget refresh error handling")
	}
	if !strings.Contains(dashJS, "function expandedChartSeries(widget)") || !strings.Contains(dashJS, "const chartSeries = expandedChartSeries(widget)") {
		t.Fatalf("dashboard asset should expand shorthand chart series before rendering")
	}

	commonReq := httptest.NewRequest(http.MethodGet, "/assets/dashboard_utils.js", nil)
	commonRec := httptest.NewRecorder()
	mux.ServeHTTP(commonRec, commonReq)
	if commonRec.Code != http.StatusOK {
		t.Fatalf("dashboard utils asset status mismatch: got=%d want=200", commonRec.Code)
	}
	commonJS := commonRec.Body.String()
	if !strings.Contains(commonJS, "const seriesItems = Array.isArray(seriesMap)") {
		t.Fatalf("dashboard utils asset should accept ordered series arrays")
	}
}

func TestRegister_ServesDashboardConfigEndpoint(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard-config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard-config status mismatch: got=%d want=200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("expected non-empty dashboard config")
	}
}

func TestRegister_ServesDashboardConfigFromFile(t *testing.T) {
	root := t.TempDir()
	config := []byte(`{
  "title": "Custom Sample",
  "groups": [{"id":"overview","label":"Overview","widgets":["sample"]}],
  "widgets": {
    "sample": {
      "type": "number",
      "title": "Sample",
      "series": [{"db":"source","metric":"temp.synthetic00"}]
    }
  }
}`)
	if err := os.WriteFile(root+"/dashboard.json", config, 0o644); err != nil {
		t.Fatalf("write dashboard config: %v", err)
	}

	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard-config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard-config status mismatch: got=%d want=200", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "Custom Sample") {
		t.Fatalf("expected dashboard config body to include custom title, got %q", got)
	}
}

func TestRegister_ValidatesDashboardConfig(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Edited Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"overview","label":"Overview","widgets":["sample"]}],
  "widgets": {
    "sample": {
      "type": "line_chart",
      "title": "Sample",
      "lookback": "6h",
      "interval": "1m",
      "series": [{"metric": "temp.cpu"}]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate status mismatch: got=%d want=200 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("expected ok response, got %q", rec.Body.String())
	}
}

func TestRegister_ValidatesDashboardQueryConfig(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Edited Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"overview","label":"Overview","widgets":["sample"]}],
  "widgets": {
    "sample": {
      "type": "line_chart",
      "title": "Sample",
      "lookback": "6h",
      "interval": "1m",
			"series": [{"query": "temp.cpu", "aggregate": "p95"}]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate status mismatch: got=%d want=200 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("expected ok response, got %q", rec.Body.String())
	}
}

func TestRegister_RejectsUnsupportedDashboardQueryAggregate(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Edited Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"overview","label":"Overview","widgets":["sample"]}],
  "widgets": {
    "sample": {
      "type": "line_chart",
      "title": "Sample",
      "lookback": "6h",
      "interval": "1m",
			"series": [{"query": "temp.cpu", "aggregate": "bogus"}]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("validate status mismatch: got=%d want=400 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `unsupported aggregate \"bogus\"`) {
		t.Fatalf("expected unsupported aggregate error, got %q", rec.Body.String())
	}
}

func TestRegister_ValidatesDashboardAggregateBandConfig(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Edited Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"overview","label":"Overview","widgets":["sample"]}],
  "widgets": {
    "sample": {
			"type": "aggregate_band",
      "title": "CPU Busy",
      "lookback": "24h",
      "interval": "10m",
      "series": [
		{"query": "cpu.busy_pct", "aggregate": "avg"},
		{"query": "cpu.busy_pct", "aggregate": "min"},
		{"query": "cpu.busy_pct", "aggregate": "max"}
      ]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate status mismatch: got=%d want=200 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("expected ok response, got %q", rec.Body.String())
	}
}

func TestRegister_ValidatesDashboardAggregateBandShortcutConfig(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Edited Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"overview","label":"Overview","widgets":["sample"]}],
  "widgets": {
    "sample": {
			"type": "aggregate_band",
      "title": "CPU Busy",
      "lookback": "24h",
      "interval": "10m",
      "series": [
		{"query": "cpu.busy_pct"}
      ]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate status mismatch: got=%d want=200 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("expected ok response, got %q", rec.Body.String())
	}
}

func TestRegister_ValidatesDashboardAggregateBandShortcutUsingIntervalWindow(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Edited Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"overview","label":"Overview","widgets":["sample"]}],
  "widgets": {
    "sample": {
      "type": "aggregate_band",
      "title": "CPU Busy",
      "lookback": "24h",
      "interval": "10m",
      "series": [
        {"query": "cpu.busy_pct"}
      ]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate status mismatch: got=%d want=200 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("expected ok response, got %q", rec.Body.String())
	}
}

func TestRegister_RejectsDuplicateLineChartLabels(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Edited Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"overview","label":"Overview","widgets":["sample"]}],
  "widgets": {
    "sample": {
      "type": "line_chart",
      "title": "Sample",
      "lookback": "6h",
      "interval": "1m",
      "series": [
        {"label": "CPU", "metric": "temp.cpu"},
        {"label": "CPU", "metric": "temp.gpu"}
      ]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("validate status mismatch: got=%d want=400 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `duplicate line chart label`) {
		t.Fatalf("expected duplicate line chart label error, got %q", rec.Body.String())
	}
}

func TestRegister_SavesDashboardConfigWithBackup(t *testing.T) {
	root := t.TempDir()
	original := []byte(`{
  "title": "Original Dashboard",
  "groups": [{"id":"overview","label":"Overview","widgets":["sample"]}],
  "widgets": {
    "sample": {"type":"number","title":"Sample","series":[{"metric":"temp.cpu"}]}
  }
}`)
	if err := os.WriteFile(filepath.Join(root, "dashboard.json"), original, 0o644); err != nil {
		t.Fatalf("write original dashboard: %v", err)
	}

	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Edited Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"overview","label":"Overview","widgets":["history"]}],
  "widgets": {
    "history": {
      "type": "line_chart",
      "title": "History",
      "refresh_sec": 60,
      "auto_refresh": false,
      "lookback": "24h",
      "interval": "5m",
      "series": [{"metric": "temp.cpu"}]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPut, "/api/dashboard-config", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status mismatch: got=%d want=200 body=%q", rec.Code, rec.Body.String())
	}
	savedRaw, err := os.ReadFile(filepath.Join(root, "dashboard.json"))
	if err != nil {
		t.Fatalf("read saved dashboard: %v", err)
	}
	if !strings.Contains(string(savedRaw), `"Edited Dashboard"`) {
		t.Fatalf("expected saved dashboard content, got %q", string(savedRaw))
	}
	if !strings.Contains(string(savedRaw), `"auto_refresh": false`) {
		t.Fatalf("expected auto_refresh field in saved dashboard, got %q", string(savedRaw))
	}
	backupDir := filepath.Join(root, "dashboard_backups")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("expected at least one backup file")
	}
	backupRaw, err := os.ReadFile(filepath.Join(backupDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read backup file: %v", err)
	}
	if !strings.Contains(string(backupRaw), `"Original Dashboard"`) {
		t.Fatalf("expected original dashboard in backup, got %q", string(backupRaw))
	}
}

func TestRegister_UsesWebRootOverrides(t *testing.T) {
	root := t.TempDir()
	webRoot := filepath.Join(root, "ui")
	if err := os.MkdirAll(filepath.Join(webRoot, "dashboard_assets"), 0o755); err != nil {
		t.Fatalf("mkdir dashboard assets: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(webRoot, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir explore assets: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(webRoot, "common_assets"), 0o755); err != nil {
		t.Fatalf("mkdir common assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "dashboard.html"), []byte(`<!doctype html><html><body>CUSTOM-DASH {{ .Title }}</body></html>`), 0o644); err != nil {
		t.Fatalf("write dashboard html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte(`<!doctype html><html><body>CUSTOM-EXPLORE {{ .Title }}</body></html>`), 0o644); err != nil {
		t.Fatalf("write explore html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "dashboard_assets", "dashboard_app.js"), []byte("console.log('custom dashboard');"), 0o644); err != nil {
		t.Fatalf("write dashboard js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "assets", "app.js"), []byte("console.log('custom explore');"), 0o644); err != nil {
		t.Fatalf("write explore js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "common_assets", "common.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatalf("write common css: %v", err)
	}

	mux := http.NewServeMux()
	cfg := DefaultConfig()
	cfg.WebRoot = webRoot
	Register(mux, cfg, root)

	dashReq := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	dashRec := httptest.NewRecorder()
	mux.ServeHTTP(dashRec, dashReq)
	if dashRec.Code != http.StatusOK || !strings.Contains(dashRec.Body.String(), "CUSTOM-DASH") {
		t.Fatalf("expected custom dashboard html, code=%d body=%q", dashRec.Code, dashRec.Body.String())
	}

	assetReq := httptest.NewRequest(http.MethodGet, "/dashboard/assets/dashboard_app.js", nil)
	assetRec := httptest.NewRecorder()
	mux.ServeHTTP(assetRec, assetReq)
	if assetRec.Code != http.StatusOK || !strings.Contains(assetRec.Body.String(), "custom dashboard") {
		t.Fatalf("expected custom dashboard asset, code=%d body=%q", assetRec.Code, assetRec.Body.String())
	}
}

func TestRegister_InjectsAPIBaseURL(t *testing.T) {
	mux := http.NewServeMux()
	cfg := DefaultConfig()
	cfg.APIBaseURL = "https://ui.example.test/nanotdb-api"
	Register(mux, cfg, t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status mismatch: got=%d want=200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "https://ui.example.test/nanotdb-api") {
		t.Fatalf("expected api_base_url injection, got %q", rec.Body.String())
	}
}

func TestExportAssets_WritesBundle(t *testing.T) {
	root := t.TempDir()
	if err := ExportAssets(root); err != nil {
		t.Fatalf("ExportAssets failed: %v", err)
	}
	for _, rel := range []string{"dashboard.html", "editor.html", "index.html", "engine.html", filepath.Join("dashboard_assets", "dashboard_app.js"), filepath.Join("dashboard_assets", "editor_app.js"), filepath.Join("dashboard_assets", "editor.css"), filepath.Join("assets", "app.js"), filepath.Join("engine_assets", "engine_app.js"), filepath.Join("common_assets", "common.css")} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("expected exported file %s: %v", rel, err)
		}
	}
}

func TestRegister_DisabledSkipsDashboardRoutes(t *testing.T) {
	mux := http.NewServeMux()
	cfg := DefaultConfig()
	cfg.Enabled = false
	Register(mux, cfg, t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status mismatch: got=%d want=404", rec.Code)
	}
}

func TestRegister_ValidatesDashboardEventLogWidget(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Events Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"events","label":"Events","widgets":["slow_writes"]}],
  "widgets": {
    "slow_writes": {
      "type": "event_log",
      "title": "Slow Writes",
      "lookback": "6h",
      "series": [
        {
          "event_name_pattern": "disk.sd_write_probe.slow",
          "event_limit": 10,
          "db": "metrics"
        }
      ]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate status mismatch: got=%d want=200 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("expected ok response, got %q", rec.Body.String())
	}
}

func TestRegister_RejectsEventWidgetWithoutEventNamePattern(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Events Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"events","label":"Events","widgets":["bad"]}],
  "widgets": {
    "bad": {
      "type": "event_log",
      "title": "Missing Event Pattern",
      "series": [{"db": "metrics"}]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("validate status mismatch: got=%d want=400 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `event_name_pattern`) {
		t.Fatalf("expected event_name_pattern error, got %q", rec.Body.String())
	}
}

func TestRegister_RejectsEventWidgetWithMetricFields(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Events Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"events","label":"Events","widgets":["bad"]}],
  "widgets": {
    "bad": {
      "type": "event_log",
      "title": "Mixed Fields",
      "series": [
        {
          "event_name_pattern": "disk.sd_write_probe.slow",
          "metric": "cpu.busy_pct",
          "db": "metrics"
        }
      ]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("validate status mismatch: got=%d want=400 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `cannot mix`) {
		t.Fatalf("expected cannot mix error, got %q", rec.Body.String())
	}
}

func TestRegister_RejectsMetricWidgetWithEventFields(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Mixed Dashboard",
  "default_db": "metrics",
  "groups": [{"id":"overview","label":"Overview","widgets":["bad"]}],
  "widgets": {
    "bad": {
      "type": "numbers",
      "series": [
        {
          "metric": "cpu.busy_pct",
          "event_name_pattern": "disk.sd_write_probe.slow"
        }
      ]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("validate status mismatch: got=%d want=400 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `event_name_pattern`) {
		t.Fatalf("expected event_name_pattern error, got %q", rec.Body.String())
	}
}

func TestRegister_AcceptsMixedDashboardWithMetricAndEventWidgets(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)

	body := strings.NewReader(`{
  "title": "Mixed Dashboard",
  "default_db": "metrics",
  "groups": [
    {"id":"overview","label":"Overview","widgets":["cpu_widget"]},
    {"id":"events","label":"Events","widgets":["slow_writes"]}
  ],
  "widgets": {
    "cpu_widget": {
      "type": "numbers",
      "series": [{"metric": "cpu.busy_pct"}]
    },
    "slow_writes": {
      "type": "event_log",
      "lookback": "6h",
      "series": [{"event_name_pattern": "disk.sd_write_probe.slow"}]
    }
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate status mismatch: got=%d want=200 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("expected ok response, got %q", rec.Body.String())
	}
}

// validateDashboardJSON is a small helper that POSTs a dashboard config
// to /api/dashboard-config/validate and returns (code, body). Used by
// the event-backed line-chart and event_overlays tests below to keep
// each case readable.
func validateDashboardJSON(t *testing.T, body string) (int, string) {
	t.Helper()
	root := t.TempDir()
	mux := http.NewServeMux()
	Register(mux, DefaultConfig(), root)
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard-config/validate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestEmbeddedDefaultDashboardValidates guards the shipped showcase
// dashboard so a broken default never ships to a fresh install.
func TestEmbeddedDefaultDashboardValidates(t *testing.T) {
	code, body := validateDashboardJSON(t, string(DefaultDashboardConfig()))
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("embedded default_dashboard.json failed validation: code=%d body=%s", code, body)
	}
}

// TestValidate_EventBackedLineChartSeries_Accepts confirms a line_chart
// widget with an event-backed series (event_name_pattern present, no
// metric fields) validates successfully.
func TestValidate_EventBackedLineChartSeries_Accepts(t *testing.T) {
	code, body := validateDashboardJSON(t, `{
  "title": "Events on a chart",
  "default_db": "metrics",
  "groups": [{"id":"g","widgets":["w"]}],
  "widgets": {
    "w": {
      "type": "line_chart",
      "title": "Slow disk events",
      "lookback": "6h",
      "interval": "1m",
      "series": [{"label": "ms", "event_name_pattern": "disc.write.slow"}]
    }
  }
}`)
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("expected ok, got code=%d body=%q", code, body)
	}
}

// TestValidate_EventBackedLineChartSeries_RejectsMixedFields ensures
// the validator catches a series that has both event_name_pattern and
// metric-shaped fields.
func TestValidate_EventBackedLineChartSeries_RejectsMixedFields(t *testing.T) {
	code, body := validateDashboardJSON(t, `{
  "title": "Bad mix",
  "default_db": "metrics",
  "groups": [{"id":"g","widgets":["w"]}],
  "widgets": {
    "w": {
      "type": "line_chart",
      "lookback": "6h",
      "interval": "1m",
      "series": [{"event_name_pattern": "ev.x", "metric": "temp.cpu"}]
    }
  }
}`)
	if code == http.StatusOK {
		t.Fatalf("expected non-OK for mixed series, got 200 body=%q", body)
	}
	if !strings.Contains(body, "cannot mix event_name_pattern with metric fields") {
		t.Fatalf("expected mix error, got %q", body)
	}
}

// TestValidate_EventBackedLineChartSeries_RejectsAggregate ensures
// numeric-aggregate-over-events is rejected (designed but not built —
// v1 does not support per-event-value aggregation on event-backed
// line series).
func TestValidate_EventBackedLineChartSeries_RejectsAggregate(t *testing.T) {
	code, body := validateDashboardJSON(t, `{
  "title": "Bad agg",
  "default_db": "metrics",
  "groups": [{"id":"g","widgets":["w"]}],
  "widgets": {
    "w": {
      "type": "line_chart",
      "lookback": "6h",
      "interval": "1m",
      "series": [{"event_name_pattern": "ev.x", "aggregate": "avg", "window": "1m"}]
    }
  }
}`)
	if code == http.StatusOK {
		t.Fatalf("expected non-OK for event series w/ aggregate, got 200 body=%q", body)
	}
	if !strings.Contains(body, "event-backed line-chart series does not support aggregate/window") {
		t.Fatalf("expected aggregate-rejection error, got %q", body)
	}
}

// TestValidate_EventOverlays_Accepts confirms a line_chart with one
// metric series and two event_overlays validates.
func TestValidate_EventOverlays_Accepts(t *testing.T) {
	code, body := validateDashboardJSON(t, `{
  "title": "Overlays",
  "default_db": "metrics",
  "groups": [{"id":"g","widgets":["w"]}],
  "widgets": {
    "w": {
      "type": "line_chart",
      "lookback": "6h",
      "interval": "1m",
      "series": [{"metric": "temp.cpu"}],
      "event_overlays": [
        {"event_name_pattern": "temp.over*", "color": "#c00"},
        {"event_name_pattern": "deploy.completed", "color": "blue"}
      ]
    }
  }
}`)
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("expected ok, got code=%d body=%q", code, body)
	}
}

// TestValidate_EventOverlays_RejectsOnNonLineChart confirms overlays
// are only valid on line_chart widgets.
func TestValidate_EventOverlays_RejectsOnNonLineChart(t *testing.T) {
	code, body := validateDashboardJSON(t, `{
  "title": "Bad",
  "default_db": "metrics",
  "groups": [{"id":"g","widgets":["w"]}],
  "widgets": {
    "w": {
      "type": "event_log",
      "lookback": "6h",
      "series": [{"event_name_pattern": "ev.x"}],
      "event_overlays": [{"event_name_pattern": "ev.y"}]
    }
  }
}`)
	if code == http.StatusOK {
		t.Fatalf("expected non-OK for overlays on event_log, got 200 body=%q", body)
	}
	if !strings.Contains(body, "event_overlays only supported on line_chart") {
		t.Fatalf("expected scope error, got %q", body)
	}
}

// TestValidate_EventOverlays_RejectsEmptyPattern ensures each overlay
// must carry an event_name_pattern.
func TestValidate_EventOverlays_RejectsEmptyPattern(t *testing.T) {
	code, body := validateDashboardJSON(t, `{
  "title": "Bad",
  "default_db": "metrics",
  "groups": [{"id":"g","widgets":["w"]}],
  "widgets": {
    "w": {
      "type": "line_chart",
      "lookback": "6h",
      "interval": "1m",
      "series": [{"metric": "temp.cpu"}],
      "event_overlays": [{"color": "#c00"}]
    }
  }
}`)
	if code == http.StatusOK {
		t.Fatalf("expected non-OK for overlay missing pattern, got 200 body=%q", body)
	}
	if !strings.Contains(body, "must define event_name_pattern") {
		t.Fatalf("expected missing-pattern error, got %q", body)
	}
}

// TestValidate_EventOverlays_RejectsBadColor verifies obvious garbage
// in the color field surfaces a clean error.
func TestValidate_EventOverlays_RejectsBadColor(t *testing.T) {
	code, body := validateDashboardJSON(t, `{
  "title": "Bad",
  "default_db": "metrics",
  "groups": [{"id":"g","widgets":["w"]}],
  "widgets": {
    "w": {
      "type": "line_chart",
      "lookback": "6h",
      "interval": "1m",
      "series": [{"metric": "temp.cpu"}],
      "event_overlays": [{"event_name_pattern": "ev.x", "color": "javascript:alert(1)"}]
    }
  }
}`)
	if code == http.StatusOK {
		t.Fatalf("expected non-OK for bogus color, got 200 body=%q", body)
	}
	if !strings.Contains(body, "invalid color") {
		t.Fatalf("expected color error, got %q", body)
	}
}

// TestValidate_EventOverlays_RejectsDuplicateLabels catches an editor
// authoring mistake — two overlays with the same label or fallback
// pattern would be indistinguishable in the UI legend.
func TestValidate_EventOverlays_RejectsDuplicateLabels(t *testing.T) {
	code, body := validateDashboardJSON(t, `{
  "title": "Dup",
  "default_db": "metrics",
  "groups": [{"id":"g","widgets":["w"]}],
  "widgets": {
    "w": {
      "type": "line_chart",
      "lookback": "6h",
      "interval": "1m",
      "series": [{"metric": "temp.cpu"}],
      "event_overlays": [
        {"event_name_pattern": "ev.x"},
        {"event_name_pattern": "ev.x"}
      ]
    }
  }
}`)
	if code == http.StatusOK {
		t.Fatalf("expected non-OK for duplicate overlay label, got 200 body=%q", body)
	}
	if !strings.Contains(body, "duplicate label") {
		t.Fatalf("expected duplicate-label error, got %q", body)
	}
}
