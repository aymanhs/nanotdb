package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func TestRunImportParts_CreatesMonthlyMetricFilesAndCatalog(t *testing.T) {
	outDir := t.TempDir()
	input := filepath.Join(outDir, "input.lp")
	baseA := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	baseB := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	lp := "" +
		"source/temp.out_dry 21.5 " + engine.FormatTimestamp(engine.Timestamp(baseA.UnixNano())) + "\n" +
		"temp.out_dry 22.5 " + engine.FormatTimestamp(engine.Timestamp(baseB.UnixNano())) + "\n" +
		"source/sample.count 7i " + engine.FormatTimestamp(engine.Timestamp(baseA.Add(time.Minute).UnixNano())) + "\n"
	if err := os.WriteFile(input, []byte(lp), 0644); err != nil {
		t.Fatalf("WriteFile input.lp failed: %v", err)
	}

	if err := runImport([]string{"parts", "--in", input, "--partition-mode", "month", "--out-dir", outDir}); err != nil {
		t.Fatalf("runImport parts failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(outDir, "catalog.json")); err != nil {
		t.Fatalf("catalog.json missing: %v", err)
	}
	for _, name := range []string{"metric-2026-05.dat", "metric-2026-06.dat"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	cat, err := engine.LoadCatalog(filepath.Join(outDir, "catalog.json"))
	if err != nil {
		t.Fatalf("LoadCatalog failed: %v", err)
	}
	defer cat.Close()
	if _, ok := cat.GetMetricEntry("temp.out_dry"); !ok {
		t.Fatal("expected temp.out_dry in catalog")
	}
	if entry, ok := cat.GetMetricEntry("sample.count"); !ok || entry.ValueType != engine.Int32Sample {
		t.Fatalf("expected sample.count int32 entry, got ok=%v entry=%+v", ok, entry)
	}
	if summary, err := engine.ReadMetricFileSummary(filepath.Join(outDir, "metric-2026-05.dat")); err != nil {
		t.Fatalf("ReadMetricFileSummary failed: %v", err)
	} else if summary.Version != 2 {
		t.Fatalf("expected metric v2 file, got v%d", summary.Version)
	}
}

func TestRunExportParts_EmitsLPWithoutAndWithDBPrefix(t *testing.T) {
	outDir := t.TempDir()
	input := filepath.Join(outDir, "input.lp")
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	lp := "" +
		"source/temp.out_dry 21.5 " + engine.FormatTimestamp(engine.Timestamp(base.UnixNano())) + "\n" +
		"sample.count 7i " + engine.FormatTimestamp(engine.Timestamp(base.Add(time.Minute).UnixNano())) + "\n"
	if err := os.WriteFile(input, []byte(lp), 0644); err != nil {
		t.Fatalf("WriteFile input.lp failed: %v", err)
	}
	if err := runImport([]string{"parts", "--in", input, "--partition-mode", "month", "--out-dir", outDir}); err != nil {
		t.Fatalf("runImport parts failed: %v", err)
	}

	plainOut := filepath.Join(outDir, "plain.lp")
	if err := runExport([]string{"parts", "--in", outDir, "--catalog", filepath.Join(outDir, "catalog.json"), "--out", plainOut}); err != nil {
		t.Fatalf("runExport parts failed: %v", err)
	}
	plain, err := os.ReadFile(plainOut)
	if err != nil {
		t.Fatalf("ReadFile plain.lp failed: %v", err)
	}
	plainText := string(plain)
	if strings.Contains(plainText, "source/") {
		t.Fatalf("did not expect db prefix in plain export:\n%s", plainText)
	}
	if !strings.Contains(plainText, "sample.count 7i ") {
		t.Fatalf("expected int suffix preserved in export:\n%s", plainText)
	}

	withDBOut := filepath.Join(outDir, "with-db.lp")
	if err := runExport([]string{"parts", "--in", outDir, "--catalog", filepath.Join(outDir, "catalog.json"), "--out", withDBOut, "--with-db", "derived"}); err != nil {
		t.Fatalf("runExport parts with db failed: %v", err)
	}
	withDB, err := os.ReadFile(withDBOut)
	if err != nil {
		t.Fatalf("ReadFile with-db.lp failed: %v", err)
	}
	if !strings.Contains(string(withDB), "derived/temp.out_dry ") {
		t.Fatalf("expected db prefix in export:\n%s", string(withDB))
	}
}
