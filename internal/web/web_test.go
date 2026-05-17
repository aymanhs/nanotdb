package web

import (
	"net/http"
	"net/http/httptest"
	"os"
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
