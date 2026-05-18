package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func TestRunRollupBackfillsSourceDatabase(t *testing.T) {
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sensors"), 0755); err != nil {
		t.Fatalf("MkdirAll sensors failed: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-3 * time.Hour).Truncate(time.Hour)
	sourceManifest := `[retention]
grace = "1m"
retention_days = 30
max_active_days = 2

[wal]
enabled = true
skip_before = "1h"

[page]
max_records = 16000
max_bytes = 127000
max_age = "60s"

[rollups]
enabled = true
checkpoint_file = "rollup.checkpoints.log"
default_grace = "0s"

[[rollups.jobs]]
id = "temperature_1h"
source_metric = "temperature"
interval = "1h"
aggregates = ["sum"]
destination_db = "sensors_rollup_1h"
destination_metric_prefix = "temperature"
`
	if err := os.WriteFile(filepath.Join(root, "sensors", "manifest.toml"), []byte(sourceManifest), 0644); err != nil {
		t.Fatalf("WriteFile sensors manifest failed: %v", err)
	}

	input := filepath.Join(root, "input.lp")
	lp := "" +
		"sensors/temperature 10i " + itoa64(base.Add(10*time.Minute).UnixNano()) + "\n" +
		"sensors/temperature 20i " + itoa64(base.Add(30*time.Minute).UnixNano()) + "\n" +
		"sensors/temperature 1i " + itoa64(base.Add(1*time.Hour).UnixNano()) + "\n"
	if err := os.WriteFile(input, []byte(lp), 0644); err != nil {
		t.Fatalf("WriteFile input.lp failed: %v", err)
	}
	if err := runImport([]string{"--root", root, "--in", input}); err != nil {
		t.Fatalf("runImport failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine stale state setup failed: %v", err)
	}
	if err := e.AddLine("sensors_rollup_1h/stale.metric 99 " + itoa64(base.Add(90*time.Minute).UnixNano())); err != nil {
		t.Fatalf("AddLine stale metric failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close stale state setup failed: %v", err)
	}

	if err := runRollup([]string{"--root", root, "--db", "sensors"}); err != nil {
		t.Fatalf("runRollup failed: %v", err)
	}

	verify, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine verify failed: %v", err)
	}
	defer verify.Close()

	assertMetricHasValue(t, verify, "sensors_rollup_1h", "temperature.sum", engine.Timestamp(base.UnixNano()), engine.Timestamp(now.UnixNano()), 30)
	if _, found, err := verify.QueryLast("sensors_rollup_1h", "stale.metric"); err != nil {
		t.Fatalf("QueryLast stale.metric failed: %v", err)
	} else if found {
		t.Fatalf("expected stale metric to be removed by runRollup")
	}
}
