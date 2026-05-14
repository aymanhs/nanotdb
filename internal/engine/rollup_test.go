package engine

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBuildRollupJobPeriodComputesConfiguredAggregations(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	periodStart := Timestamp(1_000_000_000)
	periodEnd := periodStart + Timestamp(time.Hour)

	if err := e.AddLine("prod/sensors.temp 10 " + itoa64(int64(periodStart)+1)); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.AddLine("prod/sensors.temp 20 " + itoa64(int64(periodStart)+2)); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.AddLine("prod/sensors.temp 40 " + itoa64(int64(periodStart)+3)); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.AddLine("prod/sensors.temp 100 " + itoa64(int64(periodEnd))); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}

	sourceDB, _, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB source failed: %v", err)
	}
	rollupDB, _, err := e.getOrCreateDB("rollup")
	if err != nil {
		t.Fatalf("getOrCreateDB rollup failed: %v", err)
	}

	job := DBManifestRollupJob{
		ID:                      "temp-1h",
		SourceMetric:            "sensors.temp",
		Interval:                "1h",
		Aggregates:              []string{"min", "max", "sum", "avg", "count"},
		DestinationDB:           "rollup",
		DestinationMetricPrefix: "sensors.temp",
	}

	if err := e.buildRollupJobPeriod(rollupDB, sourceDB, job, periodStart, periodEnd); err != nil {
		t.Fatalf("buildRollupJobPeriod failed: %v", err)
	}

	assertRollupValue(t, e, "rollup", "sensors.temp.min", periodStart, 10)
	assertRollupValue(t, e, "rollup", "sensors.temp.max", periodStart, 40)
	assertRollupValue(t, e, "rollup", "sensors.temp.sum", periodStart, 70)
	assertRollupValue(t, e, "rollup", "sensors.temp.avg", periodStart, float32(70.0/3.0))
	assertRollupValue(t, e, "rollup", "sensors.temp.count", periodStart, 3)
}

func TestTriggerRollupsOnlyProcessesRequestedSourceDatabase(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UTC().Truncate(time.Second)
	periodStart := now.Add(-2 * time.Hour).Truncate(time.Hour)
	ts := Timestamp(periodStart.Add(10 * time.Minute).UnixNano())

	if err := e.AddLine("prod/sensors.temp 7 " + itoa64(int64(ts))); err != nil {
		t.Fatalf("AddLine prod failed: %v", err)
	}
	if err := e.AddLine("other/sensors.temp 11 " + itoa64(int64(ts))); err != nil {
		t.Fatalf("AddLine other failed: %v", err)
	}

	_, prodRT, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB prod failed: %v", err)
	}
	prodRT.info.Rollups = DBManifestRollups{
		Enabled:        true,
		CheckpointFile: defaultRollupCheckpointFile,
		DefaultGrace:   "30m",
		Jobs: []DBManifestRollupJob{{
			ID:                      "prod-temp-1h",
			SourceMetric:            "sensors.temp",
			Interval:                "1h",
			Aggregates:              []string{"sum"},
			DestinationDB:           "prod_rollup",
			DestinationMetricPrefix: "sensors.temp",
		}},
	}

	_, otherRT, err := e.getOrCreateDB("other")
	if err != nil {
		t.Fatalf("getOrCreateDB other failed: %v", err)
	}
	otherRT.info.Rollups = DBManifestRollups{
		Enabled:        true,
		CheckpointFile: defaultRollupCheckpointFile,
		DefaultGrace:   "30m",
		Jobs: []DBManifestRollupJob{{
			ID:                      "other-temp-1h",
			SourceMetric:            "sensors.temp",
			Interval:                "1h",
			Aggregates:              []string{"sum"},
			DestinationDB:           "other_rollup",
			DestinationMetricPrefix: "sensors.temp",
		}},
	}

	e.TriggerRollupsForSource("prod")

	assertRollupValue(t, e, "prod_rollup", "sensors.temp.sum", Timestamp(periodStart.UnixNano()), 7)

	_, found, err := e.QueryLast("other_rollup", "sensors.temp.sum")
	if err != nil {
		t.Fatalf("QueryLast other_rollup failed: %v", err)
	}
	if found {
		t.Fatalf("expected no rollup sample for non-triggered source database")
	}
}

