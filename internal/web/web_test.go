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

	for _, p := range []string{"/", "/dashboard", "/dashboard/"} {
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
	adhocReq := httptest.NewRequest(http.MethodGet, "/adhoc/assets/app.js", nil)
	adhocRec := httptest.NewRecorder()
	mux.ServeHTTP(adhocRec, adhocReq)
	if adhocRec.Code != http.StatusOK {
		t.Fatalf("adhoc asset status mismatch: got=%d want=200", adhocRec.Code)
	}
	commonReq := httptest.NewRequest(http.MethodGet, "/assets/common.css", nil)
	commonRec := httptest.NewRecorder()
	mux.ServeHTTP(commonRec, commonReq)
	if commonRec.Code != http.StatusOK {
		t.Fatalf("common asset status mismatch: got=%d want=200", commonRec.Code)
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

func TestRegister_UsesWebRootOverrides(t *testing.T) {
	root := t.TempDir()
	webRoot := filepath.Join(root, "ui")
	if err := os.MkdirAll(filepath.Join(webRoot, "dashboard_assets"), 0o755); err != nil {
		t.Fatalf("mkdir dashboard assets: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(webRoot, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir adhoc assets: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(webRoot, "common_assets"), 0o755); err != nil {
		t.Fatalf("mkdir common assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "dashboard.html"), []byte(`<!doctype html><html><body>CUSTOM-DASH {{ .Title }}</body></html>`), 0o644); err != nil {
		t.Fatalf("write dashboard html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte(`<!doctype html><html><body>CUSTOM-ADHOC {{ .Title }}</body></html>`), 0o644); err != nil {
		t.Fatalf("write adhoc html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "dashboard_assets", "dashboard_app.js"), []byte("console.log('custom dashboard');"), 0o644); err != nil {
		t.Fatalf("write dashboard js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "assets", "app.js"), []byte("console.log('custom adhoc');"), 0o644); err != nil {
		t.Fatalf("write adhoc js: %v", err)
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
	for _, rel := range []string{"dashboard.html", "index.html", filepath.Join("dashboard_assets", "dashboard_app.js"), filepath.Join("assets", "app.js"), filepath.Join("common_assets", "common.css")} {
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
