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
	if err := e.AddLine("prod/sensors.temp 1 " + itoa64(int64(periodStart.Add(1*time.Hour).UnixNano()))); err != nil {
		t.Fatalf("AddLine prod close-hour failed: %v", err)
	}
	if err := e.AddLine("other/sensors.temp 11 " + itoa64(int64(ts))); err != nil {
		t.Fatalf("AddLine other failed: %v", err)
	}
	if err := e.AddLine("other/sensors.temp 1 " + itoa64(int64(periodStart.Add(1*time.Hour).UnixNano()))); err != nil {
		t.Fatalf("AddLine other close-hour failed: %v", err)
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

func TestTriggerRollupsDoesNotComputePartialInterval(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UTC().Truncate(time.Second)
	periodStart := now.Add(-2 * time.Hour).Truncate(time.Hour)

	// Only partial data in first hour; rollup must not finalize this interval yet.
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

	_, found, err := e.QueryLast("prod_rollup", "sensors.temp.sum")
	if err != nil {
		t.Fatalf("QueryLast failed: %v", err)
	}
	if found {
		t.Fatalf("did not expect rollup sample for partial interval")
	}

	checkpointPath := filepath.Join(root, "prod", "rollup.checkpoints.log")
	raw, err := os.ReadFile(checkpointPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("ReadFile checkpoint failed: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "" {
		t.Fatalf("did not expect checkpoint entry for partial interval, got %q", strings.TrimSpace(string(raw)))
	}
}

func TestTriggerRollups_MultiJobSameDestinationDoesNotDropSecondJob(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}

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

	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	reopened, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine reopen failed: %v", err)
	}
	defer reopened.Close()

	rowsOffice = 0
	err = reopened.QueryRange("prod_rollup_1h", "temp.office.min", Timestamp(base.UnixNano()), Timestamp(now.UnixNano()), 1, func(s Sample) error {
		rowsOffice++
		return nil
	})
	if err != nil {
		t.Fatalf("QueryRange office after reopen failed: %v", err)
	}
	if rowsOffice == 0 {
		t.Fatalf("expected office rollup rows after reopen")
	}

	rowsOutside = 0
	err = reopened.QueryRange("prod_rollup_1h", "temp.out_dry.min", Timestamp(base.UnixNano()), Timestamp(now.UnixNano()), 1, func(s Sample) error {
		rowsOutside++
		return nil
	})
	if err != nil {
		t.Fatalf("QueryRange outside after reopen failed: %v", err)
	}
	if rowsOutside == 0 {
		t.Fatalf("expected outside rollup rows after reopen")
	}
}

func TestTriggerRollups_MultiJobSameDestinationCoalescesFrames(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}

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
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	stats, frames, err := ScanDataFileHeaders(filepath.Join(root, "prod_rollup_1h", "data-"+base.Format("2006-01")+".dat"))
	if err != nil {
		t.Fatalf("ScanDataFileHeaders failed: %v", err)
	}
	if stats.Frames != 1 {
		t.Fatalf("expected one coalesced frame, got %d", stats.Frames)
	}
	if len(frames) != 1 {
		t.Fatalf("expected one frame entry, got %d", len(frames))
	}
	if frames[0].NumRecords != 4 {
		t.Fatalf("expected 4 coalesced records from two fully closed periods, got %d", frames[0].NumRecords)
	}
}

func TestTriggerRollups_AutoJobWildcardWithExclusions(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-3 * time.Hour).Truncate(time.Hour)

	if err := e.AddLine("prod/temp.outside 30 " + itoa64(base.Add(10*time.Minute).UnixNano())); err != nil {
		t.Fatalf("AddLine outside failed: %v", err)
	}
	if err := e.AddLine("prod/temp.office 10 " + itoa64(base.Add(15*time.Minute).UnixNano())); err != nil {
		t.Fatalf("AddLine office failed: %v", err)
	}
	if err := e.AddLine("prod/temp.outside 1 " + itoa64(base.Add(1*time.Hour).UnixNano())); err != nil {
		t.Fatalf("AddLine outside close failed: %v", err)
	}
	if err := e.AddLine("prod/temp.office 1 " + itoa64(base.Add(1*time.Hour+5*time.Minute).UnixNano())); err != nil {
		t.Fatalf("AddLine office close failed: %v", err)
	}

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB prod failed: %v", err)
	}
	rt.info.Rollups = DBManifestRollups{
		Enabled:               true,
		CheckpointFile:        defaultRollupCheckpointFile,
		DefaultGrace:          "0s",
		GlobalExcludePatterns: []string{"temp.out*"},
		Jobs: []DBManifestRollupJob{{
			ID:            "all-temp-1h",
			SourcePattern: "temp.*",
			ExcludePatterns: []string{
				"*.debug",
			},
			Interval:      "1h",
			Aggregates:    []string{"sum"},
			DestinationDB: "prod_rollup_1h",
		}},
	}

	e.TriggerRollupsForSource("prod")

	assertRollupValue(t, e, "prod_rollup_1h", "temp.office.sum", Timestamp(base.UnixNano()), 10)

	if _, found, err := e.QueryLast("prod_rollup_1h", "temp.outside.sum"); err != nil {
		t.Fatalf("QueryLast outside failed: %v", err)
	} else if found {
		t.Fatalf("expected temp.outside to be excluded from auto rollup job")
	}
}