func TestTriggerRollupsForSourceAppendsCheckpoint(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UTC().Truncate(time.Second)
	periodStart := now.Add(-2 * time.Hour).Truncate(time.Hour)
	ts1 := Timestamp(periodStart.Add(5 * time.Minute).UnixNano())
	ts2 := Timestamp(periodStart.Add(20 * time.Minute).UnixNano())
	ts3 := Timestamp(periodStart.Add(1 * time.Hour).UnixNano())

	if err := e.AddLine("prod/sensors.temp 3 " + itoa64(int64(ts1))); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.AddLine("prod/sensors.temp 5 " + itoa64(int64(ts2))); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.AddLine("prod/sensors.temp 7 " + itoa64(int64(ts3))); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}

	_, prodRT, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB prod failed: %v", err)
	}
	prodRT.info.Rollups = DBManifestRollups{
		Enabled:        true,
		CheckpointFile: "rollup.checkpoints.log",
		DefaultGrace:   "30m",
		Jobs: []DBManifestRollupJob{{
			ID:                      "prod-temp-1h",
			SourceMetric:            "sensors.temp",
			Interval:                "1h",
			Aggregates:              []string{"sum"},
			DestinationDB:           "prod_rollup",
			DestinationMetricPrefix: "sensors.temp",
		}},
	}

	e.TriggerRollupsForSource("prod")

	var foundFirst bool
	err = e.QueryRange("prod_rollup", "sensors.temp.sum", Timestamp(periodStart.UnixNano()), Timestamp(periodStart.Add(time.Hour).UnixNano()-1), 1, func(s Sample) error {
		if s.TS == Timestamp(periodStart.UnixNano()) && math.Abs(float64(s.Float32-8)) < 0.0001 {
			foundFirst = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if !foundFirst {
		t.Fatalf("expected first period rollup value for prod_rollup/sensors.temp.sum")
	}

	raw, err := os.ReadFile(filepath.Join(root, "prod", "rollup.checkpoints.log"))
	if err != nil {
		t.Fatalf("ReadFile checkpoint failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 {
		t.Fatalf("expected non-empty checkpoint log")
	}
	last := lines[len(lines)-1]
	parts := strings.Split(last, ",")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) != "prod-temp-1h" {
		t.Fatalf("unexpected checkpoint line: %q", last)
	}
	checkpointTS, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		t.Fatalf("invalid checkpoint timestamp in %q: %v", last, err)
	}
	if checkpointTS < periodStart.Add(time.Hour).UnixNano() {
		t.Fatalf("expected checkpoint to advance at least one interval, got=%d", checkpointTS)
	}
}

func TestTriggerRollupsDoesNotAdvancePastSourceMetricLastTS(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UTC().Truncate(time.Second)
	periodStart := now.Add(-2 * time.Hour).Truncate(time.Hour)

	// Only partial data in first hour; rollup may finalize this hour, but checkpoint
	// must not leap far beyond source LastTS.
	ts1 := Timestamp(periodStart.Add(5 * time.Minute).UnixNano())
	ts2 := Timestamp(periodStart.Add(20 * time.Minute).UnixNano())
	if err := e.AddLine("prod/sensors.temp 3 " + itoa64(int64(ts1))); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.AddLine("prod/sensors.temp 5 " + itoa64(int64(ts2))); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}

	_, prodRT, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB prod failed: %v", err)
	}
	prodRT.info.Rollups = DBManifestRollups{
		Enabled:        true,
		CheckpointFile: "rollup.checkpoints.log",
		DefaultGrace:   "0s",
		Jobs: []DBManifestRollupJob{{
			ID:                      "prod-temp-1h",
			SourceMetric:            "sensors.temp",
			Interval:                "1h",
			Aggregates:              []string{"sum"},
			DestinationDB:           "prod_rollup",
			DestinationMetricPrefix: "sensors.temp",
		}},
	}

	e.TriggerRollupsForSource("prod")
	assertRollupValue(t, e, "prod_rollup", "sensors.temp.sum", Timestamp(periodStart.UnixNano()), 8)

	checkpointPath := filepath.Join(root, "prod", "rollup.checkpoints.log")
	raw, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatalf("ReadFile checkpoint failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) == "" {
		t.Fatalf("expected checkpoint entry")
	}
	parts := strings.Split(lines[len(lines)-1], ",")
	if len(parts) != 2 {
		t.Fatalf("unexpected checkpoint line: %q", lines[len(lines)-1])
	}
	checkpointTS, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		t.Fatalf("invalid checkpoint timestamp in %q: %v", lines[len(lines)-1], err)
	}
	wantCheckpoint := periodStart.Add(1 * time.Hour).UnixNano()
	if checkpointTS != wantCheckpoint {
		t.Fatalf("checkpoint mismatch: got=%d want=%d", checkpointTS, wantCheckpoint)
	}
}

