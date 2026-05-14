package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRuntimeConfig(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "engine.toml")
	content := []byte("[engine]\nlisten = \":9999\"\n[wal]\nmax_segment_size = 12345\n")
	if err := os.WriteFile(configPath, content, 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	listen, dataDir, walMaxSeg, err := loadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("loadRuntimeConfig failed: %v", err)
	}
	if listen != ":9999" {
		t.Fatalf("listen mismatch: got=%q want=%q", listen, ":9999")
	}
	if dataDir != root {
		t.Fatalf("dataDir mismatch: got=%q want=%q", dataDir, root)
	}
	if walMaxSeg != 12345 {
		t.Fatalf("walMaxSeg mismatch: got=%d want=%d", walMaxSeg, 12345)
	}
}

func TestLoadRuntimeConfig_Defaults(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "engine.toml")
	if err := os.WriteFile(configPath, []byte("[engine]\nlisten = \"\"\n"), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	listen, _, walMaxSeg, err := loadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("loadRuntimeConfig failed: %v", err)
	}
	if listen != ":8428" {
		t.Fatalf("listen default mismatch: got=%q want=%q", listen, ":8428")
	}
	if walMaxSeg != 64*1024*1024 {
		t.Fatalf("wal max default mismatch: got=%d want=%d", walMaxSeg, 64*1024*1024)
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