func TestTriggerRollups_CreatedRollupDBUsesHourlyDefaults(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-3 * time.Hour).Truncate(time.Hour)
	if err := e.AddLine("prod/temp.office 10 " + itoa64(base.Add(10*time.Minute).UnixNano())); err != nil {
		t.Fatalf("AddLine sample failed: %v", err)
	}
	if err := e.AddLine("prod/temp.office 1 " + itoa64(base.Add(1*time.Hour).UnixNano())); err != nil {
		t.Fatalf("AddLine close sample failed: %v", err)
	}

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB prod failed: %v", err)
	}
	rt.info.Rollups = DBManifestRollups{
		Enabled:        true,
		CheckpointFile: defaultRollupCheckpointFile,
		DefaultGrace:   "0s",
		Jobs: []DBManifestRollupJob{{
			ID:                      "temp-1h",
			SourceMetric:            "temp.office",
			Interval:                "1h",
			Aggregates:              []string{"sum"},
			DestinationDB:           "prod_rollup_1h",
			DestinationMetricPrefix: "temp.office",
		}},
	}

	e.TriggerRollupsForSource("prod")

	raw, err := os.ReadFile(filepath.Join(root, "prod_rollup_1h", "manifest.toml"))
	if err != nil {
		t.Fatalf("ReadFile manifest failed: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "WAL is disabled because rollup data is derived") {
		t.Fatalf("expected rollup manifest rationale comment, got: %s", text)
	}
	if !strings.Contains(text, "enabled = false") {
		t.Fatalf("expected rollup wal disabled in manifest")
	}
	if !strings.Contains(text, "partition = \"month\"") {
		t.Fatalf("expected monthly partition for hourly rollups")
	}
	if !strings.Contains(text, "max_age = \"6h\"") {
		t.Fatalf("expected 6h page max_age for hourly rollups")
	}
}

func TestTriggerRollupsForSourceReturnsAfterRollupJobError(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-3 * time.Hour).Truncate(time.Hour)
	if err := e.AddLine("prod/temp.office 10 " + itoa64(base.Add(10*time.Minute).UnixNano())); err != nil {
		t.Fatalf("AddLine first sample failed: %v", err)
	}
	if err := e.AddLine("prod/temp.office 20 " + itoa64(base.Add(40*time.Minute).UnixNano())); err != nil {
		t.Fatalf("AddLine second sample failed: %v", err)
	}
	if err := e.AddLine("prod/temp.office 1 " + itoa64(base.Add(1*time.Hour).UnixNano())); err != nil {
		t.Fatalf("AddLine close sample failed: %v", err)
	}

	if err := e.AddLine("prod_rollup_1h/temp.office.sum 99.5 " + itoa64(base.Add(2*time.Hour).UnixNano())); err != nil {
		t.Fatalf("AddLine conflicting destination metric failed: %v", err)
	}

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB prod failed: %v", err)
	}
	rt.info.Rollups = DBManifestRollups{
		Enabled:        true,
		CheckpointFile: defaultRollupCheckpointFile,
		DefaultGrace:   "0s",
		Jobs: []DBManifestRollupJob{{
			ID:                      "temp-1h",
			SourceMetric:            "temp.office",
			Interval:                "1h",
			Aggregates:              []string{"sum"},
			DestinationDB:           "prod_rollup_1h",
			DestinationMetricPrefix: "temp.office",
		}},
	}

	done := make(chan struct{})
	go func() {
		e.TriggerRollupsForSource("prod")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("TriggerRollupsForSource hung after rollup job error")
	}

	checkpointPath := filepath.Join(root, "prod", defaultRollupCheckpointFile)
	if _, err := os.Stat(checkpointPath); !os.IsNotExist(err) {
		t.Fatalf("expected no checkpoint file after failed rollup, got err=%v", err)
	}

	if sample, found, err := e.QueryLast("prod_rollup_1h", "temp.office.sum"); err != nil {
		t.Fatalf("QueryLast destination failed: %v", err)
	} else if !found {
		t.Fatalf("expected conflicting destination metric to remain present")
	} else if sample.TS != Timestamp(base.Add(2*time.Hour).UnixNano()) {
		t.Fatalf("destination metric timestamp mismatch: got=%d want=%d", sample.TS, base.Add(2*time.Hour).UnixNano())
	}
}

