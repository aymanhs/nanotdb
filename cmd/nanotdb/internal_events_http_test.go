package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aymanhs/nanotdb/internal/engine"
)

// newInternalEventsHTTPEngine returns a fresh engine — the internal
// events emitter is enabled by default in the engine config, so no
// manifest seed work is needed.
func newInternalEventsHTTPEngine(t *testing.T) *engine.Engine {
	t.Helper()
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

func TestHandleInternalEventsCatalog(t *testing.T) {
	eng := newInternalEventsHTTPEngine(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/internal-events/catalog", nil)
	rec := httptest.NewRecorder()
	handleInternalEventsCatalog(eng)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType    string `json:"resultType"`
			MasterEnabled bool   `json:"master_enabled"`
			DestinationDB string `json:"destination_db"`
			Groups        []struct {
				Name   string `json:"name"`
				Events []struct {
					Name string `json:"name"`
				} `json:"events"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Status != "success" {
		t.Fatalf("status=%q", parsed.Status)
	}
	if parsed.Data.ResultType != "internal_events_catalog" {
		t.Fatalf("resultType=%q", parsed.Data.ResultType)
	}
	if !parsed.Data.MasterEnabled {
		t.Fatalf("master_enabled=false")
	}
	if parsed.Data.DestinationDB != "internal" {
		t.Fatalf("destination_db=%q", parsed.Data.DestinationDB)
	}
	if len(parsed.Data.Groups) < 10 {
		t.Fatalf("expected at least 10 groups, got %d", len(parsed.Data.Groups))
	}
}

func TestHandleInternalEventsGroupsGet(t *testing.T) {
	eng := newInternalEventsHTTPEngine(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/internal-events/groups", nil)
	rec := httptest.NewRecorder()
	handleInternalEventsGroups(eng)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"nanotdb.lifecycle"`) {
		t.Fatalf("expected nanotdb.lifecycle in body, got %s", rec.Body.String())
	}
}

func TestHandleInternalEventsGroupsPostToggle(t *testing.T) {
	eng := newInternalEventsHTTPEngine(t)
	body := `{"nanotdb.wal":"on"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal-events/groups", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleInternalEventsGroups(eng)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Confirm the runtime flip via the engine API.
	if !eng.InternalEventsCatalog().Groups[0].Enabled && eng.InternalEventsCatalog().Groups[0].Name == "nanotdb.wal" {
		// just touch — the next assertion is what matters
	}
	groups := eng.InternalEventsGroups()
	found := false
	for _, g := range groups {
		if g.Name == "nanotdb.wal" {
			found = true
			if !g.Enabled {
				t.Fatalf("nanotdb.wal still disabled after POST")
			}
			if g.Source != "runtime" {
				t.Fatalf("source=%q want \"runtime\"", g.Source)
			}
		}
	}
	if !found {
		t.Fatalf("nanotdb.wal not in groups list")
	}
}

func TestHandleInternalEventsGroupsPostUnknownKey(t *testing.T) {
	eng := newInternalEventsHTTPEngine(t)
	body := `{"nantodb.misspelled":"on"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal-events/groups", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleInternalEventsGroups(eng)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown group, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleInternalEventsGroupsPostInvalidValue(t *testing.T) {
	eng := newInternalEventsHTTPEngine(t)
	body := `{"nanotdb.wal":"sometimes"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal-events/groups", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleInternalEventsGroups(eng)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid value, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleInternalEventsCatalogMethodNotAllowed(t *testing.T) {
	eng := newInternalEventsHTTPEngine(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal-events/catalog", nil)
	rec := httptest.NewRecorder()
	handleInternalEventsCatalog(eng)(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}
