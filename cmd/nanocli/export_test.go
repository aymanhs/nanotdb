package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func TestRunExport_WritesToStdoutWhenOutOmitted(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	ts := engine.Timestamp(time.Date(2026, 5, 14, 12, 34, 56, 123456789, time.UTC).UnixNano())
	if err := e.AddSample("sensors", "temperature", ts, int32(42)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	got := captureStdout(t, func() {
		if err := runExport([]string{"--root", root, "--db", "sensors"}); err != nil {
			t.Fatalf("runExport failed: %v", err)
		}
	})

	want := "sensors/temperature 42 " + engine.FormatTimestamp(ts) + "\n"
	if got != want {
		t.Fatalf("stdout export mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestRunExport_JSONRequiresOutWhenStdout(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	if err := e.AddSample("sensors", "temperature", engine.Timestamp(time.Now().UnixNano()), int32(1)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	err = runExport([]string{"--root", root, "--db", "sensors", "--json"})
	if err == nil {
		t.Fatalf("expected error when --json is used without --out")
	}
	if !strings.Contains(err.Error(), "--json requires --out") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunExport_NormalizesTrailingSlashInDBName(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	ts := engine.Timestamp(time.Date(2026, 5, 14, 12, 34, 56, 123456789, time.UTC).UnixNano())
	if err := e.AddSample("sensors_rollup_1h", "temperature.sum", ts, float32(42)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	got := captureStdout(t, func() {
		if err := runExport([]string{"--root", root, "--db", "sensors_rollup_1h/"}); err != nil {
			t.Fatalf("runExport failed: %v", err)
		}
	})

	want := "sensors_rollup_1h/temperature.sum 42 " + engine.FormatTimestamp(ts) + "\n"
	if got != want {
		t.Fatalf("stdout export mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	defer func() {
		os.Stdout = old
	}()
	os.Stdout = w

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("pipe close failed: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("pipe close failed: %v", err)
	}
	return string(out)
}
