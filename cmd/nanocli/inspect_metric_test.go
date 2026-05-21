package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func TestBuildInspectMetricReport_ShowsCoalescedFrames(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}

	base := engine.Timestamp(1_700_000_000_000_000_000)
	for _, sample := range []struct {
		metric string
		ts     engine.Timestamp
		value  any
	}{
		{metric: "cpu.temp", ts: base + 1, value: float32(41.25)},
		{metric: "cpu.idle", ts: base + 2, value: int32(80)},
		{metric: "cpu.temp", ts: base + 3, value: float32(41.50)},
	} {
		switch v := sample.value.(type) {
		case int32:
			if err := e.AddSample("prod", sample.metric, sample.ts, v); err != nil {
				t.Fatalf("AddSample failed: %v", err)
			}
		case float32:
			if err := e.AddSample("prod", sample.metric, sample.ts, v); err != nil {
				t.Fatalf("AddSample failed: %v", err)
			}
		default:
			t.Fatalf("unsupported sample type %T", sample.value)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close batch1 failed: %v", err)
	}

	e, err = engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("ReopenEngine failed: %v", err)
	}

	for _, sample := range []struct {
		metric string
		ts     engine.Timestamp
		value  any
	}{
		{metric: "cpu.idle", ts: base + 4, value: int32(82)},
		{metric: "cpu.temp", ts: base + 5, value: float32(42.00)},
		{metric: "cpu.idle", ts: base + 6, value: int32(83)},
	} {
		switch v := sample.value.(type) {
		case int32:
			if err := e.AddSample("prod", sample.metric, sample.ts, v); err != nil {
				t.Fatalf("AddSample failed: %v", err)
			}
		case float32:
			if err := e.AddSample("prod", sample.metric, sample.ts, v); err != nil {
				t.Fatalf("AddSample failed: %v", err)
			}
		default:
			t.Fatalf("unsupported sample type %T", sample.value)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close batch2 failed: %v", err)
	}

	e, err = engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("ReopenEngine failed: %v", err)
	}
	defer e.Close()

	partition := time.Unix(0, int64(base)).UTC().Format("2006-01-02")
	if _, err := e.BuildMetricFileV1("prod", partition); err != nil {
		t.Fatalf("BuildMetricFileV1 failed: %v", err)
	}

	ctx, err := resolveDBContext(root, "prod")
	if err != nil {
		t.Fatalf("resolveDBContext failed: %v", err)
	}
	report, err := buildInspectMetricReport(ctx, true)
	if err != nil {
		t.Fatalf("buildInspectMetricReport failed: %v", err)
	}

	if report.FileCount != 1 {
		t.Fatalf("file count mismatch: got=%d want=1", report.FileCount)
	}
	if report.TotalFrames != 2 {
		t.Fatalf("total frames mismatch: got=%d want=2", report.TotalFrames)
	}
	if report.TotalPoints != 6 {
		t.Fatalf("total points mismatch: got=%d want=6", report.TotalPoints)
	}
	if report.TotalMetrics != 2 {
		t.Fatalf("total metrics mismatch: got=%d want=2", report.TotalMetrics)
	}
	if len(report.Files[0].FramesDetail) != 2 {
		t.Fatalf("frame detail count mismatch: got=%d want=2", len(report.Files[0].FramesDetail))
	}
	for _, frame := range report.Files[0].FramesDetail {
		if frame.PointCount != 3 {
			t.Fatalf("expected merged frame point count 3, got %d", frame.PointCount)
		}
	}
}
