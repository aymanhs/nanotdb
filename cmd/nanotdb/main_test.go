package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
	"github.com/aymanhs/nanotdb/internal/web"
)

func TestLoadRuntimeConfig(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "engine.toml")
	content := []byte("[engine]\nlisten = \":9999\"\n[wal]\nmax_segment_size = 12345\n")
	if err := os.WriteFile(configPath, content, 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	runtimeCfg, err := loadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("loadRuntimeConfig failed: %v", err)
	}
	listen := runtimeCfg.EngineConfig.Engine.Listen
	dataDir := runtimeCfg.DataDir
	walMaxSeg := runtimeCfg.EngineConfig.WAL.MaxSegmentSize
	webCfg := runtimeCfg.WebConfig
	if listen != ":9999" {
		t.Fatalf("listen mismatch: got=%q want=%q", listen, ":9999")
	}
	if dataDir != root {
		t.Fatalf("dataDir mismatch: got=%q want=%q", dataDir, root)
	}
	if walMaxSeg != 12345 {
		t.Fatalf("walMaxSeg mismatch: got=%d want=%d", walMaxSeg, 12345)
	}
	if runtimeCfg.EngineConfig.Engine.Listen != ":9999" {
		t.Fatalf("listen mismatch: got=%q want=%q", runtimeCfg.EngineConfig.Engine.Listen, ":9999")
	}
	if runtimeCfg.DataDir != root {
		t.Fatalf("dataDir mismatch: got=%q want=%q", runtimeCfg.DataDir, root)
	}
	if runtimeCfg.EngineConfig.WAL.MaxSegmentSize != 12345 {
		t.Fatalf("walMaxSeg mismatch: got=%d want=%d", runtimeCfg.EngineConfig.WAL.MaxSegmentSize, 12345)
	}
	if !webCfg.Enabled {
		t.Fatalf("expected web enabled by default")
	}
	if webCfg.BasePath != "/dashboard" {
		t.Fatalf("web base path mismatch: got=%q want=%q", webCfg.BasePath, "/dashboard")
	}
	if webCfg.ExplorePath != "/explore" {
		t.Fatalf("web explore path mismatch: got=%q want=%q", webCfg.ExplorePath, "/explore")
	}
}

func TestLoadRuntimeConfig_Defaults(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "engine.toml")
	if err := os.WriteFile(configPath, []byte("[engine]\nlisten = \"\"\n"), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	runtimeCfg, err := loadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("loadRuntimeConfig failed: %v", err)
	}
	listen := runtimeCfg.EngineConfig.Engine.Listen
	walMaxSeg := runtimeCfg.EngineConfig.WAL.MaxSegmentSize
	webCfg := runtimeCfg.WebConfig
	if listen != ":8428" {
		t.Fatalf("listen default mismatch: got=%q want=%q", listen, ":8428")
	}
	if walMaxSeg != 64*1024*1024 {
		t.Fatalf("wal max default mismatch: got=%d want=%d", walMaxSeg, 64*1024*1024)
	}
	if runtimeCfg.EngineConfig.Engine.Listen != ":8428" {
		t.Fatalf("listen default mismatch: got=%q want=%q", runtimeCfg.EngineConfig.Engine.Listen, ":8428")
	}
	if runtimeCfg.EngineConfig.WAL.MaxSegmentSize != 64*1024*1024 {
		t.Fatalf("wal max default mismatch: got=%d want=%d", runtimeCfg.EngineConfig.WAL.MaxSegmentSize, 64*1024*1024)
	}
	if webCfg.RefreshSeconds <= 0 {
		t.Fatalf("expected positive default web refresh seconds, got=%d", webCfg.RefreshSeconds)
	}
}

func TestLoadRuntimeConfig_WebOverrides(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "engine.toml")
	content := []byte("[engine]\nlisten = \":9998\"\n[wal]\nmax_segment_size = 222\n[web]\nenabled = false\nbase_path = \"/dash\"\nexplore_path = \"/quick\"\nengine_path = \"/ops\"\ntitle = \"My Dash\"\nrefresh_seconds = 7\ndashboard_config = \"custom/dashboard.json\"\nweb_root = \"custom/ui\"\napi_base_url = \"https://api.example.test\"\n")
	if err := os.WriteFile(configPath, content, 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	runtimeCfg, err := loadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("loadRuntimeConfig failed: %v", err)
	}
	webCfg := runtimeCfg.WebConfig
	if webCfg.Enabled {
		t.Fatalf("expected web disabled override")
	}
	if webCfg.BasePath != "/dash" {
		t.Fatalf("web base path override mismatch: got=%q want=/dash", webCfg.BasePath)
	}
	if webCfg.ExplorePath != "/quick" {
		t.Fatalf("web explore path override mismatch: got=%q want=/quick", webCfg.ExplorePath)
	}
	if webCfg.EnginePath != "/ops" {
		t.Fatalf("web engine path override mismatch: got=%q want=/ops", webCfg.EnginePath)
	}
	if webCfg.Title != "My Dash" {
		t.Fatalf("web title override mismatch: got=%q want=%q", webCfg.Title, "My Dash")
	}
	if webCfg.RefreshSeconds != 7 {
		t.Fatalf("web refresh override mismatch: got=%d want=7", webCfg.RefreshSeconds)
	}
	if webCfg.DashboardFile != "custom/dashboard.json" {
		t.Fatalf("web dashboard config override mismatch: got=%q want=%q", webCfg.DashboardFile, "custom/dashboard.json")
	}
	if webCfg.WebRoot != "custom/ui" {
		t.Fatalf("web root override mismatch: got=%q want=%q", webCfg.WebRoot, "custom/ui")
	}
	if webCfg.APIBaseURL != "https://api.example.test" {
		t.Fatalf("web api base override mismatch: got=%q want=%q", webCfg.APIBaseURL, "https://api.example.test")
	}
}

