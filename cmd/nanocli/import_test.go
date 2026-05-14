package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aymanhs/nanotdb/internal/engine"
)

func TestRunImportTriggersSourceConfiguredRollups(t *testing.T) {
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sensors"), 0755); err != nil {
		t.Fatalf("MkdirAll sensors failed: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	dayStart := now.Truncate(24 * time.Hour).Add(-48 * time.Hour)
	hourStart := dayStart.Add(2 * time.Hour)
	if dayStart.Add(24 * time.Hour).After(now.Add(-2 * time.Minute)) {
		t.Fatalf("test timestamps are not safely in the past")
	}

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
default_grace = "1m"

[[rollups.jobs]]
id = "office_temp_1h"
source_metric = "temp.office"
interval = "1h"
aggregates = ["sum"]
destination_db = "sensors_rollup_1h"
destination_metric_prefix = "temp.office"

[[rollups.jobs]]
id = "outside_temp_1h"
source_metric = "temp.out_dry"
interval = "1h"
aggregates = ["sum"]
destination_db = "sensors_rollup_1h"
destination_metric_prefix = "temp.out_dry"

[[rollups.jobs]]
id = "outside_temp_1d"
source_metric = "temp.out_dry"
interval = "24h"
aggregates = ["sum"]
destination_db = "sensors_rollup_1d"
destination_metric_prefix = "temp.out_dry"
`
	if err := os.WriteFile(filepath.Join(root, "sensors", "manifest.toml"), []byte(sourceManifest), 0644); err != nil {
		t.Fatalf("WriteFile sensors manifest failed: %v", err)
	}

	input := filepath.Join(root, "input.lp")
	lp := "" +
		"sensors/temp.office 5i " + itoa64(hourStart.Add(10*time.Minute).UnixNano()) + "\n" +
		"sensors/temp.out_dry 10i " + itoa64(hourStart.Add(10*time.Minute).UnixNano()) + "\n" +
		"sensors/temp.office 15i " + itoa64(hourStart.Add(40*time.Minute).UnixNano()) + "\n" +
		"sensors/temp.out_dry 20i " + itoa64(hourStart.Add(40*time.Minute).UnixNano()) + "\n" +
		"sensors/temp.office 1i " + itoa64(hourStart.Add(1*time.Hour).UnixNano()) + "\n" +
		"sensors/temp.out_dry 30i " + itoa64(dayStart.Add(26*time.Hour).UnixNano()) + "\n"
	if err := os.WriteFile(input, []byte(lp), 0644); err != nil {
		t.Fatalf("WriteFile input.lp failed: %v", err)
	}

	if err := runImport([]string{"--root", root, "--in", input}); err != nil {
		t.Fatalf("runImport failed: %v", err)
	}

	e, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine verify failed: %v", err)
	}
	defer e.Close()

	from := engine.Timestamp(dayStart.UnixNano())
	to := engine.Timestamp(now.UnixNano())

	assertMetricHasValue(t, e, "sensors_rollup_1h", "temp.office.sum", from, to, 20)
	assertMetricHasValue(t, e, "sensors_rollup_1h", "temp.out_dry.sum", from, to, 30)
	assertMetricHasValue(t, e, "sensors_rollup_1d", "temp.out_dry.sum", from, to, 30)

	raw, err := os.ReadFile(filepath.Join(root, "sensors", "rollup.checkpoints.log"))
	if err != nil {
		t.Fatalf("ReadFile checkpoint log failed: %v", err)
	}
	text := string(raw)
	for _, jobID := range []string{"office_temp_1h", "outside_temp_1h", "outside_temp_1d"} {
		if !strings.Contains(text, jobID+",") {
			t.Fatalf("expected checkpoint log to contain %s, got:\n%s", jobID, text)
		}
	}
}

func assertMetricHasValue(t *testing.T, e *engine.Engine, dbName, metric string, from, to engine.Timestamp, want float32) {
	t.Helper()

	rows := make([]engine.Sample, 0, 8)
	err := e.QueryRange(dbName, metric, from, to, 1, func(s engine.Sample) error {
		rows = append(rows, s)
		return nil
	})
	if err != nil {
		t.Fatalf("QueryRange %s/%s failed: %v", dbName, metric, err)
	}
	if len(rows) == 0 {
		t.Fatalf("expected rollup samples for %s/%s", dbName, metric)
	}
	for _, row := range rows {
		if row.Float32 == want {
			return
		}
	}
	t.Fatalf("expected %s/%s to contain value %f, got=%v", dbName, metric, want, rows)
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}

const testImportEngineTOML = `[engine]
listen = ":8428"

[wal]
max_segment_size = 67108864
fsync_policy = "segment"

[durability]
profile = "strict"

[stats]
enabled = false
interval = "30s"

[defaults]
databases = []

[manifest_defaults.retention]
grace = "5m"
retention_days = 30
max_active_days = 2

[manifest_defaults.wal]
enabled = true
skip_before = "1h"

[manifest_defaults.page]
max_records = 16000
max_bytes = 127000
max_age = "60s"
`

func TestChainedRollups_1HTo1D(t *testing.T) {
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "engine.toml"), []byte(testImportEngineTOML), 0644); err != nil {
		t.Fatalf("WriteFile engine.toml failed: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	dayStart := now.Truncate(24 * time.Hour).Add(-48 * time.Hour)
	hourStart := dayStart
	if dayStart.Add(24 * time.Hour).After(now.Add(-2 * time.Minute)) {
		t.Fatalf("test timestamps are not safely in the past")
	}

	// Source DB: imports raw data and rolls up to 1h
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
default_grace = "1m"

[[rollups.jobs]]
id = "temp_1h"
source_metric = "temperature"
interval = "1h"
aggregates = ["sum"]
destination_db = "sensors_rollup_1h"
destination_metric_prefix = "temperature"
`

	// 1H DB: intermediate aggregation, rolls up further to 1d
	h1Manifest := `[retention]
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
default_grace = "1m"

[[rollups.jobs]]
id = "temp_1d_from_1h"
source_metric = "temperature.sum"
interval = "24h"
aggregates = ["min", "max"]
destination_db = "sensors_rollup_1d"
destination_metric_prefix = "temperature"
`

	// 1D DB: final destination, no further rollups
	d1Manifest := `[retention]
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
enabled = false
checkpoint_file = "rollup.checkpoints.log"
default_grace = ""
`

	for _, dbName := range []string{"sensors", "sensors_rollup_1h", "sensors_rollup_1d"} {
		if err := os.MkdirAll(filepath.Join(root, dbName), 0755); err != nil {
			t.Fatalf("MkdirAll %s failed: %v", dbName, err)
		}
	}

	if err := os.WriteFile(filepath.Join(root, "sensors", "manifest.toml"), []byte(sourceManifest), 0644); err != nil {
		t.Fatalf("WriteFile sensors manifest failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sensors_rollup_1h", "manifest.toml"), []byte(h1Manifest), 0644); err != nil {
		t.Fatalf("WriteFile 1h manifest failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sensors_rollup_1d", "manifest.toml"), []byte(d1Manifest), 0644); err != nil {
		t.Fatalf("WriteFile 1d manifest failed: %v", err)
	}

	// Write source data over ~26h so 1h rollups close and 1d cascade can finalize.
	input := filepath.Join(root, "input.lp")
	lp := "" +
		"sensors/temperature 100i " + itoa64(hourStart.UnixNano()) + "\n" +
		"sensors/temperature 200i " + itoa64(hourStart.Add(15*time.Minute).UnixNano()) + "\n" +
		"sensors/temperature 300i " + itoa64(hourStart.Add(45*time.Minute).UnixNano()) + "\n" +
		"sensors/temperature 400i " + itoa64(hourStart.Add(1*time.Hour).UnixNano()) + "\n" +
		"sensors/temperature 500i " + itoa64(hourStart.Add(1*time.Hour+30*time.Minute).UnixNano()) + "\n" +
		"sensors/temperature 600i " + itoa64(hourStart.Add(24*time.Hour).UnixNano()) + "\n" +
		"sensors/temperature 700i " + itoa64(hourStart.Add(25*time.Hour).UnixNano()) + "\n"
	if err := os.WriteFile(input, []byte(lp), 0644); err != nil {
		t.Fatalf("WriteFile input.lp failed: %v", err)
	}

	// Step 1: Import raw data to sensors
	if err := runImport([]string{"--root", root, "--in", input}); err != nil {
		t.Fatalf("runImport failed: %v", err)
	}

	// Step 2: Verify the chain worked automatically
	// (1h rollups triggered on each sample arrival during import,
	//  1d rollups triggered on each 1h sample arrival, creating cascade)
	e, err := engine.OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine verify failed: %v", err)
	}
	defer e.Close()

	from := engine.Timestamp(dayStart.UnixNano())
	to := engine.Timestamp(now.UnixNano())

	// Verify 1h rollups exist (sum of temperature in each hour)
	// First hour: 100+200+300 = 600
	// Second hour: 400+500 = 900
	rows1h := make([]engine.Sample, 0)
	if err := e.QueryRange("sensors_rollup_1h", "temperature.sum", from, to, 1, func(s engine.Sample) error {
		rows1h = append(rows1h, s)
		return nil
	}); err != nil {
		t.Fatalf("QueryRange 1h failed: %v", err)
	}
	if len(rows1h) == 0 {
		t.Fatalf("expected 1h rollup samples but got none")
	}
	has600 := false
	has900 := false
	for _, row := range rows1h {
		if row.Float32 == 600 {
			has600 = true
		}
		if row.Float32 == 900 {
			has900 = true
		}
	}
	if !has600 || !has900 {
		t.Fatalf("expected 1h sums to include 600 and 900, got=%v", rows1h)
	}

	// Verify 1d rollups exist (min/max of the 1h sums)
	rows1d := make([]engine.Sample, 0)
	if err := e.QueryRange("sensors_rollup_1d", "temperature.min", from, to, 1, func(s engine.Sample) error {
		rows1d = append(rows1d, s)
		return nil
	}); err != nil {
		t.Fatalf("QueryRange 1d min failed: %v", err)
	}
	if len(rows1d) == 0 {
		t.Fatalf("expected 1d rollup samples (min) but got none")
	}
	hasMin600 := false
	for _, row := range rows1d {
		if row.Float32 == 600 {
			hasMin600 = true
		}
	}
	if !hasMin600 {
		t.Fatalf("expected 1d min to include 600, got=%v", rows1d)
	}

	rows1dMax := make([]engine.Sample, 0)
	if err := e.QueryRange("sensors_rollup_1d", "temperature.max", from, to, 1, func(s engine.Sample) error {
		rows1dMax = append(rows1dMax, s)
		return nil
	}); err != nil {
		t.Fatalf("QueryRange 1d max failed: %v", err)
	}
	if len(rows1dMax) == 0 {
		t.Fatalf("expected 1d rollup samples (max) but got none")
	}
	hasMax900 := false
	for _, row := range rows1dMax {
		if row.Float32 == 900 {
			hasMax900 = true
		}
	}
	if !hasMax900 {
		t.Fatalf("expected 1d max to include 900, got=%v", rows1dMax)
	}

	// Verify checkpoint logs exist for both rollup sources
	raw, err := os.ReadFile(filepath.Join(root, "sensors", "rollup.checkpoints.log"))
	if err != nil {
		t.Fatalf("ReadFile sensors checkpoint log failed: %v", err)
	}
	if !strings.Contains(string(raw), "temp_1h,") {
		t.Fatalf("expected sensors checkpoint to contain temp_1h")
	}

	raw1h, err := os.ReadFile(filepath.Join(root, "sensors_rollup_1h", "rollup.checkpoints.log"))
	if err != nil {
		t.Fatalf("ReadFile 1h checkpoint log failed: %v", err)
	}
	if !strings.Contains(string(raw1h), "temp_1d_from_1h,") {
		t.Fatalf("expected 1h checkpoint to contain temp_1d_from_1h")
	}
}