func TestTriggerRollups_MultiJobSameDestinationDoesNotDropSecondJob(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-4 * time.Hour).Truncate(time.Hour)

	for i := 0; i < 3; i++ {
		tsA := Timestamp(base.Add(time.Duration(i)*time.Hour + 10*time.Minute).UnixNano())
		tsB := Timestamp(base.Add(time.Duration(i)*time.Hour + 20*time.Minute).UnixNano())
		if err := e.AddLine("prod/temp.office_dry 10 " + itoa64(int64(tsA))); err != nil {
			t.Fatalf("AddLine office failed: %v", err)
		}
		if err := e.AddLine("prod/temp.out_dry 20 " + itoa64(int64(tsB))); err != nil {
			t.Fatalf("AddLine outside failed: %v", err)
		}
	}

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB prod failed: %v", err)
	}
	rt.info.Rollups = DBManifestRollups{
		Enabled:        true,
		CheckpointFile: defaultRollupCheckpointFile,
		DefaultGrace:   "0s",
		Jobs: []DBManifestRollupJob{
			{
				ID:                      "office_1h",
				SourceMetric:            "temp.office_dry",
				Interval:                "1h",
				Aggregates:              []string{"min"},
				DestinationDB:           "prod_rollup_1h",
				DestinationMetricPrefix: "temp.office",
			},
			{
				ID:                      "outside_1h",
				SourceMetric:            "temp.out_dry",
				Interval:                "1h",
				Aggregates:              []string{"min"},
				DestinationDB:           "prod_rollup_1h",
				DestinationMetricPrefix: "temp.out_dry",
			},
		},
	}

	e.TriggerRollupsForSource("prod")

	rowsOffice := 0
	err = e.QueryRange("prod_rollup_1h", "temp.office.min", Timestamp(base.UnixNano()), Timestamp(now.UnixNano()), 1, func(s Sample) error {
		rowsOffice++
		return nil
	})
	if err != nil {
		t.Fatalf("QueryRange office failed: %v", err)
	}
	if rowsOffice == 0 {
		t.Fatalf("expected office rollup rows")
	}

	rowsOutside := 0
	err = e.QueryRange("prod_rollup_1h", "temp.out_dry.min", Timestamp(base.UnixNano()), Timestamp(now.UnixNano()), 1, func(s Sample) error {
		rowsOutside++
		return nil
	})
	if err != nil {
		t.Fatalf("QueryRange outside failed: %v", err)
	}
	if rowsOutside == 0 {
		t.Fatalf("expected outside rollup rows")
	}
}

func assertRollupValue(t *testing.T, e *Engine, dbName, metric string, wantTS Timestamp, want float32) {
	t.Helper()

	s, found, err := e.QueryLast(dbName, metric)
	if err != nil {
		t.Fatalf("QueryLast(%s/%s) failed: %v", dbName, metric, err)
	}
	if !found {
		t.Fatalf("expected rollup sample for %s/%s", dbName, metric)
	}
	if s.ValueType != Float32Sample {
		t.Fatalf("expected float32 rollup value, got type=%d", s.ValueType)
	}
	if s.TS != wantTS {
		t.Fatalf("timestamp mismatch for %s/%s: got=%d want=%d", dbName, metric, s.TS, wantTS)
	}
	if math.Abs(float64(s.Float32-want)) > 0.0001 {
		t.Fatalf("value mismatch for %s/%s: got=%f want=%f", dbName, metric, s.Float32, want)
	}
}
