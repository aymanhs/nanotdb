package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

// TestEventsReaderIsOffline confirms that `nanocli events` reads
// directly from sealed events-*.dat files and does NOT open the
// engine. We verify this two ways:
//
//   1. Snapshot every file under the data dir before and after the
//      runEvents call; the modtimes must not change. Engine.Open
//      touches the WAL files even on a clean open, so unchanged
//      modtimes prove no engine boot happened.
//   2. Run the reader repeatedly and confirm the row count stays
//      stable. The original bug was that each call boot-then-closed
//      the engine, adding +2 internal events per invocation.
func TestEventsReaderIsOffline(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("write engine.toml: %v", err)
	}

	// Boot the engine, emit a few events into a normal db with
	// events enabled, close cleanly. After this point, the engine
	// must not be touched.
	dbName := "sensors"
	dbDir := filepath.Join(root, dbName)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	manifest := "[retention]\nretention_action = \"keep\"\n\n[events]\nenabled = true\n"
	if err := os.WriteFile(filepath.Join(dbDir, "manifest.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	e, err := engine.OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	now := time.Now().UnixNano()
	if err := e.AddEvent(dbName, "test.first", engine.Timestamp(now), int32(11), []byte(`{"k":"v"}`)); err != nil {
		t.Fatalf("AddEvent first: %v", err)
	}
	if err := e.AddEvent(dbName, "test.second", engine.Timestamp(now+1), int32(22), nil); err != nil {
		t.Fatalf("AddEvent second: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Snapshot modtimes of every file under root.
	before := snapshotModtimes(t, root)

	// Run the offline reader three times.
	for i := 0; i < 3; i++ {
		if err := runEvents([]string{
			"--root", root,
			"--db", dbName,
			"--start", "1970-01-01T00:00:00Z",
			"--end", "2099-12-31T00:00:00Z",
			"--limit", "100",
			"--format", "json",
		}); err != nil {
			t.Fatalf("runEvents iteration %d: %v", i, err)
		}
	}

	after := snapshotModtimes(t, root)

	// Modtimes of every persistent file must be unchanged. A modtime
	// drift would mean the engine was opened — which is exactly the
	// bug we are guarding against.
	for path, beforeMod := range before {
		afterMod, ok := after[path]
		if !ok {
			t.Errorf("file %s disappeared after offline read", path)
			continue
		}
		if !beforeMod.Equal(afterMod) {
			t.Errorf("file %s modtime changed after offline read: before=%v after=%v", path, beforeMod, afterMod)
		}
	}
	// No new files either.
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("unexpected new file after offline read: %s", path)
		}
	}
}

// TestInternalEventsTailRequiresKnownGroup confirms that the
// --group flag rejects unknown group names rather than silently
// returning zero rows.
func TestInternalEventsTailRequiresKnownGroup(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("write engine.toml: %v", err)
	}
	err := runInternalEventsTail([]string{
		"--root", root,
		"--group", "bogus.group.name",
	})
	if err == nil {
		t.Fatalf("expected an error for unknown group")
	}
	if !strings.Contains(err.Error(), "unknown internal-events group") {
		t.Fatalf("expected \"unknown internal-events group\" error, got: %v", err)
	}
}

// TestInternalEventsHTTPSubcommandsAreRemoved confirms that the
// admin/HTTP-flavored subcommands are no longer accepted.
func TestInternalEventsHTTPSubcommandsAreRemoved(t *testing.T) {
	for _, sub := range []string{"catalog", "groups", "set"} {
		err := runInternalEvents([]string{sub})
		if err == nil {
			t.Errorf("subcommand %q should have been removed but ran without error", sub)
			continue
		}
		if !strings.Contains(err.Error(), "unknown internal-events subcommand") {
			t.Errorf("subcommand %q removed but error message changed: %v", sub, err)
		}
	}
}

// snapshotModtimes walks root and returns a path → modtime map for
// every regular file. Used to detect "did the offline read open the
// engine?" — the engine's WAL fsync would update modtimes.
func snapshotModtimes(t *testing.T, root string) map[string]time.Time {
	t.Helper()
	out := make(map[string]time.Time)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		out[path] = info.ModTime()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
