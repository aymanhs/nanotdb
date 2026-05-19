package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
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
	content := []byte("[engine]\nlisten = \":9998\"\n[wal]\nmax_segment_size = 222\n[web]\nenabled = false\nbase_path = \"/dash\"\nadhoc_path = \"/quick\"\ntitle = \"My Dash\"\nrefresh_seconds = 7\ndashboard_config = \"custom/dashboard.json\"\nweb_root = \"custom/ui\"\napi_base_url = \"https://api.example.test\"\n")
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
	if webCfg.AdhocPath != "/quick" {
		t.Fatalf("web adhoc path override mismatch: got=%q want=/quick", webCfg.AdhocPath)
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
	got, err := parseTimeParam(input)
	if err != nil {
		t.Fatalf("parseTimeParam failed: %v", err)
	}
	want := time.Date(2026, 5, 14, 12, 34, 56, 123456789, time.UTC).UnixNano()
	if int64(got) != want {
		t.Fatalf("parseTimeParam mismatch: got=%d want=%d", int64(got), want)
	}
}

func TestParseTimeParam_IntegerSecondsStillSupported(t *testing.T) {
	got, err := parseTimeParam("1715680000")
	if err != nil {
		t.Fatalf("parseTimeParam failed: %v", err)
	}
	want := int64(1715680000) * int64(time.Second)
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

func TestHandleRollupBackfill(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll prod failed: %v", err)
	}
	manifest := `[rollups]
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

	sensorsManifest := "[rollups]\nenabled = true\n[[rollups.jobs]]\nid = \"temp_1h\"\nsource_metric = \"temp.out_dry\"\ninterval = \"1h\"\naggregates = [\"sum\"]\ndestination_db = \"sensors_rollup_1h\"\ndestination_metric_prefix = \"temp.out_dry\"\n"
	if err := os.WriteFile(filepath.Join(root, "sensors", "manifest.toml"), []byte(sensorsManifest), 0644); err != nil {
		t.Fatalf("WriteFile sensors manifest failed: %v", err)
	}

	rollupManifest := "[rollups]\nenabled = true\n[[rollups.jobs]]\nid = \"temp_1d_from_1h\"\nsource_metric = \"temp.out_dry.sum\"\ninterval = \"24h\"\naggregates = [\"avg\"]\ndestination_db = \"sensors_rollup_1d\"\ndestination_metric_prefix = \"temp.out_dry\"\n"
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
