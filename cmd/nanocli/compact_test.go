package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func TestRunCompactRewritesSealedPartition(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte("[engine]\nlisten=\":8428\"\n"), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll prod failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), []byte("[retention]\nretention_action = \"keep\"\n\n[page]\nmax_records = 1\nmax_bytes = 64\nmax_age = \"10m\"\n"), 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	base := time.Date(2024, time.January, 4, 12, 0, 0, 0, time.UTC)
	ts1 := base.UnixNano()
	ts2 := base.Add(5 * time.Minute).UnixNano()
	part := base.Format("2006-01-02")
	if err := e.AddLine("prod/temp.out_dry 11 " + itoaCLI(ts1)); err != nil {
		t.Fatalf("AddLine first failed: %v", err)
	}
	if err := e.AddLine("prod/temp.out_dry 13 " + itoaCLI(ts2)); err != nil {
		t.Fatalf("AddLine second failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	path := filepath.Join(root, "prod", "data-"+part+".dat")
	statsBefore, err := engine.ScanDataFileStats(path)
	if err != nil {
		t.Fatalf("ScanDataFileStats before failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), []byte("[retention]\nretention_action = \"keep\"\n\n[page]\nmax_records = 128\nmax_bytes = 4096\nmax_age = \"10m\"\n"), 0644); err != nil {
		t.Fatalf("WriteFile manifest update failed: %v", err)
	}

	if err := runCompact([]string{"--root", root, "--db", "prod", "--part", part}); err != nil {
		t.Fatalf("runCompact failed: %v", err)
	}

	statsAfter, err := engine.ScanDataFileStats(path)
	if err != nil {
		t.Fatalf("ScanDataFileStats after failed: %v", err)
	}
	if statsAfter.Frames >= statsBefore.Frames {
		t.Fatalf("expected fewer frames after compact, before=%d after=%d", statsBefore.Frames, statsAfter.Frames)
	}
}

func itoaCLI(v int64) string {
	return strconv.FormatInt(v, 10)
}