func TestHandleQueryRangeAggregate(t *testing.T) {
	eng, err := engine.OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	base := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		offset time.Duration
		value  float32
	}{
		{offset: 10 * time.Second, value: 10},
		{offset: 4 * time.Minute, value: 20},
		{offset: 5*time.Minute + 10*time.Second, value: 30},
	} {
		if err := eng.AddSample("prod", "temp.out_dry", engine.Timestamp(base.Add(sample.offset).UnixNano()), sample.value); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?db=prod&query=temp.out_dry&start="+strconv.FormatInt(base.UnixNano(), 10)+"&end="+strconv.FormatInt(base.Add(10*time.Minute).UnixNano(), 10)+"&aggregate=sum,count&window=5m", nil)
	rec := httptest.NewRecorder()
	handleQueryRange(eng)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status mismatch: got=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]interface{}   `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if resp.Status != "success" || resp.Data.ResultType != "matrix" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if len(resp.Data.Result) != 2 {
		t.Fatalf("result length mismatch: got=%d want=2", len(resp.Data.Result))
	}
	if resp.Data.Result[0].Metric["aggregate"] != "sum" || resp.Data.Result[1].Metric["aggregate"] != "count" {
		t.Fatalf("unexpected aggregate labels: %+v", resp.Data.Result)
	}
	if len(resp.Data.Result[0].Values) != 2 || len(resp.Data.Result[1].Values) != 2 {
		t.Fatalf("unexpected values lengths: %+v", resp.Data.Result)
	}
}

func TestHandleQueryRangeAggregate_AllowsMissingEnd(t *testing.T) {
	eng, err := engine.OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	base := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		offset time.Duration
		value  float32
	}{
		{offset: 10 * time.Second, value: 10},
		{offset: 4 * time.Minute, value: 20},
		{offset: 5*time.Minute + 10*time.Second, value: 30},
	} {
		if err := eng.AddSample("prod", "temp.out_dry", engine.Timestamp(base.Add(sample.offset).UnixNano()), sample.value); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?db=prod&query=temp.out_dry&start="+strconv.FormatInt(base.UnixNano(), 10)+"&aggregate=sum&window=5m", nil)
	rec := httptest.NewRecorder()
	handleQueryRange(eng)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status mismatch: got=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Values [][]interface{} `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if resp.Status != "success" || len(resp.Data.Result) != 1 || len(resp.Data.Result[0].Values) != 2 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestHandleQueryRangeStepUsesStableBuckets(t *testing.T) {
	eng, err := engine.OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	base := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		offset time.Duration
		value  float32
	}{
		{offset: 5 * time.Second, value: 1},
		{offset: 20 * time.Second, value: 2},
		{offset: 35 * time.Second, value: 3},
		{offset: 50 * time.Second, value: 4},
	} {
		if err := eng.AddSample("prod", "temp.out_dry", engine.Timestamp(base.Add(sample.offset).UnixNano()), sample.value); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}

	makeReq := func(startOffset, endOffset time.Duration) *httptest.ResponseRecorder {
		req := httptest.NewRequest(
			http.MethodGet,
			"/api/v1/query_range?db=prod&query=temp.out_dry&start="+strconv.FormatInt(base.Add(startOffset).UnixNano(), 10)+
				"&end="+strconv.FormatInt(base.Add(endOffset).UnixNano(), 10)+
				"&step=30s",
			nil,
		)
		rec := httptest.NewRecorder()
		handleQueryRange(eng)(rec, req)
		return rec
	}

	first := makeReq(0, 55*time.Second)
	if first.Code != http.StatusOK {
		t.Fatalf("first status mismatch: got=%d want=%d body=%s", first.Code, http.StatusOK, first.Body.String())
	}
	second := makeReq(10*time.Second, 65*time.Second)
	if second.Code != http.StatusOK {
		t.Fatalf("second status mismatch: got=%d want=%d body=%s", second.Code, http.StatusOK, second.Body.String())
	}

	var firstResp, secondResp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][]interface{}   `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("first unmarshal failed: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondResp); err != nil {
		t.Fatalf("second unmarshal failed: %v", err)
	}
	if len(firstResp.Data.Result) != 1 || len(secondResp.Data.Result) != 1 {
		t.Fatalf("unexpected result counts: first=%+v second=%+v", firstResp.Data.Result, secondResp.Data.Result)
	}
	if len(firstResp.Data.Result[0].Values) != 2 || len(secondResp.Data.Result[0].Values) != 2 {
		t.Fatalf("unexpected value counts: first=%d second=%d", len(firstResp.Data.Result[0].Values), len(secondResp.Data.Result[0].Values))
	}
	if got := firstResp.Data.Result[0].Values[0][1]; got != "1.5" {
		t.Fatalf("unexpected first bucket avg: got=%v want=1.5", got)
	}
	if got := firstResp.Data.Result[0].Values[1][1]; got != "3.5" {
		t.Fatalf("unexpected second bucket avg: got=%v want=3.5", got)
	}
	if got := secondResp.Data.Result[0].Values[0][1]; got != "2" {
		t.Fatalf("unexpected shifted first bucket avg: got=%v want=2", got)
	}
	if got := secondResp.Data.Result[0].Values[1][1]; got != "3.5" {
		t.Fatalf("unexpected shifted second bucket avg: got=%v want=3.5", got)
	}
	if got := firstResp.Data.Result[0].Metric["aggregate"]; got != "avg" {
		t.Fatalf("unexpected aggregate label: got=%q want=avg", got)
	}
	if got := firstResp.Data.Result[0].Metric["window"]; got != "30s" {
		t.Fatalf("unexpected window label: got=%q want=30s", got)
	}
}

func TestHandleAggregatesReturnsEngineRegistry(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/aggregates", nil)
	rec := httptest.NewRecorder()
	handleAggregates()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status mismatch: got=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result  []string `json:"result"`
			Default string   `json:"default"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("unexpected status: %+v", resp)
	}
	if len(resp.Data.Result) == 0 {
		t.Fatal("expected aggregates in response")
	}
	if resp.Data.Default != engine.DefaultStepAggregate() {
		t.Fatalf("default aggregate mismatch: got=%q want=%q", resp.Data.Default, engine.DefaultStepAggregate())
	}
	want := engine.SupportedAggregates()
	if len(resp.Data.Result) != len(want) {
		t.Fatalf("aggregate count mismatch: got=%d want=%d", len(resp.Data.Result), len(want))
	}
	for i := range want {
		if resp.Data.Result[i] != want[i] {
			t.Fatalf("aggregates mismatch at %d: got=%v want=%v", i, resp.Data.Result, want)
		}
	}
}

func TestInitConfigFile_CreatesDefaultConfig(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "engine.toml")
	if err := initConfigFile(configPath); err != nil {
		t.Fatalf("initConfigFile failed: %v", err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("expected config file to exist: %v", err)
	}
}