func TestDefaultRollupDestinationDBInfoForDailyRollups(t *testing.T) {
	info := defaultRollupDestinationDBInfo(defaultDBInfo(), 24*time.Hour)
	if info.WALEnabled {
		t.Fatalf("expected WAL disabled for rollup destination")
	}
	if info.Partition != "year" {
		t.Fatalf("partition mismatch: got=%q want=%q", info.Partition, "year")
	}
	if info.PageMaxAge != "168h" {
		t.Fatalf("page max age mismatch: got=%q want=%q", info.PageMaxAge, "168h")
	}
}

func TestBackfillRollups_RebuildsDestinationState(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-3 * time.Hour).Truncate(time.Hour)
	if err := e.AddLine("prod/temp.office 10 " + itoa64(base.Add(10*time.Minute).UnixNano())); err != nil {
		t.Fatalf("AddLine first sample failed: %v", err)
	}
	if err := e.AddLine("prod/temp.office 20 " + itoa64(base.Add(40*time.Minute).UnixNano())); err != nil {
		t.Fatalf("AddLine second sample failed: %v", err)
	}
	if err := e.AddLine("prod/temp.office 1 " + itoa64(base.Add(1*time.Hour).UnixNano())); err != nil {
		t.Fatalf("AddLine close sample failed: %v", err)
	}

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB prod failed: %v", err)
	}
	rt.info.Rollups = DBManifestRollups{
		Enabled:        true,
		CheckpointFile: defaultRollupCheckpointFile,
		DefaultGrace:   "0s",
		Jobs: []DBManifestRollupJob{{
			ID:                      "temp-1h",
			SourceMetric:            "temp.office",
			Interval:                "1h",
			Aggregates:              []string{"sum"},
			DestinationDB:           "prod_rollup_1h",
			DestinationMetricPrefix: "temp.office",
		}},
	}

	e.TriggerRollupsForSource("prod")

	if err := e.AddLine("prod_rollup_1h/stale.metric 99 " + itoa64(base.Add(90*time.Minute).UnixNano())); err != nil {
		t.Fatalf("AddLine stale destination metric failed: %v", err)
	}

	manifestPath := filepath.Join(root, "prod_rollup_1h", "manifest.toml")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("Remove manifest failed: %v", err)
	}

	report, err := e.BackfillRollups([]string{"prod"})
	if err != nil {
		t.Fatalf("BackfillRollups failed: %v", err)
	}
	if len(report.SourceDatabases) != 1 || report.SourceDatabases[0] != "prod" {
		t.Fatalf("unexpected source databases: %v", report.SourceDatabases)
	}
	if len(report.DestinationDatabases) != 1 || report.DestinationDatabases[0] != "prod_rollup_1h" {
		t.Fatalf("unexpected destination databases: %v", report.DestinationDatabases)
	}

	assertRollupValue(t, e, "prod_rollup_1h", "temp.office.sum", Timestamp(base.UnixNano()), 30)

	if _, found, err := e.QueryLast("prod_rollup_1h", "stale.metric"); err != nil {
		t.Fatalf("QueryLast stale.metric failed: %v", err)
	} else if found {
		t.Fatalf("expected stale destination metric to be removed by backfill")
	}

	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile rebuilt manifest failed: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "WAL is disabled because rollup data is derived") {
		t.Fatalf("expected rebuilt rollup manifest comments, got: %s", text)
	}

	checkpointRaw, err := os.ReadFile(filepath.Join(root, "prod", defaultRollupCheckpointFile))
	if err != nil {
		t.Fatalf("ReadFile checkpoint failed: %v", err)
	}
	if !strings.Contains(string(checkpointRaw), "temp-1h,") {
		t.Fatalf("expected checkpoint to be recreated, got: %s", string(checkpointRaw))
	}

	dataPath := filepath.Join(root, "prod_rollup_1h", "data-"+base.Format("2006-01")+".dat")
	stats, err := WalkDataFileHeaders(dataPath, nil)
	if err != nil {
		t.Fatalf("WalkDataFileHeaders failed: %v", err)
	}
	if stats.Frames == 0 {
		t.Fatalf("expected persisted rollup frames on disk after backfill")
	}

	catalogRaw, err := os.ReadFile(filepath.Join(root, "prod_rollup_1h", "catalog.json"))
	if err != nil {
		t.Fatalf("ReadFile rebuilt catalog failed: %v", err)
	}
	if !strings.Contains(string(catalogRaw), "temp.office.sum") {
		t.Fatalf("expected rebuilt catalog to contain rollup metric, got: %s", string(catalogRaw))
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
