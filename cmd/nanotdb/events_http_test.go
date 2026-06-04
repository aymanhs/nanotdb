package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aymanhs/nanotdb/internal/engine"
)

// Helpers ----------------------------------------------------------

// newEventsHTTPEngine builds a fresh engine with [events] enabled for
// the given database name. Returns the engine, the data root, and a
// cleanup func.
func newEventsHTTPEngine(t *testing.T, dbName string) *engine.Engine {
	t.Helper()
	root := t.TempDir()
	dbDir := filepath.Join(root, dbName)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := []byte("" +
		"[retention]\n" +
		"retention_action = \"keep\"\n\n" +
		"[events]\n" +
		"enabled = true\n")
	if err := os.WriteFile(filepath.Join(dbDir, "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	eng, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// postEvents posts a JSON body (string-or-bytes) to /api/v1/events and
// returns the recorder.
func postEvents(t *testing.T, eng *engine.Engine, body string, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/events", strings.NewReader(body))
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	handleEvents(eng)(rec, req)
	return rec
}

// getEvents calls /api/v1/events?... and returns the recorder.
func getEvents(t *testing.T, eng *engine.Engine, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?"+query, nil)
	rec := httptest.NewRecorder()
	handleEvents(eng)(rec, req)
	return rec
}

func getEventsAggregate(t *testing.T, eng *engine.Engine, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/aggregate?"+query, nil)
	rec := httptest.NewRecorder()
	handleEventsAggregate(eng)(rec, req)
	return rec
}

// Decode response structs --------------------------------------

type importResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Imported   int    `json:"imported"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

type queryRespEvent struct {
	Name      string          `json:"name"`
	EventID   uint16          `json:"id"`
	TS        int64           `json:"ts"`
	ValueType string          `json:"value_type"`
	Int32     *int32          `json:"int32,omitempty"`
	Float32   *float32        `json:"float32,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type queryResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string           `json:"resultType"`
		DB         string           `json:"db"`
		Result     []queryRespEvent `json:"result"`
	} `json:"data"`
}

type catalogResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		DB         string `json:"db"`
		Result     []struct {
			Name      string `json:"name"`
			ID        uint16 `json:"id"`
			ValueType string `json:"value_type"`
		} `json:"result"`
	} `json:"data"`
}

type aggregateResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		DB         string `json:"db"`
		Window     string `json:"window"`
		Result     []struct {
			TS    int64 `json:"ts"`
			Count int64 `json:"count"`
		} `json:"result"`
	} `json:"data"`
}

// Tests ------------------------------------------------------------