func TestInitConfigFile_RejectsInvalidConfigName(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "my.toml")
	if err := initConfigFile(configPath); err == nil {
		t.Fatal("expected initConfigFile to reject invalid config file name")
	}
}

func TestParseTimeParam_AcceptsHumanReadable(t *testing.T) {
	input := "2026-05-14 12:34:56.123456789"
	got, err := parseTimeParam(input, "")
	if err != nil {
		t.Fatalf("parseTimeParam failed: %v", err)
	}
	want := time.Date(2026, 5, 14, 12, 34, 56, 123456789, time.UTC).UnixNano()
	if int64(got) != want {
		t.Fatalf("parseTimeParam mismatch: got=%d want=%d", int64(got), want)
	}
}

func TestParseTimeParam_DefaultsToNanoseconds(t *testing.T) {
	// Default unit is nanoseconds — bare integers are no longer heuristically
	// promoted from seconds. Callers wanting seconds must pass tsUnit="s".
	got, err := parseTimeParam("1715680000000000000", "")
	if err != nil {
		t.Fatalf("parseTimeParam failed: %v", err)
	}
	if int64(got) != 1715680000000000000 {
		t.Fatalf("parseTimeParam mismatch: got=%d want=%d", int64(got), int64(1715680000000000000))
	}
}

func TestParseTimeParam_SecondsViaUnit(t *testing.T) {
	got, err := parseTimeParam("1715680000", "s")
	if err != nil {
		t.Fatalf("parseTimeParam failed: %v", err)
	}
	want := int64(1715680000) * int64(time.Second)
	if int64(got) != want {
		t.Fatalf("parseTimeParam mismatch: got=%d want=%d", int64(got), want)
	}
}

func TestParseTimeParam_MillisecondsViaUnit(t *testing.T) {
	// 1700000000000 ms is year 2023; the old heuristic mis-bucketed this as ns
	// (year 1970-01-20). With explicit unit it now resolves correctly.
	got, err := parseTimeParam("1700000000000", "ms")
	if err != nil {
		t.Fatalf("parseTimeParam failed: %v", err)
	}
	want := int64(1700000000000) * int64(time.Millisecond)
	if int64(got) != want {
		t.Fatalf("parseTimeParam mismatch: got=%d want=%d", int64(got), want)
	}
}

func TestHandleDatabases_DefaultAndIncludeInternal(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	if err := eng.AddLine("prod/temp 21"); err != nil {
		t.Fatalf("AddLine prod failed: %v", err)
	}
	if err := eng.AddLine("sensors/humidity 60"); err != nil {
		t.Fatalf("AddLine sensors failed: %v", err)
	}
	if err := eng.AddLine("internal/test.metric 1"); err != nil {
		t.Fatalf("AddLine internal failed: %v", err)
	}

	defaultReq := httptest.NewRequest(http.MethodGet, "/api/v1/databases", nil)
	defaultRec := httptest.NewRecorder()
	handleDatabases(eng)(defaultRec, defaultReq)

	if defaultRec.Code != http.StatusOK {
		t.Fatalf("default status mismatch: got=%d want=%d", defaultRec.Code, http.StatusOK)
	}

	var defaultResp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string   `json:"resultType"`
			Result     []string `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(defaultRec.Body).Decode(&defaultResp); err != nil {
		t.Fatalf("decode default response failed: %v", err)
	}
	if defaultResp.Status != "success" {
		t.Fatalf("default status mismatch: got=%q want=success", defaultResp.Status)
	}
	if defaultResp.Data.ResultType != "databases" {
		t.Fatalf("default resultType mismatch: got=%q want=databases", defaultResp.Data.ResultType)
	}
	wantDefault := []string{"prod", "sensors"}
	if !reflect.DeepEqual(defaultResp.Data.Result, wantDefault) {
		t.Fatalf("default result mismatch: got=%v want=%v", defaultResp.Data.Result, wantDefault)
	}

	withInternalReq := httptest.NewRequest(http.MethodGet, "/api/v1/databases?include_internal=true", nil)
	withInternalRec := httptest.NewRecorder()
	handleDatabases(eng)(withInternalRec, withInternalReq)

	if withInternalRec.Code != http.StatusOK {
		t.Fatalf("include_internal status mismatch: got=%d want=%d", withInternalRec.Code, http.StatusOK)
	}
	var withInternalResp struct {
		Status string `json:"status"`
		Data   struct {
			Result []string `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(withInternalRec.Body).Decode(&withInternalResp); err != nil {
		t.Fatalf("decode include_internal response failed: %v", err)
	}
	wantWithInternal := []string{"internal", "prod", "sensors"}
	if !reflect.DeepEqual(withInternalResp.Data.Result, wantWithInternal) {
		t.Fatalf("include_internal result mismatch: got=%v want=%v", withInternalResp.Data.Result, wantWithInternal)
	}
}

func TestHandleMetrics_NamesAndDetails(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	if err := eng.AddLine("prod/alpha 10"); err != nil {
		t.Fatalf("AddLine alpha failed: %v", err)
	}
	if err := eng.AddLine("prod/beta 12.5"); err != nil {
		t.Fatalf("AddLine beta failed: %v", err)
	}

	namesReq := httptest.NewRequest(http.MethodGet, "/api/v1/metrics?db=prod", nil)
	namesRec := httptest.NewRecorder()
	handleMetrics(eng)(namesRec, namesReq)

	if namesRec.Code != http.StatusOK {
		t.Fatalf("names status mismatch: got=%d want=%d", namesRec.Code, http.StatusOK)
	}
	var namesResp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string   `json:"resultType"`
			DB         string   `json:"db"`
			Result     []string `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(namesRec.Body).Decode(&namesResp); err != nil {
		t.Fatalf("decode names response failed: %v", err)
	}
	if namesResp.Status != "success" {
		t.Fatalf("names status mismatch: got=%q want=success", namesResp.Status)
	}
	if namesResp.Data.ResultType != "metrics" {
		t.Fatalf("names resultType mismatch: got=%q want=metrics", namesResp.Data.ResultType)
	}
	if namesResp.Data.DB != "prod" {
		t.Fatalf("names db mismatch: got=%q want=prod", namesResp.Data.DB)
	}
	wantNames := []string{"alpha", "beta"}
	if !reflect.DeepEqual(namesResp.Data.Result, wantNames) {
		t.Fatalf("names result mismatch: got=%v want=%v", namesResp.Data.Result, wantNames)
	}

	detailsReq := httptest.NewRequest(http.MethodGet, "/api/v1/metrics?db=prod&details=true", nil)
	detailsRec := httptest.NewRecorder()
	handleMetrics(eng)(detailsRec, detailsReq)

	if detailsRec.Code != http.StatusOK {
		t.Fatalf("details status mismatch: got=%d want=%d", detailsRec.Code, http.StatusOK)
	}
	var detailsResp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Name string `json:"name"`
				ID   uint16 `json:"id"`
				Type string `json:"type"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(detailsRec.Body).Decode(&detailsResp); err != nil {
		t.Fatalf("decode details response failed: %v", err)
	}
	if len(detailsResp.Data.Result) != 2 {
		t.Fatalf("details result length mismatch: got=%d want=2", len(detailsResp.Data.Result))
	}
	if detailsResp.Data.Result[0].Name != "alpha" {
		t.Fatalf("details first entry mismatch: got=%+v", detailsResp.Data.Result[0])
	}
	if detailsResp.Data.Result[0].Type != "int32" && detailsResp.Data.Result[0].Type != "float32" {
		t.Fatalf("details first entry type mismatch: got=%q", detailsResp.Data.Result[0].Type)
	}
	if detailsResp.Data.Result[1].Name != "beta" {
		t.Fatalf("details second entry mismatch: got=%+v", detailsResp.Data.Result[1])
	}
	if detailsResp.Data.Result[1].Type != "float32" {
		t.Fatalf("details second entry type mismatch: got=%q want=float32", detailsResp.Data.Result[1].Type)
	}
}

