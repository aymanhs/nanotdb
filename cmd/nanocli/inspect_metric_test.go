package main

import (
	"fmt"
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
	if report.Files[0].Version != 1 {
		t.Fatalf("version mismatch: got=%d want=1", report.Files[0].Version)
	}
	if report.Files[0].TimeFrames != 0 {
		t.Fatalf("time frame count mismatch: got=%d want=0", report.Files[0].TimeFrames)
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

func TestBuildInspectMetricReport_NonVerboseUsesPageInfosOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := engine.Timestamp(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC).UnixNano())
	for i := 0; i < 4; i++ {
		if err := e.AddSample("prod", "cpu.temp", base+engine.Timestamp(i*10), float32(40+i)); err != nil {
			t.Fatalf("AddSample cpu.temp failed: %v", err)
		}
		if err := e.AddSample("prod", "cpu.idle", base+engine.Timestamp(i*10+1), int32(80+i)); err != nil {
			t.Fatalf("AddSample cpu.idle failed: %v", err)
		}
	}
	partition := time.Unix(0, int64(base)).UTC().Format("2006-01-02")
	metricPath, err := e.BuildMetricFileV1("prod", partition)
	if err != nil {
		t.Fatalf("BuildMetricFileV1 failed: %v", err)
	}

	infos, err := engine.ReadMetricFilePageInfosV1(metricPath)
	if err != nil {
		t.Fatalf("ReadMetricFilePageInfosV1 failed: %v", err)
	}
	if len(infos) == 0 {
		t.Fatal("expected non-empty page infos")
	}

	f, err := os.OpenFile(metricPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFile metricPath failed: %v", err)
	}
	corruptAt := int64(infos[0].PageOffset)
	if _, err := f.WriteAt([]byte{0}, corruptAt); err != nil {
		_ = f.Close()
		t.Fatalf("WriteAt corrupt frame header failed: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close corrupt file failed: %v", err)
	}

	ctx, err := resolveDBContext(root, "prod")
	if err != nil {
		t.Fatalf("resolveDBContext failed: %v", err)
	}

	report, err := buildInspectMetricReport(ctx, false)
	if err != nil {
		t.Fatalf("buildInspectMetricReport non-verbose failed: %v", err)
	}
	if report.HasErrors {
		t.Fatalf("expected non-verbose report without scan errors, got=%+v", report.Files)
	}
	if report.TotalFrames == 0 || report.TotalPoints == 0 {
		t.Fatalf("expected non-verbose report totals, got frames=%d points=%d", report.TotalFrames, report.TotalPoints)
	}

	verboseReport, err := buildInspectMetricReport(ctx, true)
	if err != nil {
		t.Fatalf("buildInspectMetricReport verbose failed: %v", err)
	}
	if !verboseReport.HasErrors {
		t.Fatal("expected verbose report to surface payload corruption")
	}
	if len(verboseReport.Files) != 1 || verboseReport.Files[0].ScanError == "" {
		t.Fatalf("expected verbose scan error, got=%+v", verboseReport.Files)
	}
	if verboseReport.Files[0].ScanError != fmt.Sprintf("invalid frame magic at offset %d", infos[0].PageOffset) {
		t.Fatalf("unexpected verbose scan error: %q", verboseReport.Files[0].ScanError)
	}
}

func TestBuildInspectMetricReport_V2ShowsSharedTimeFrames(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := engine.Timestamp(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC).UnixNano())
	for i := 0; i < 4; i++ {
		ts := base + engine.Timestamp(i*10)
		if err := e.AddSample("prod", "cpu.temp", ts, float32(40+i)); err != nil {
			t.Fatalf("AddSample cpu.temp failed: %v", err)
		}
		if err := e.AddSample("prod", "cpu.idle", ts, int32(80-i)); err != nil {
			t.Fatalf("AddSample cpu.idle failed: %v", err)
		}
	}
	partition := time.Unix(0, int64(base)).UTC().Format("2006-01-02")
	if _, err := e.BuildMetricFileV2("prod", partition); err != nil {
		t.Fatalf("BuildMetricFileV2 failed: %v", err)
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
	if report.Files[0].Version != 2 {
		t.Fatalf("version mismatch: got=%d want=2", report.Files[0].Version)
	}
	if report.Files[0].TimeFrames != 1 {
		t.Fatalf("time frame count mismatch: got=%d want=1", report.Files[0].TimeFrames)
	}
	if report.Files[0].Frames != 2 {
		t.Fatalf("metric frame count mismatch: got=%d want=2", report.Files[0].Frames)
	}
	if report.TotalPoints != 8 {
		t.Fatalf("total points mismatch: got=%d want=8", report.TotalPoints)
	}
	if len(report.Files[0].FramesDetail) != 2 {
		t.Fatalf("frame detail count mismatch: got=%d want=2", len(report.Files[0].FramesDetail))
	}
	for _, frame := range report.Files[0].FramesDetail {
		if frame.PointCount != 4 {
			t.Fatalf("expected frame point count 4, got %d", frame.PointCount)
		}
	}
}
