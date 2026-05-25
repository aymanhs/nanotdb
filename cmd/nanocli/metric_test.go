package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func TestRunMetricBuild_BuildsAllPartitionsWithOverrides(t *testing.T) {
	root := t.TempDir()
	engineTOML := testImportEngineTOML + `
[metrics]
enabled = false
compression = "zstd_fastest"
raw_ingest_action = "keep"
`
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(engineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	base := time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		ts     time.Time
		metric string
		value  float32
	}{
		{ts: base, metric: "cpu.temp", value: 40.5},
		{ts: base.Add(5 * time.Minute), metric: "cpu.temp", value: 41.0},
		{ts: base.Add(24 * time.Hour), metric: "cpu.temp", value: 42.0},
	} {
		if err := e.AddSample("prod", sample.metric, engine.Timestamp(sample.ts.UnixNano()), sample.value); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	got := captureStdout(t, func() {
		if err := runMetricBuild([]string{"--root", root, "--db", "prod", "--codec", "s2", "--verify"}); err != nil {
			t.Fatalf("runMetricBuild failed: %v", err)
		}
	})

	if !strings.Contains(got, "codec=s2") {
		t.Fatalf("expected codec override in output, got:\n%s", got)
	}
	if !strings.Contains(got, "format=v2") {
		t.Fatalf("expected default v2 format in output, got:\n%s", got)
	}
	for _, partition := range []string{"2026-05-03", "2026-05-04"} {
		if _, err := os.Stat(filepath.Join(root, "prod", "metric-"+partition+".dat")); err != nil {
			t.Fatalf("metric file missing for %s: %v", partition, err)
		}
	}
}

func TestRunQueryRejectsInvalidMetricFilesOverride(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll prod failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "prod", "catalog.json"), []byte(`{"metrics":[]}`), 0644); err != nil {
		t.Fatalf("WriteFile catalog failed: %v", err)
	}

	err := runQuery([]string{"--root", root, "--db", "prod", "--metric-files", "maybe"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "invalid --metric-files") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunQueryAggregateJSON(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	base := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		offset time.Duration
		value  float32
	}{
		{offset: 10 * time.Second, value: 10},
		{offset: 4 * time.Minute, value: 20},
		{offset: 5*time.Minute + 10*time.Second, value: 30},
	} {
		if err := e.AddSample("prod", "temp.out_dry", engine.Timestamp(base.Add(sample.offset).UnixNano()), sample.value); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runQuery([]string{
			"--root", root,
			"--db", "prod",
			"--metric", "^temp\\.out_dry$",
			"--start", strconv.FormatInt(base.UnixNano(), 10),
			"--end", strconv.FormatInt(base.Add(10*time.Minute).UnixNano(), 10),
			"--aggregate", "sum,count",
			"--window", "5m",
			"--json",
		}); err != nil {
			t.Fatalf("runQuery failed: %v", err)
		}
	})

	var report queryReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("Unmarshal failed: %v\noutput=%s", err, out)
	}
	if report.RowCount != 4 {
		t.Fatalf("row count mismatch: got=%d want=4", report.RowCount)
	}
	if report.Rows[0].Aggregate != "count" && report.Rows[0].Aggregate != "sum" {
		t.Fatalf("expected aggregate rows, got=%+v", report.Rows[0])
	}
	for _, row := range report.Rows {
		if row.Window != "5m0s" {
			t.Fatalf("window mismatch: got=%q want=5m0s", row.Window)
		}
		if row.EndNS == 0 {
			t.Fatalf("expected aggregate end timestamp in row: %+v", row)
		}
	}
}

func TestRunQueryAggregateJSON_AllowsMissingEnd(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	base := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	for _, sample := range []struct {
		offset time.Duration
		value  float32
	}{
		{offset: 10 * time.Second, value: 10},
		{offset: 4 * time.Minute, value: 20},
		{offset: 5*time.Minute + 10*time.Second, value: 30},
	} {
		if err := e.AddSample("prod", "temp.out_dry", engine.Timestamp(base.Add(sample.offset).UnixNano()), sample.value); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runQuery([]string{
			"--root", root,
			"--db", "prod",
			"--metric", "^temp\\.out_dry$",
			"--start", strconv.FormatInt(base.UnixNano(), 10),
			"--aggregate", "sum,count",
			"--window", "5m",
			"--json",
		}); err != nil {
			t.Fatalf("runQuery failed: %v", err)
		}
	})

	var report queryReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("Unmarshal failed: %v\noutput=%s", err, out)
	}
	if report.RowCount != 4 {
		t.Fatalf("row count mismatch: got=%d want=4", report.RowCount)
	}
	if report.End != nil {
		t.Fatalf("expected omitted report end when --end is absent, got=%d", *report.End)
	}
}