func TestHandleMetrics_DatabaseNotFound(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics?db=does-not-exist", nil)
	rec := httptest.NewRecorder()
	handleMetrics(eng)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status mismatch: got=%d want=%d", rec.Code, http.StatusNotFound)
	}
	var resp struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if resp.Status != "error" || resp.ErrorType != "not_found" {
		t.Fatalf("error payload mismatch: got status=%q type=%q", resp.Status, resp.ErrorType)
	}
}

func TestHandleEngineOverviewAndRuntime(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	if err := eng.AddLine("prod/temp.office 21.5"); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}

	overviewReq := httptest.NewRequest(http.MethodGet, "/api/engine/overview", nil)
	overviewRec := httptest.NewRecorder()
	overviewCfg := runtimeConfig{
		EngineConfig: engine.EngineConfig{
			Engine:     engine.EngineConfigEngine{Listen: ":8428"},
			WAL:        engine.EngineConfigWAL{MaxSegmentSize: 67108864, FsyncPolicy: engine.WALFsyncPolicySegment},
			Durability: engine.EngineConfigDurability{Profile: engine.DurabilityProfileStrict},
			Stats:      engine.EngineConfigStats{Enabled: true, Interval: "30s"},
		},
		DBDefaults: engine.DBInfo{Grace: "5m", RetentionDays: 30, MaxActiveDays: 2, Partition: "day", WALEnabled: true, WALSkipBefore: "1h", PageMaxRecords: 16000, PageMaxBytes: 192000, PageMaxAge: "60s"},
		WebConfig:  web.Config{Enabled: true, BasePath: "/dashboard", ExplorePath: "/explore", EnginePath: "/engine"},
	}
	handleEngineOverview(eng, overviewCfg)(overviewRec, overviewReq)
	if overviewRec.Code != http.StatusOK {
		t.Fatalf("overview status mismatch: got=%d want=200 body=%s", overviewRec.Code, overviewRec.Body.String())
	}
	var overviewResp struct {
		Status string `json:"status"`
		Data   struct {
			Settings struct {
				WALFsyncPolicy    string `json:"wal_fsync_policy"`
				DurabilityProfile string `json:"durability_profile"`
				StatsInterval     string `json:"stats_interval"`
			} `json:"settings"`
			Result []struct {
				Name        string `json:"name"`
				MetricCount int    `json:"metric_count"`
				OpenPages   int    `json:"open_pages"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(overviewRec.Body).Decode(&overviewResp); err != nil {
		t.Fatalf("decode overview failed: %v", err)
	}
	if overviewResp.Status != "success" || len(overviewResp.Data.Result) == 0 {
		t.Fatalf("unexpected overview payload: %+v", overviewResp)
	}
	if overviewResp.Data.Settings.WALFsyncPolicy != engine.WALFsyncPolicySegment || overviewResp.Data.Settings.DurabilityProfile != engine.DurabilityProfileStrict || overviewResp.Data.Settings.StatsInterval != "30s" {
		t.Fatalf("unexpected overview settings payload: %+v", overviewResp.Data.Settings)
	}
	prodIdx := -1
	for i := range overviewResp.Data.Result {
		if overviewResp.Data.Result[i].Name == "prod" {
			prodIdx = i
			break
		}
	}
	if prodIdx < 0 {
		t.Fatalf("expected prod in overview payload: %+v", overviewResp.Data.Result)
	}
	if overviewResp.Data.Result[prodIdx].MetricCount != 1 {
		t.Fatalf("overview result mismatch: %+v", overviewResp.Data.Result[prodIdx])
	}

	runtimeReq := httptest.NewRequest(http.MethodGet, "/api/engine/runtime?db=prod", nil)
	runtimeRec := httptest.NewRecorder()
	handleEngineRuntime(eng)(runtimeRec, runtimeReq)
	if runtimeRec.Code != http.StatusOK {
		t.Fatalf("runtime status mismatch: got=%d want=200 body=%s", runtimeRec.Code, runtimeRec.Body.String())
	}
	var runtimeResp struct {
		Status string `json:"status"`
		Data   struct {
			Result struct {
				Runtime struct {
					Stats struct {
						WAL struct {
							AppendCount  int64     `json:"AppendCount"`
							FlushCount   int64     `json:"FlushCount"`
							LastAppendAt time.Time `json:"LastAppendAt"`
							LastFlushAt  time.Time `json:"LastFlushAt"`
						} `json:"WAL"`
					} `json:"stats"`
					OpenPages []struct {
						Day     string `json:"day"`
						Records int    `json:"records"`
					} `json:"open_pages"`
				} `json:"runtime"`
				WALPreview struct {
					Total int `json:"total"`
					First []struct {
						MetricName string `json:"metric_name"`
					} `json:"first"`
				} `json:"wal_preview"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(runtimeRec.Body).Decode(&runtimeResp); err != nil {
		t.Fatalf("decode runtime failed: %v", err)
	}
	if runtimeResp.Status != "success" {
		t.Fatalf("runtime payload mismatch: %+v", runtimeResp)
	}
	if len(runtimeResp.Data.Result.Runtime.OpenPages) == 0 {
		t.Fatalf("expected open page in runtime payload")
	}
	if runtimeResp.Data.Result.WALPreview.Total == 0 || len(runtimeResp.Data.Result.WALPreview.First) == 0 || runtimeResp.Data.Result.WALPreview.First[0].MetricName != "temp.office" {
		t.Fatalf("expected named WAL preview records in runtime payload: %+v", runtimeResp.Data.Result.WALPreview)
	}
	if runtimeResp.Data.Result.Runtime.Stats.WAL.AppendCount == 0 {
		t.Fatalf("expected WAL append count in runtime payload: %+v", runtimeResp.Data.Result.Runtime.Stats.WAL)
	}
	if runtimeResp.Data.Result.Runtime.Stats.WAL.LastAppendAt.IsZero() {
		t.Fatalf("expected last WAL append time in runtime payload: %+v", runtimeResp.Data.Result.Runtime.Stats.WAL)
	}

	runtimeOverviewReq := httptest.NewRequest(http.MethodGet, "/api/engine/runtime", nil)
	runtimeOverviewRec := httptest.NewRecorder()
	handleEngineRuntime(eng)(runtimeOverviewRec, runtimeOverviewReq)
	if runtimeOverviewRec.Code != http.StatusOK {
		t.Fatalf("runtime overview status mismatch: got=%d want=200 body=%s", runtimeOverviewRec.Code, runtimeOverviewRec.Body.String())
	}
	var runtimeOverviewResp struct {
		Status string `json:"status"`
		Data   struct {
			Result struct {
				DatabaseCount       int `json:"database_count"`
				ActiveDatabaseCount int `json:"active_database_count"`
				Process             struct {
					StartedAt time.Time `json:"started_at"`
					RSSBytes  int64     `json:"rss_bytes"`
				} `json:"process"`
				GoMem struct {
					HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
				} `json:"go_mem"`
				OpenPages []struct {
					Database string `json:"database"`
					Day      string `json:"day"`
				} `json:"open_pages"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(runtimeOverviewRec.Body).Decode(&runtimeOverviewResp); err != nil {
		t.Fatalf("decode runtime overview failed: %v", err)
	}
	if runtimeOverviewResp.Status != "success" {
		t.Fatalf("runtime overview payload mismatch: %+v", runtimeOverviewResp)
	}
	if runtimeOverviewResp.Data.Result.DatabaseCount == 0 || runtimeOverviewResp.Data.Result.ActiveDatabaseCount == 0 {
		t.Fatalf("expected database counts in runtime overview: %+v", runtimeOverviewResp.Data.Result)
	}
	if runtimeOverviewResp.Data.Result.Process.StartedAt.IsZero() {
		t.Fatalf("expected process start time in runtime overview: %+v", runtimeOverviewResp.Data.Result.Process)
	}
	if runtimeOverviewResp.Data.Result.GoMem.HeapAllocBytes == 0 {
		t.Fatalf("expected Go memstats in runtime overview: %+v", runtimeOverviewResp.Data.Result.GoMem)
	}
	if len(runtimeOverviewResp.Data.Result.OpenPages) == 0 || runtimeOverviewResp.Data.Result.OpenPages[0].Database == "" {
		t.Fatalf("expected database-tagged open pages in runtime overview: %+v", runtimeOverviewResp.Data.Result.OpenPages)
	}
}

func TestHandleEngineOverviewIncludesOnDiskRollupDB(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	if err := eng.AddLine("metrics/temp.office 21.5"); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	rollupDir := filepath.Join(root, "metrics_rollup_1h")
	if err := os.MkdirAll(rollupDir, 0755); err != nil {
		t.Fatalf("MkdirAll rollup failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rollupDir, "manifest.toml"), []byte("[retention]\nretention_days = 30\nretention_action = \"keep\"\n"), 0644); err != nil {
		t.Fatalf("WriteFile rollup manifest failed: %v", err)
	}

	overviewCfg := runtimeConfig{
		EngineConfig: engine.EngineConfig{
			Engine:     engine.EngineConfigEngine{Listen: ":8428"},
			WAL:        engine.EngineConfigWAL{MaxSegmentSize: 67108864, FsyncPolicy: engine.WALFsyncPolicySegment},
			Durability: engine.EngineConfigDurability{Profile: engine.DurabilityProfileStrict},
			Stats:      engine.EngineConfigStats{Enabled: true, Interval: "30s"},
		},
		DBDefaults: engine.DBInfo{Grace: "5m", RetentionDays: 30, MaxActiveDays: 2, Partition: "day", WALEnabled: true, WALSkipBefore: "1h", PageMaxRecords: 16000, PageMaxBytes: 192000, PageMaxAge: "60s"},
		WebConfig:  web.Config{Enabled: true, BasePath: "/dashboard", ExplorePath: "/explore", EnginePath: "/engine"},
	}
	overviewReq := httptest.NewRequest(http.MethodGet, "/api/engine/overview", nil)
	overviewRec := httptest.NewRecorder()
	handleEngineOverview(eng, overviewCfg)(overviewRec, overviewReq)
	if overviewRec.Code != http.StatusOK {
		t.Fatalf("overview status mismatch: got=%d want=200 body=%s", overviewRec.Code, overviewRec.Body.String())
	}
	var overviewResp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Name string `json:"name"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(overviewRec.Body).Decode(&overviewResp); err != nil {
		t.Fatalf("decode overview failed: %v", err)
	}
	gotNames := make([]string, 0, len(overviewResp.Data.Result))
	for _, item := range overviewResp.Data.Result {
		gotNames = append(gotNames, item.Name)
	}
	if !reflect.DeepEqual(gotNames, []string{"internal", "metrics", "metrics_rollup_1h"}) {
		t.Fatalf("overview database names mismatch: got=%v", gotNames)
	}

	dbReq := httptest.NewRequest(http.MethodGet, "/api/v1/databases", nil)
	dbRec := httptest.NewRecorder()
	handleDatabases(eng)(dbRec, dbReq)
	if dbRec.Code != http.StatusOK {
		t.Fatalf("databases status mismatch: got=%d want=200 body=%s", dbRec.Code, dbRec.Body.String())
	}
	var dbResp struct {
		Status string `json:"status"`
		Data   struct {
			Result []string `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(dbRec.Body).Decode(&dbResp); err != nil {
		t.Fatalf("decode databases failed: %v", err)
	}
	if !reflect.DeepEqual(dbResp.Data.Result, []string{"metrics", "metrics_rollup_1h"}) {
		t.Fatalf("databases result mismatch: got=%v", dbResp.Data.Result)
	}
}

func TestHandleEngineDatabaseIncludesLastValue(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	if err := eng.AddLine("prod/temp.office 21.5 1715000000000000000"); err != nil {
		t.Fatalf("AddLine first failed: %v", err)
	}
	if err := eng.AddLine("prod/temp.office 22.75 1715000001000000000"); err != nil {
		t.Fatalf("AddLine second failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/engine/database?db=prod", nil)
	rec := httptest.NewRecorder()
	handleEngineDatabase(eng)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("database status mismatch: got=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result struct {
				Metrics []struct {
					Name          string `json:"name"`
					LastValue     string `json:"last_value"`
					LastTimestamp string `json:"last_timestamp"`
					LastTSNS      int64  `json:"last_timestamp_ns"`
				} `json:"metrics"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode database failed: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("unexpected database payload: %+v", resp)
	}
	if len(resp.Data.Result.Metrics) != 1 {
		t.Fatalf("expected one metric, got=%d", len(resp.Data.Result.Metrics))
	}
	metric := resp.Data.Result.Metrics[0]
	if metric.Name != "temp.office" {
		t.Fatalf("metric name mismatch: %+v", metric)
	}
	if metric.LastValue != "22.75" {
		t.Fatalf("last value mismatch: got=%q want=22.75", metric.LastValue)
	}
	if metric.LastTSNS != 1715000001000000000 {
		t.Fatalf("last timestamp ns mismatch: got=%d want=%d", metric.LastTSNS, int64(1715000001000000000))
	}
	if metric.LastTimestamp == "" {
		t.Fatalf("expected formatted last timestamp")
	}
}

func TestHandleEngineFiles(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	if err := eng.AddLine("prod/temp.office 21.5 1715000000000000000"); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := eng.AddLine("prod/temp.office 22.5 1715086400000000000"); err != nil {
		t.Fatalf("AddLine second day failed: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	eng, err = engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("reopen engine failed: %v", err)
	}
	defer eng.Close()

	dataPaths, err := filepath.Glob(filepath.Join(root, "prod", "data-*.dat"))
	if err != nil {
		t.Fatalf("Glob data files failed: %v", err)
	}
	if len(dataPaths) < 2 {
		t.Fatalf("expected at least two data files, got=%v", dataPaths)
	}
	selectedFile := dataPaths[0]
	filesReq := httptest.NewRequest(http.MethodGet, "/api/engine/files?db=prod&data_file="+url.QueryEscape(selectedFile), nil)
	filesRec := httptest.NewRecorder()
	handleEngineFiles(eng)(filesRec, filesReq)
	if filesRec.Code != http.StatusOK {
		t.Fatalf("files status mismatch: got=%d want=200 body=%s", filesRec.Code, filesRec.Body.String())
	}
	var filesResp struct {
		Status string `json:"status"`
		Data   struct {
			Result struct {
				Database string `json:"database"`
				Data     []struct {
					Path   string `json:"path"`
					Frames int    `json:"frames"`
					Pages  []struct {
						UncompressedLen      uint64  `json:"uncompressed_len"`
						DurationNS           int64   `json:"duration_ns"`
						AvgDiskBytesPerPoint float64 `json:"avg_disk_bytes_per_point"`
					} `json:"pages"`
				} `json:"data"`
				Metric []struct {
					Path            string `json:"path"`
					Frames          int    `json:"frames"`
					DistinctMetrics int    `json:"distinct_metrics"`
					Points          int64  `json:"points"`
					AvgPayloadBytes int64  `json:"avg_payload_bytes"`
				} `json:"metric"`
				WAL []struct {
					Path    string `json:"path"`
					Records int    `json:"records"`
				} `json:"wal"`
				RecordPreview struct {
					Total int `json:"total"`
					First []struct {
						MetricName string `json:"metric_name"`
					} `json:"first"`
				} `json:"record_preview"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(filesRec.Body).Decode(&filesResp); err != nil {
		t.Fatalf("decode files failed: %v", err)
	}
	if filesResp.Status != "success" || filesResp.Data.Result.Database != "prod" {
		t.Fatalf("files payload mismatch: %+v", filesResp)
	}
	if len(filesResp.Data.Result.Data) < 2 || filesResp.Data.Result.Data[0].Frames == 0 {
		t.Fatalf("expected scanned .dat frames in files payload: %+v", filesResp.Data.Result.Data)
	}
	if len(filesResp.Data.Result.WAL) == 0 {
		t.Fatalf("expected WAL file summaries in files payload: %+v", filesResp.Data.Result)
	}
	if filesResp.Data.Result.RecordPreview.Total < 0 {
		t.Fatalf("expected non-negative WAL preview total: %+v", filesResp.Data.Result.RecordPreview)
	}
	if len(filesResp.Data.Result.Metric) != 0 {
		t.Fatalf("expected no metric summaries before metric files exist: %+v", filesResp.Data.Result.Metric)
	}
	selectedFound := false
	for _, item := range filesResp.Data.Result.Data {
		if item.Path == selectedFile {
			selectedFound = true
			if len(item.Pages) == 0 {
				t.Fatalf("expected scanned page details for selected file: %+v", item)
			}
			if item.Pages[0].UncompressedLen == 0 {
				t.Fatalf("expected positive uncompressed page size: %+v", item.Pages[0])
			}
			if item.Pages[0].AvgDiskBytesPerPoint <= 0 {
				t.Fatalf("expected positive average disk bytes per point: %+v", item.Pages[0])
			}
			if item.Pages[0].DurationNS < 0 {
				t.Fatalf("expected non-negative page duration: %+v", item.Pages[0])
			}
			continue
		}
		if len(item.Pages) != 0 {
			t.Fatalf("expected unselected files to omit page details: %+v", item)
		}
	}
	if !selectedFound {
		t.Fatalf("selected file missing from files payload: selected=%q data=%+v", selectedFile, filesResp.Data.Result.Data)
	}

	part := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(selectedFile), "data-"), ".dat")
	if _, err := eng.BuildMetricFileV2("prod", part); err != nil {
		t.Fatalf("BuildMetricFileV2 failed: %v", err)
	}
	filesReq = httptest.NewRequest(http.MethodGet, "/api/engine/files?db=prod&data_file="+url.QueryEscape(selectedFile), nil)
	filesRec = httptest.NewRecorder()
	handleEngineFiles(eng)(filesRec, filesReq)
	if filesRec.Code != http.StatusOK {
		t.Fatalf("files with metric status mismatch: got=%d want=200 body=%s", filesRec.Code, filesRec.Body.String())
	}
	if err := json.NewDecoder(filesRec.Body).Decode(&filesResp); err != nil {
		t.Fatalf("decode files with metric failed: %v", err)
	}
	if len(filesResp.Data.Result.Metric) == 0 {
		t.Fatalf("expected metric summaries after metric file build: %+v", filesResp.Data.Result)
	}
	if filesResp.Data.Result.Metric[0].Frames == 0 || filesResp.Data.Result.Metric[0].DistinctMetrics == 0 || filesResp.Data.Result.Metric[0].Points == 0 {
		t.Fatalf("expected populated metric summary: %+v", filesResp.Data.Result.Metric[0])
	}
	if filesResp.Data.Result.Metric[0].AvgPayloadBytes <= 0 {
		t.Fatalf("expected positive metric avg payload bytes: %+v", filesResp.Data.Result.Metric[0])
	}
}

func TestHandleEngineCompactMetric(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	base := time.Date(2024, time.January, 3, 12, 0, 0, 0, time.UTC)
	part := base.Format("2006-01-02")
	if err := eng.AddLine("prod/temp.office 21.5 " + strconv.FormatInt(base.UnixNano(), 10)); err != nil {
		t.Fatalf("AddLine first failed: %v", err)
	}
	if err := eng.AddLine("prod/temp.office 22.5 " + strconv.FormatInt(base.Add(5*time.Minute).UnixNano(), 10)); err != nil {
		t.Fatalf("AddLine second failed: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	eng, err = engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("reopen engine failed: %v", err)
	}
	defer eng.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/engine/compact_metric", bytes.NewBufferString(`{"db":"prod","part":"`+part+`"}`))
	rec := httptest.NewRecorder()
	handleEngineCompactMetric(eng)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status mismatch: got=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result struct {
				Database    string `json:"database"`
				Part        string `json:"part"`
				DataPath    string `json:"data_path"`
				MetricPath  string `json:"metric_path"`
				DataBytes   int64  `json:"data_bytes"`
				MetricBytes int64  `json:"metric_bytes"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode compact metric failed: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("unexpected compact metric payload: %+v", resp)
	}
	if resp.Data.Result.Database != "prod" || resp.Data.Result.Part != part {
		t.Fatalf("unexpected compact metric identity: %+v", resp.Data.Result)
	}
	if resp.Data.Result.DataBytes <= 0 || resp.Data.Result.MetricBytes <= 0 {
		t.Fatalf("expected positive size info from compact metric response: %+v", resp.Data.Result)
	}
	if _, err := os.Stat(resp.Data.Result.MetricPath); err != nil {
		t.Fatalf("expected metric file to exist after compact metric: %v", err)
	}
}

func TestHandleEngineRecompact(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll prod failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), []byte("[retention]\nretention_action = \"keep\"\n\n[page]\nmax_records = 1\nmax_bytes = 64\nmax_age = \"10m\"\n"), 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	base := time.Date(2024, time.January, 3, 12, 0, 0, 0, time.UTC)
	part := base.Format("2006-01-02")
	if err := eng.AddLine("prod/temp.office 21.5 " + strconv.FormatInt(base.UnixNano(), 10)); err != nil {
		t.Fatalf("AddLine first failed: %v", err)
	}
	if err := eng.AddLine("prod/temp.office 22.5 " + strconv.FormatInt(base.Add(5*time.Minute).UnixNano(), 10)); err != nil {
		t.Fatalf("AddLine second failed: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), []byte("[retention]\nretention_action = \"keep\"\n\n[page]\nmax_records = 128\nmax_bytes = 4096\nmax_age = \"10m\"\n"), 0644); err != nil {
		t.Fatalf("WriteFile manifest update failed: %v", err)
	}

	eng, err = engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("reopen engine failed: %v", err)
	}
	defer eng.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/engine/recompact", bytes.NewBufferString(`{"db":"prod","part":"`+part+`"}`))
	rec := httptest.NewRecorder()
	handleEngineRecompact(eng)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status mismatch: got=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     struct {
				Database  string `json:"database"`
				Part      string `json:"part"`
				OldFrames int    `json:"old_frames"`
				NewFrames int    `json:"new_frames"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if resp.Status != "success" || resp.Data.ResultType != "engine_recompact" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Data.Result.Database != "prod" || resp.Data.Result.Part != part {
		t.Fatalf("unexpected result identity: %+v", resp.Data.Result)
	}
	if resp.Data.Result.NewFrames >= resp.Data.Result.OldFrames {
		t.Fatalf("expected fewer frames after recompact: %+v", resp.Data.Result)
	}
}

func TestHandleRollupBackfill(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll prod failed: %v", err)
	}
	manifest := `[retention]
retention_action = "keep"

[rollups]
enabled = true
checkpoint_file = "rollup.checkpoints.log"
default_grace = "0s"

[[rollups.jobs]]
id = "temp_1h"
source_metric = "temp.office"
interval = "1h"
aggregates = ["sum"]
destination_db = "prod_rollup_1h"
	destination_metric_prefix = "temp.office"
`
	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), []byte(manifest), 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-3 * time.Hour).Truncate(time.Hour)
	if err := eng.AddLine("prod/temp.office 10 " + strconv.FormatInt(base.Add(10*time.Minute).UnixNano(), 10)); err != nil {
		t.Fatalf("AddLine first sample failed: %v", err)
	}
	if err := eng.AddLine("prod/temp.office 20 " + strconv.FormatInt(base.Add(30*time.Minute).UnixNano(), 10)); err != nil {
		t.Fatalf("AddLine second sample failed: %v", err)
	}
	if err := eng.AddLine("prod/temp.office 1 " + strconv.FormatInt(base.Add(1*time.Hour).UnixNano(), 10)); err != nil {
		t.Fatalf("AddLine close sample failed: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close before backfill failed: %v", err)
	}

	eng, err = engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("Re-open engine failed: %v", err)
	}
	defer eng.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rollup/backfill", bytes.NewBufferString(`{"source_db":"prod"}`))
	rec := httptest.NewRecorder()
	handleRollupBackfill(eng)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status mismatch: got=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string                      `json:"resultType"`
			Result     engine.RollupBackfillReport `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode response failed: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("status mismatch: got=%q want=success", resp.Status)
	}
	if resp.Data.ResultType != "rollup_backfill" {
		t.Fatalf("resultType mismatch: got=%q want=rollup_backfill", resp.Data.ResultType)
	}
	if len(resp.Data.Result.DestinationDatabases) != 1 || resp.Data.Result.DestinationDatabases[0] != "prod_rollup_1h" {
		t.Fatalf("unexpected destination databases: %v", resp.Data.Result.DestinationDatabases)
	}

	if _, found, err := eng.QueryLast("prod_rollup_1h", "temp.office.sum"); err != nil {
		t.Fatalf("QueryLast rollup failed: %v", err)
	} else if !found {
		t.Fatalf("expected rollup sample after handler backfill")
	}
}

func TestHandleMetrics_LineageValidationErrors(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	if err := eng.AddLine("prod/alpha 1"); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}

	lineageWithoutDetailsReq := httptest.NewRequest(http.MethodGet, "/api/v1/metrics?db=prod&lineage=rollups", nil)
	lineageWithoutDetailsRec := httptest.NewRecorder()
	handleMetrics(eng)(lineageWithoutDetailsRec, lineageWithoutDetailsReq)
	if lineageWithoutDetailsRec.Code != http.StatusBadRequest {
		t.Fatalf("lineage/details status mismatch: got=%d want=%d", lineageWithoutDetailsRec.Code, http.StatusBadRequest)
	}

	invalidMaxHopsReq := httptest.NewRequest(http.MethodGet, "/api/v1/metrics?db=prod&details=true&lineage=rollups&max_hops=9", nil)
	invalidMaxHopsRec := httptest.NewRecorder()
	handleMetrics(eng)(invalidMaxHopsRec, invalidMaxHopsReq)
	if invalidMaxHopsRec.Code != http.StatusBadRequest {
		t.Fatalf("max_hops status mismatch: got=%d want=%d", invalidMaxHopsRec.Code, http.StatusBadRequest)
	}
}

func TestHandleMetrics_LineageMultiHop(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sensors"), 0755); err != nil {
		t.Fatalf("MkdirAll sensors failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sensors_rollup_1h"), 0755); err != nil {
		t.Fatalf("MkdirAll sensors_rollup_1h failed: %v", err)
	}

	sensorsManifest := "[retention]\nretention_action = \"keep\"\n[rollups]\nenabled = true\n[[rollups.jobs]]\nid = \"temp_1h\"\nsource_metric = \"temp.out_dry\"\ninterval = \"1h\"\naggregates = [\"sum\"]\ndestination_db = \"sensors_rollup_1h\"\ndestination_metric_prefix = \"temp.out_dry\"\n"
	if err := os.WriteFile(filepath.Join(root, "sensors", "manifest.toml"), []byte(sensorsManifest), 0644); err != nil {
		t.Fatalf("WriteFile sensors manifest failed: %v", err)
	}

	rollupManifest := "[retention]\nretention_action = \"keep\"\n[rollups]\nenabled = true\n[[rollups.jobs]]\nid = \"temp_1d_from_1h\"\nsource_metric = \"temp.out_dry.sum\"\ninterval = \"24h\"\naggregates = [\"avg\"]\ndestination_db = \"sensors_rollup_1d\"\ndestination_metric_prefix = \"temp.out_dry\"\n"
	if err := os.WriteFile(filepath.Join(root, "sensors_rollup_1h", "manifest.toml"), []byte(rollupManifest), 0644); err != nil {
		t.Fatalf("WriteFile rollup manifest failed: %v", err)
	}

	eng, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer eng.Close()

	if err := eng.AddLine("sensors/temp.out_dry 21.5 1715000000000000000"); err != nil {
		t.Fatalf("AddLine source failed: %v", err)
	}
	if err := eng.AddLine("sensors_rollup_1h/temp.out_dry.sum 21.5 1715000000000000000"); err != nil {
		t.Fatalf("AddLine hop1 failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics?db=sensors&details=true&lineage=rollups&max_hops=2", nil)
	rec := httptest.NewRecorder()
	handleMetrics(eng)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status mismatch: got=%d want=%d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Name    string `json:"name"`
				Rollups *struct {
					Downstream []struct {
						Hop       int    `json:"hop"`
						JobID     string `json:"job_id"`
						DB        string `json:"db"`
						Metric    string `json:"metric"`
						Aggregate string `json:"aggregate"`
					} `json:"downstream"`
					MaxHops int `json:"max_hops"`
				} `json:"rollups"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if resp.Status != "success" {
		t.Fatalf("status payload mismatch: got=%q want=success", resp.Status)
	}
	if len(resp.Data.Result) == 0 {
		t.Fatalf("expected metrics in response")
	}

	targetIdx := -1
	for i := range resp.Data.Result {
		if resp.Data.Result[i].Name == "temp.out_dry" {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		t.Fatalf("expected temp.out_dry metric in response")
	}
	target := resp.Data.Result[targetIdx]
	if target.Rollups == nil {
		t.Fatalf("expected rollups in details response")
	}
	if target.Rollups.MaxHops != 2 {
		t.Fatalf("max_hops mismatch: got=%d want=2", target.Rollups.MaxHops)
	}
	if len(target.Rollups.Downstream) < 2 {
		t.Fatalf("expected at least two downstream rollup entries, got=%d", len(target.Rollups.Downstream))
	}

	var foundHop1 bool
	var foundHop2 bool
	for _, d := range target.Rollups.Downstream {
		if d.Hop == 1 && d.DB == "sensors_rollup_1h" && d.Metric == "temp.out_dry.sum" && d.JobID == "temp_1h" && d.Aggregate == "sum" {
			foundHop1 = true
		}
		if d.Hop == 2 && d.DB == "sensors_rollup_1d" && d.Metric == "temp.out_dry.avg" && d.JobID == "temp_1d_from_1h" && d.Aggregate == "avg" {
			foundHop2 = true
		}
	}
	if !foundHop1 {
		t.Fatalf("missing hop=1 downstream entry")
	}
	if !foundHop2 {
		t.Fatalf("missing hop=2 downstream entry")
	}
}