// TestEventsHTTP_ImportAndQuery_RoundTrip is the headline e2e test:
// POST a JSON batch with mixed value types, then GET the events back
// and verify every field round-trips through the wire format.
func TestEventsHTTP_ImportAndQuery_RoundTrip(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")

	body := `[
	  {"db":"sensors","name":"disc.write.slow","ts":1000000,"value":542,"payload":{"path":"/tmp"}},
	  {"db":"sensors","name":"temp.over","ts":2000000,"value":31.25},
	  {"db":"sensors","name":"heartbeat","ts":3000000},
	  {"db":"sensors","name":"disc.write.slow","ts":4000000,"value":870}
	]`
	rec := postEvents(t, eng, body, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var imp importResp
	if err := json.NewDecoder(rec.Body).Decode(&imp); err != nil {
		t.Fatal(err)
	}
	if imp.Status != "success" || imp.Data.Imported != 4 {
		t.Fatalf("import resp wrong: %+v", imp)
	}

	rec = getEvents(t, eng, "db=sensors&start=0&end=5000000")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var q queryResp
	if err := json.NewDecoder(rec.Body).Decode(&q); err != nil {
		t.Fatal(err)
	}
	if q.Data.ResultType != "events" || q.Data.DB != "sensors" || len(q.Data.Result) != 4 {
		t.Fatalf("query resp wrong: %+v", q)
	}

	got := q.Data.Result

	// rec[0]: disc.write.slow with int32 value + JSON payload
	if got[0].Name != "disc.write.slow" || got[0].ValueType != "int32" {
		t.Errorf("rec[0]: name/type = %q/%q", got[0].Name, got[0].ValueType)
	}
	if got[0].Int32 == nil || *got[0].Int32 != 542 {
		t.Errorf("rec[0].Int32 wrong: %v", got[0].Int32)
	}
	if got[0].Float32 != nil {
		t.Errorf("rec[0].Float32 should be nil for int32 event")
	}
	if !bytes.Contains(got[0].Payload, []byte(`"path"`)) {
		t.Errorf("rec[0].Payload missing 'path': %s", got[0].Payload)
	}

	// rec[1]: float32
	if got[1].ValueType != "float32" || got[1].Float32 == nil || *got[1].Float32 != 31.25 {
		t.Errorf("rec[1]: vt=%s float=%v", got[1].ValueType, got[1].Float32)
	}

	// rec[2]: none-typed, no payload
	if got[2].ValueType != "none" || got[2].Int32 != nil || got[2].Float32 != nil {
		t.Errorf("rec[2] should be none-typed with no value fields, got %+v", got[2])
	}
	if len(got[2].Payload) != 0 {
		t.Errorf("rec[2] payload should be omitted, got %s", got[2].Payload)
	}
}

// TestEventsHTTP_ImportSingleObject accepts a bare JSON object in
// addition to an array — handy for single-event emitters.
func TestEventsHTTP_ImportSingleObject(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	rec := postEvents(t, eng, `{"db":"sensors","name":"ev.x","ts":1,"value":7}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var imp importResp
	_ = json.NewDecoder(rec.Body).Decode(&imp)
	if imp.Data.Imported != 1 {
		t.Errorf("Imported = %d, want 1", imp.Data.Imported)
	}
}

// TestEventsHTTP_NameFilter limits GET results to a single event name.
func TestEventsHTTP_NameFilter(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	body := `[
	  {"db":"sensors","name":"disc.write.slow","ts":1,"value":1},
	  {"db":"sensors","name":"heartbeat","ts":2},
	  {"db":"sensors","name":"disc.write.slow","ts":3,"value":2}
	]`
	if rec := postEvents(t, eng, body, "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}

	rec := getEvents(t, eng, "db=sensors&name=disc.write.slow&start=0&end=100")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var q queryResp
	_ = json.NewDecoder(rec.Body).Decode(&q)
	if len(q.Data.Result) != 2 {
		t.Fatalf("filter returned %d, want 2", len(q.Data.Result))
	}
	for _, r := range q.Data.Result {
		if r.Name != "disc.write.slow" {
			t.Errorf("filter leaked: %q", r.Name)
		}
	}
}

// TestEventsHTTP_NameWildcardFilter limits GET results by wildcard pattern.
func TestEventsHTTP_NameWildcardFilter(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	body := `[
	  {"db":"sensors","name":"disc.write.slow","ts":1,"value":1},
	  {"db":"sensors","name":"disc.read.slow","ts":2,"value":2},
	  {"db":"sensors","name":"heartbeat","ts":3}
	]`
	if rec := postEvents(t, eng, body, "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}

	rec := getEvents(t, eng, "db=sensors&name=disc.*.slow&start=0&end=100")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var q queryResp
	_ = json.NewDecoder(rec.Body).Decode(&q)
	if len(q.Data.Result) != 2 {
		t.Fatalf("wildcard filter returned %d, want 2", len(q.Data.Result))
	}
	for _, r := range q.Data.Result {
		if !strings.HasPrefix(r.Name, "disc.") || !strings.HasSuffix(r.Name, ".slow") {
			t.Errorf("wildcard leaked: %q", r.Name)
		}
	}
}

// TestEventsHTTP_LimitCapped ensures the default 100-record limit and
// hard 1000 cap are honored.
func TestEventsHTTP_LimitCapped(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	body := strings.Builder{}
	body.WriteString("[")
	for i := 0; i < 150; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"db":"sensors","name":"ev.x","ts":%d}`, i+1)
	}
	body.WriteString("]")
	if rec := postEvents(t, eng, body.String(), "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}

	// Default limit = 100.
	rec := getEvents(t, eng, "db=sensors&start=0&end=200")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var q queryResp
	_ = json.NewDecoder(rec.Body).Decode(&q)
	if len(q.Data.Result) != 100 {
		t.Errorf("default limit: got %d, want 100", len(q.Data.Result))
	}

	// Explicit larger limit.
	rec = getEvents(t, eng, "db=sensors&start=0&end=200&limit=130")
	_ = json.NewDecoder(rec.Body).Decode(&q)
	if len(q.Data.Result) != 130 {
		t.Errorf("limit=130: got %d, want 130", len(q.Data.Result))
	}

	// Over the hard cap collapses to 1000.
	rec = getEvents(t, eng, "db=sensors&start=0&end=200&limit=5000")
	_ = json.NewDecoder(rec.Body).Decode(&q)
	// We only have 150 events, so we get 150 (which is within both
	// the explicit 5000 and the engine-side hard cap of 1000).
	if len(q.Data.Result) != 150 {
		t.Errorf("limit=5000 (capped to 1000) with 150 events: got %d, want 150", len(q.Data.Result))
	}
}

// TestEventsHTTP_StringValueRejected confirms that JSON string values
// are rejected at ingress — strings belong in payload.
func TestEventsHTTP_StringValueRejected(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	body := `[{"db":"sensors","name":"ev.x","value":"not allowed"}]`
	rec := postEvents(t, eng, body, "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestEventsHTTP_RawNonJSONPayloadIsBase64Wrapped covers the
// docs/EVENTS.md rule: payload bytes that aren't valid JSON come back
// as {"raw_base64":"..."} in the response.
func TestEventsHTTP_RawNonJSONPayloadIsBase64Wrapped(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")

	// Drop a non-JSON payload directly through the engine (the HTTP
	// import path enforces JSON, but other clients — including future
	// nanocli import — won't).
	if err := eng.AddEvent("sensors", "ev.bin", 1, nil, []byte{0xff, 0xfe, 0xfd}); err != nil {
		t.Fatal(err)
	}
	rec := getEvents(t, eng, "db=sensors&start=0&end=100")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var q queryResp
	_ = json.NewDecoder(rec.Body).Decode(&q)
	if len(q.Data.Result) != 1 {
		t.Fatalf("got %d events, want 1", len(q.Data.Result))
	}
	got := q.Data.Result[0].Payload
	if !bytes.Contains(got, []byte(`"raw_base64"`)) {
		t.Errorf("non-JSON payload should be base64-wrapped, got %s", got)
	}
}

// TestEventsHTTP_DisabledDB_409 ensures the HTTP surface maps
// ErrEventsDisabled to a 409 Conflict (clean operator signal that the
// DB needs to opt in via manifest, not a 400 "bad input").
func TestEventsHTTP_DisabledDB_409(t *testing.T) {
	root := t.TempDir()
	// No manifest fixture → DB has events disabled.
	eng, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	// Bootstrap the DB with a metric so getOrCreateDB has materialized it.
	if err := eng.AddSample("plain", "x", 1, int32(1)); err != nil {
		t.Fatal(err)
	}

	rec := postEvents(t, eng, `[{"db":"plain","name":"ev.x"}]`, "application/json")
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST disabled DB: status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	rec = getEvents(t, eng, "db=plain&start=0&end=100")
	if rec.Code != http.StatusConflict {
		t.Fatalf("GET disabled DB: status = %d, want 409", rec.Code)
	}
}

// TestEventsHTTP_MethodNotAllowed checks the method dispatch returns
// 405 for unsupported methods.
func TestEventsHTTP_MethodNotAllowed(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/events", nil)
	rec := httptest.NewRecorder()
	handleEvents(eng)(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: status = %d, want 405", rec.Code)
	}
}

// TestEventsHTTP_ImportValidations covers payload validation errors
// at the JSON parsing boundary.
func TestEventsHTTP_ImportValidations(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	cases := []struct {
		name string
		body string
		ct   string
	}{
		{"empty body", "", "application/json"},
		{"bad ct", `[{"db":"sensors","name":"ev.x"}]`, "text/plain"},
		{"malformed json", `{"db":`, "application/json"},
		{"object value rejected", `[{"db":"sensors","name":"ev.x","value":{"nope":1}}]`, "application/json"},
		{"bool value rejected", `[{"db":"sensors","name":"ev.x","value":true}]`, "application/json"},
		{"int32 overflow rejected", `[{"db":"sensors","name":"ev.x","value":9999999999}]`, "application/json"},
		{"missing db", `[{"name":"ev.x"}]`, "application/json"},
		{"missing name", `[{"db":"sensors"}]`, "application/json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postEvents(t, eng, c.body, c.ct)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestEventsHTTP_QueryValidations covers the query-side input checks.
func TestEventsHTTP_QueryValidations(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	cases := []struct {
		name  string
		query string
	}{
		{"missing db", "start=0&end=100"},
		{"invalid db", "db=/etc/passwd&start=0&end=100"},
		{"bad start", "db=sensors&start=not-a-time"},
		{"bad end", "db=sensors&end=not-a-time"},
		{"inverted range", "db=sensors&start=200&end=100"},
		{"bad limit", "db=sensors&limit=abc"},
		{"zero limit", "db=sensors&limit=0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := getEvents(t, eng, c.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestEventsHTTP_Catalog returns the registered event names + ids.
func TestEventsHTTP_Catalog(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	body := `[
	  {"db":"sensors","name":"disc.write.slow","ts":1,"value":7},
	  {"db":"sensors","name":"heartbeat","ts":2}
	]`
	if rec := postEvents(t, eng, body, "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("import: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/catalog?db=sensors", nil)
	rec := httptest.NewRecorder()
	handleEventsCatalog(eng)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var c catalogResp
	if err := json.NewDecoder(rec.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Data.ResultType != "events_catalog" || c.Data.DB != "sensors" || len(c.Data.Result) != 2 {
		t.Fatalf("catalog wrong: %+v", c)
	}
	byName := map[string]string{}
	for _, e := range c.Data.Result {
		byName[e.Name] = e.ValueType
	}
	if byName["disc.write.slow"] != "int32" || byName["heartbeat"] != "none" {
		t.Errorf("catalog types wrong: %+v", byName)
	}
}

// TestEventsHTTP_Catalog_DisabledDB_409 confirms the catalog endpoint
// also maps disabled DBs to 409.
func TestEventsHTTP_Catalog_DisabledDB_409(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	if err := eng.AddSample("plain", "x", 1, int32(1)); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/catalog?db=plain", nil)
	rec := httptest.NewRecorder()
	handleEventsCatalog(eng)(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestEventsHTTP_AggregateCount(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	body := `[
	  {"db":"sensors","name":"disc.write.slow","ts":1000000000,"value":1},
	  {"db":"sensors","name":"disc.write.slow","ts":2000000000,"value":2},
	  {"db":"sensors","name":"heartbeat","ts":2100000000}
	]`
	if rec := postEvents(t, eng, body, "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}

	rec := getEventsAggregate(t, eng, "db=sensors&start=0&end=3000000000&window=1s")
	if rec.Code != http.StatusOK {
		t.Fatalf("aggregate status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var a aggregateResp
	if err := json.NewDecoder(rec.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	if a.Data.ResultType != "events_aggregate" || a.Data.DB != "sensors" || a.Data.Window != "1s" {
		t.Fatalf("aggregate envelope wrong: %+v", a)
	}
	if len(a.Data.Result) != 2 {
		t.Fatalf("aggregate buckets got %d, want 2", len(a.Data.Result))
	}
	if a.Data.Result[0].TS != 1000000000 || a.Data.Result[0].Count != 1 {
		t.Fatalf("bucket 0 mismatch: %+v", a.Data.Result[0])
	}
	if a.Data.Result[1].TS != 2000000000 || a.Data.Result[1].Count != 2 {
		t.Fatalf("bucket 1 mismatch: %+v", a.Data.Result[1])
	}

	// wildcard filter should keep only disc.*
	rec = getEventsAggregate(t, eng, "db=sensors&name=disc.*&start=0&end=3000000000&window=1s")
	if rec.Code != http.StatusOK {
		t.Fatalf("aggregate wildcard status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	if len(a.Data.Result) != 2 {
		t.Fatalf("aggregate wildcard buckets got %d, want 2", len(a.Data.Result))
	}
	if a.Data.Result[1].Count != 1 {
		t.Fatalf("aggregate wildcard bucket count mismatch: %+v", a.Data.Result[1])
	}
}

func TestEventsHTTP_AggregateValidations(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	cases := []struct {
		name  string
		query string
	}{
		{"missing db", "start=0&end=100&window=1s"},
		{"missing start", "db=sensors&end=100&window=1s"},
		{"missing window", "db=sensors&start=0&end=100"},
		{"bad window", "db=sensors&start=0&end=100&window=nope"},
		{"inverted range", "db=sensors&start=100&end=50&window=1s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := getEventsAggregate(t, eng, c.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestEventsHTTP_ImportThenQueryWithDefaultEnd ensures GET without
// &end uses 'now' as the upper bound (matches the documented default).
func TestEventsHTTP_ImportThenQueryWithDefaultEnd(t *testing.T) {
	eng := newEventsHTTPEngine(t, "sensors")
	// Use a near-current timestamp so it's still <= now when we query.
	now := strconv.FormatInt(1, 10)
	body := `[{"db":"sensors","name":"ev.x","ts":` + now + `}]`
	if rec := postEvents(t, eng, body, "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	rec := getEvents(t, eng, "db=sensors&start=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var q queryResp
	_ = json.NewDecoder(rec.Body).Decode(&q)
	if len(q.Data.Result) != 1 {
		t.Errorf("default end query returned %d events, want 1", len(q.Data.Result))
	}
}
