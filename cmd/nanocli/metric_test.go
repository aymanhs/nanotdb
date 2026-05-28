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
	now := time.Date(2026, 5, 24, 12, 10, 0, 0, time.UTC)
	oldNow := queryTimeNow
	queryTimeNow = func() time.Time { return now }
	defer func() { queryTimeNow = oldNow }()

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
	if report.End == nil {
		t.Fatal("expected report end to default to now when --end is absent")
	}
	if *report.End != now.UnixNano() {
		t.Fatalf("end mismatch: got=%d want=%d", *report.End, now.UnixNano())
	}
}

func TestRunQueryStartAcceptsRelativeDuration(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	oldNow := queryTimeNow
	queryTimeNow = func() time.Time { return now }
	defer func() { queryTimeNow = oldNow }()

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	for _, sample := range []struct {
		offset time.Duration
		value  float32
	}{
		{offset: -3 * time.Minute, value: 10},
		{offset: -90 * time.Second, value: 20},
		{offset: -30 * time.Second, value: 30},
	} {
		if err := e.AddSample("prod", "temp.out_dry", engine.Timestamp(now.Add(sample.offset).UnixNano()), sample.value); err != nil {
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
			"--start", "2m",
			"--json",
		}); err != nil {
			t.Fatalf("runQuery failed: %v", err)
		}
	})

	var report queryReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("Unmarshal failed: %v\noutput=%s", err, out)
	}
	if report.Start == nil {
		t.Fatal("expected report start to be populated")
	}
	wantStart := now.Add(-2 * time.Minute).UnixNano()
	if *report.Start != wantStart {
		t.Fatalf("start mismatch: got=%d want=%d", *report.Start, wantStart)
	}
	if report.RowCount != 2 {
		t.Fatalf("row count mismatch: got=%d want=2", report.RowCount)
	}
	if report.End == nil {
		t.Fatal("expected report end to default to now")
	}
	if *report.End != now.UnixNano() {
		t.Fatalf("end mismatch: got=%d want=%d", *report.End, now.UnixNano())
	}
}

func TestRunQueryEndAcceptsRelativeDuration(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	oldNow := queryTimeNow
	queryTimeNow = func() time.Time { return now }
	defer func() { queryTimeNow = oldNow }()

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	for _, sample := range []struct {
		offset time.Duration
		value  float32
	}{
		{offset: -4 * time.Minute, value: 10},
		{offset: -3 * time.Minute, value: 20},
		{offset: -90 * time.Second, value: 30},
	} {
		if err := e.AddSample("prod", "temp.out_dry", engine.Timestamp(now.Add(sample.offset).UnixNano()), sample.value); err != nil {
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
			"--start", "5m",
			"--end", "2m",
			"--json",
		}); err != nil {
			t.Fatalf("runQuery failed: %v", err)
		}
	})

	var report queryReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("Unmarshal failed: %v\noutput=%s", err, out)
	}
	if report.Start == nil || report.End == nil {
		t.Fatalf("expected start and end in report, got start=%v end=%v", report.Start, report.End)
	}
	wantStart := now.Add(-5 * time.Minute).UnixNano()
	wantEnd := now.Add(-2 * time.Minute).UnixNano()
	if *report.Start != wantStart {
		t.Fatalf("start mismatch: got=%d want=%d", *report.Start, wantStart)
	}
	if *report.End != wantEnd {
		t.Fatalf("end mismatch: got=%d want=%d", *report.End, wantEnd)
	}
	if report.RowCount != 2 {
		t.Fatalf("row count mismatch: got=%d want=2", report.RowCount)
	}
}

func TestParseExtendedDurationSupportsDaysAndWeeks(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{input: "1d", want: 24 * time.Hour},
		{input: "1w", want: 7 * 24 * time.Hour},
		{input: "1d2h30m", want: 26*time.Hour + 30*time.Minute},
		{input: "1.5d", want: 36 * time.Hour},
	}
	for _, tc := range tests {
		got, err := parseExtendedDuration(tc.input)
		if err != nil {
			t.Fatalf("parseExtendedDuration(%q) failed: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("parseExtendedDuration(%q) mismatch: got=%s want=%s", tc.input, got, tc.want)
		}
	}
}

func TestParseExtendedDurationRejectsUnknownUnits(t *testing.T) {
	_, err := parseExtendedDuration("1fortnight")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown duration unit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunQueryStartAcceptsDayDuration(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	oldNow := queryTimeNow
	queryTimeNow = func() time.Time { return now }
	defer func() { queryTimeNow = oldNow }()

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	for _, sample := range []struct {
		offset time.Duration
		value  float32
	}{
		{offset: -25 * time.Hour, value: 10},
		{offset: -23 * time.Hour, value: 20},
		{offset: -30 * time.Minute, value: 30},
	} {
		if err := e.AddSample("prod", "temp.out_dry", engine.Timestamp(now.Add(sample.offset).UnixNano()), sample.value); err != nil {
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
			"--start", "1d",
			"--json",
		}); err != nil {
			t.Fatalf("runQuery failed: %v", err)
		}
	})

	var report queryReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("Unmarshal failed: %v\noutput=%s", err, out)
	}
	if report.Start == nil {
		t.Fatal("expected report start to be populated")
	}
	wantStart := now.Add(-24 * time.Hour).UnixNano()
	if *report.Start != wantStart {
		t.Fatalf("start mismatch: got=%d want=%d", *report.Start, wantStart)
	}
	if report.RowCount != 2 {
		t.Fatalf("row count mismatch: got=%d want=2", report.RowCount)
	}
}

func TestRunQueryWithoutMetricQueriesAllMetrics(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	ts := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	if err := e.AddSample("prod", "cpu.temp", engine.Timestamp(ts.UnixNano()), float32(40)); err != nil {
		t.Fatalf("AddSample cpu.temp failed: %v", err)
	}
	if err := e.AddSample("prod", "mem.used", engine.Timestamp(ts.Add(time.Second).UnixNano()), float32(50)); err != nil {
		t.Fatalf("AddSample mem.used failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runQuery([]string{
			"--root", root,
			"--db", "prod",
			"--start", strconv.FormatInt(ts.Add(-time.Second).UnixNano(), 10),
			"--end", strconv.FormatInt(ts.Add(2*time.Second).UnixNano(), 10),
			"--json",
		}); err != nil {
			t.Fatalf("runQuery failed: %v", err)
		}
	})

	var report queryReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("Unmarshal failed: %v\noutput=%s", err, out)
	}
	if report.MetricRegex != ".*" {
		t.Fatalf("metric regex mismatch: got=%q want=%q", report.MetricRegex, ".*")
	}
	if report.RowCount != 2 {
		t.Fatalf("row count mismatch: got=%d want=2", report.RowCount)
	}
	gotMetrics := []string{report.Rows[0].Metric, report.Rows[1].Metric}
	if gotMetrics[0] != "cpu.temp" || gotMetrics[1] != "mem.used" {
		t.Fatalf("unexpected metrics: got=%v", gotMetrics)
	}
}
