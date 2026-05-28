package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func TestBuildInspectCatalogReport_SortsAndTypesMetrics(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	if err := e.AddSample("prod", "zeta.temp", engine.Timestamp(100), float32(42)); err != nil {
		t.Fatalf("AddSample zeta.temp failed: %v", err)
	}
	if err := e.AddSample("prod", "alpha.count", engine.Timestamp(101), int32(7)); err != nil {
		t.Fatalf("AddSample alpha.count failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	ctx, err := resolveDBContext(root, "prod")
	if err != nil {
		t.Fatalf("resolveDBContext failed: %v", err)
	}
	report, err := buildInspectCatalogReport(ctx)
	if err != nil {
		t.Fatalf("buildInspectCatalogReport failed: %v", err)
	}

	if report.Count != 2 {
		t.Fatalf("count mismatch: got=%d want=2", report.Count)
	}
	if len(report.Metrics) != 2 {
		t.Fatalf("metrics length mismatch: got=%d want=2", len(report.Metrics))
	}
	if report.Metrics[0].Name != "alpha.count" || report.Metrics[0].ValueType != "int32" {
		t.Fatalf("unexpected first metric: %+v", report.Metrics[0])
	}
	if report.Metrics[1].Name != "zeta.temp" || report.Metrics[1].ValueType != "float32" {
		t.Fatalf("unexpected second metric: %+v", report.Metrics[1])
	}
}

func TestRunInspectCatalog_HumanOutput(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	if err := e.AddSample("prod", "cpu.temp", engine.Timestamp(100), float32(42)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runInspect([]string{"catalog", "--root", root, "--db", "prod"}); err != nil {
			t.Fatalf("runInspect catalog failed: %v", err)
		}
	})

	if !strings.Contains(out, "Catalog for prod") {
		t.Fatalf("expected catalog heading, got:\n%s", out)
	}
	if !strings.Contains(out, "cpu.temp") || !strings.Contains(out, "float32") {
		t.Fatalf("expected metric row, got:\n%s", out)
	}
}

func TestRunInspectCatalog_JSONOutput(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	if err := e.AddSample("prod", "cpu.temp", engine.Timestamp(100), float32(42)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runInspect([]string{"catalog", "--root", root, "--db", "prod", "--json"}); err != nil {
			t.Fatalf("runInspect catalog json failed: %v", err)
		}
	})

	var report inspectCatalogListReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("Unmarshal failed: %v\noutput=%s", err, out)
	}
	if report.Count != 1 {
		t.Fatalf("count mismatch: got=%d want=1", report.Count)
	}
	if len(report.Metrics) != 1 || report.Metrics[0].Name != "cpu.temp" {
		t.Fatalf("unexpected metrics: %+v", report.Metrics)
	}
}
