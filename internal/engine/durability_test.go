package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenEngineDurabilityProfiles(t *testing.T) {
	tests := []struct {
		name         string
		profile      string
		wantSyncData bool
		wantSyncCat  bool
	}{
		{name: "strict", profile: DurabilityProfileStrict, wantSyncData: true, wantSyncCat: true},
		{name: "balanced", profile: DurabilityProfileBalanced, wantSyncData: true, wantSyncCat: false},
		{name: "throughput", profile: DurabilityProfileThroughput, wantSyncData: false, wantSyncCat: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := []byte("[durability]\nprofile = \"" + tc.profile + "\"\n")
			if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
				t.Fatalf("write engine.toml failed: %v", err)
			}

			e, err := OpenEngine(root, 1024*1024)
			if err != nil {
				t.Fatalf("OpenEngine failed: %v", err)
			}
			defer e.Close()

			if e.Durability != tc.profile {
				t.Fatalf("durability profile mismatch: got=%q want=%q", e.Durability, tc.profile)
			}
			if e.SyncDataFile != tc.wantSyncData {
				t.Fatalf("sync data mismatch: got=%t want=%t", e.SyncDataFile, tc.wantSyncData)
			}
			if e.SyncCatalog != tc.wantSyncCat {
				t.Fatalf("sync catalog mismatch: got=%t want=%t", e.SyncCatalog, tc.wantSyncCat)
			}
		})
	}
}

func TestOpenEngineRejectsInvalidDurabilityProfile(t *testing.T) {
	root := t.TempDir()
	cfg := []byte("[durability]\nprofile = \"invalid\"\n")
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
		t.Fatalf("write engine.toml failed: %v", err)
	}

	if _, err := OpenEngine(root, 1024*1024); err == nil {
		t.Fatal("expected OpenEngine to reject invalid durability profile")
	}
}

func TestOpenEngineRejectsInvalidLoggingLevel(t *testing.T) {
	root := t.TempDir()
	cfg := []byte("[logging]\n\n[[logging.logger]]\noutput = \"console\"\nlevel = \"loud\"\n")
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
		t.Fatalf("write engine.toml failed: %v", err)
	}

	if _, err := OpenEngine(root, 1024*1024); err == nil {
		t.Fatal("expected OpenEngine to reject invalid logging level")
	}
}

func TestOpenEngineRejectsDuplicateConsoleLoggers(t *testing.T) {
	root := t.TempDir()
	cfg := []byte("[logging]\n\n[[logging.logger]]\noutput = \"console\"\nlevel = \"info\"\n\n[[logging.logger]]\noutput = \"console\"\nlevel = \"debug\"\n")
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
		t.Fatalf("write engine.toml failed: %v", err)
	}

	if _, err := OpenEngine(root, 1024*1024); err == nil {
		t.Fatal("expected OpenEngine to reject duplicate console loggers")
	}
}
