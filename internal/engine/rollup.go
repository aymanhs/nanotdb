package engine

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultRollupCheckpointFile = "rollup.checkpoints.log"

// TriggerRollupsForSource computes rollups for one source database using jobs
// configured in the source database manifest.
func (e *Engine) TriggerRollupsForSource(sourceDBName string) {
	sourceDBName = strings.TrimSpace(sourceDBName)
	if sourceDBName == "" {
		return
	}
	sourceDB, sourceRT, err := e.getOrCreateDB(sourceDBName)
	if err != nil {
		return
	}
	if !sourceRT.info.Rollups.Enabled || len(sourceRT.info.Rollups.Jobs) == 0 {
		return
	}
	jobs := expandRollupJobs(sourceDB, sourceRT.info.Rollups)
	if len(jobs) == 0 {
		return
	}

	checkpoints, err := loadRollupCheckpoints(sourceDB.RootDataDir, sourceRT.info.Rollups.CheckpointFile)
	if err != nil {
		return
	}
	for _, job := range jobs {
		completed := e.processRollupJob(sourceDB, sourceRT, job, checkpoints[job.ID])
		if completed <= checkpoints[job.ID] {
			continue
		}
		if err := appendRollupCheckpoint(sourceDB.RootDataDir, sourceRT.info.Rollups.CheckpointFile, job.ID, completed); err != nil {
			continue
		}
		checkpoints[job.ID] = completed
	}
}

func expandRollupJobs(sourceDB *Database, cfg DBManifestRollups) []DBManifestRollupJob {
	metrics := sourceDB.catalog.ListMetrics()
	out := make([]DBManifestRollupJob, 0, len(cfg.Jobs))

	for _, job := range cfg.Jobs {
		if strings.TrimSpace(job.SourceMetric) != "" {
			out = append(out, job)
			continue
		}

		pattern := strings.TrimSpace(job.SourcePattern)
		if pattern == "" {
			continue
		}

		for _, info := range metrics {
			name := info.Name
			if !matchMetricPattern(pattern, name) {
				continue
			}
			if isRollupMetricExcluded(name, cfg.GlobalExcludePatterns, job.ExcludePatterns) {
				continue
			}

			expanded := job
			expanded.SourceMetric = name
			expanded.SourcePattern = ""
			expanded.ExcludePatterns = nil
			expanded.ID = job.ID + "::" + name
			if strings.TrimSpace(expanded.DestinationMetricPrefix) == "" {
				expanded.DestinationMetricPrefix = name
			}
			out = append(out, expanded)
		}
	}

	return out
}

func matchMetricPattern(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func isRollupMetricExcluded(name string, globalPatterns, jobPatterns []string) bool {
	for _, pat := range globalPatterns {
		pat = strings.TrimSpace(pat)
		if pat != "" && matchMetricPattern(pat, name) {
			return true
		}
	}
	for _, pat := range jobPatterns {
		pat = strings.TrimSpace(pat)
		if pat != "" && matchMetricPattern(pat, name) {
			return true
		}
	}
	return false
}

// TriggerRollupsForSources computes rollups for each unique source database name.
func (e *Engine) TriggerRollupsForSources(sourceDBNames []string) {
	seen := make(map[string]struct{}, len(sourceDBNames))
	for _, name := range sourceDBNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		seen[name] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e.TriggerRollupsForSource(name)
	}
}

// triggerRollups is called after a file is closed/flushed.
func (e *Engine) triggerRollups(sourceDBName string) {
	e.TriggerRollupsForSource(sourceDBName)
}

func (e *Engine) processRollupJob(sourceDB *Database, sourceRT *dbRuntime, job DBManifestRollupJob, lastCompleted Timestamp) Timestamp {
	interval, err := time.ParseDuration(job.Interval)
	if err != nil || interval <= 0 {
		return lastCompleted
	}

	rollupDefaults := defaultRollupDestinationDBInfo(e.dbDefaults, interval)
	rollupDB, _, err := e.getOrCreateDBWithDefaults(job.DestinationDB, rollupDefaults, true, interval)
	if err != nil {
		return lastCompleted
	}

	sourceEntry, ok := sourceDB.catalog.GetMetricEntry(job.SourceMetric)
	if !ok || !sourceEntry.LastValid {
		return lastCompleted
	}

	grace, err := resolveRollupGrace(sourceRT.info, job)
	if err != nil {
		grace = 1 * time.Hour
	}

	safeTS := Timestamp(time.Now().Add(-grace).UnixNano())
	// Only compute fully closed periods. Allowing partial intervals to checkpoint
	// causes incorrect aggregates when more source points arrive later.
	maxSafeBySource := sourceEntry.LastTS
	if maxSafeBySource < safeTS {
		safeTS = maxSafeBySource
	}
	startTS := lastCompleted
	if startTS == 0 {
		startTS = initialRollupStart(sourceDB, sourceRT, interval)
		if startTS == 0 {
			return lastCompleted
		}
	}

	newLastCompleted := lastCompleted

	for {
		periodStart := startTS
		periodEnd := periodStart + Timestamp(interval)

		if periodEnd > safeTS {
			break // not safe to compute yet
		}

		if err := e.buildRollupJobPeriod(rollupDB, sourceDB, job, periodStart, periodEnd); err != nil {
			break
		}
		startTS = periodEnd
		newLastCompleted = periodEnd
	}

	return newLastCompleted
}

func (e *Engine) buildRollupJobPeriod(rollupDB *Database, sourceDB *Database, job DBManifestRollupJob, periodStart, periodEnd Timestamp) error {
	points := make([]float32, 0, 256)

	err := e.QueryRange(sourceDB.Name, job.SourceMetric, periodStart, periodEnd-1, 1, func(s Sample) error {
		var val float32
		if s.ValueType == Int32Sample {
			val = float32(s.Int32)
		} else {
			val = s.Float32
		}
		points = append(points, val)
		return nil
	})
	if err != nil || len(points) == 0 {
		return err
	}

	prefix := strings.TrimSpace(job.DestinationMetricPrefix)
	if prefix == "" {
		prefix = job.SourceMetric
	}
	for _, agg := range job.Aggregates {
		aggFn, ok := getRollupAggregator(agg)
		if !ok {
			continue
		}
		value, err := aggFn.Compute(periodStart, periodEnd, points)
		if err != nil {
			return err
		}
		if err := e.insertRollupSample(rollupDB.Name, prefix+"."+aggFn.Name(), periodStart, value); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) insertRollupSample(dbName, metricName string, ts Timestamp, val float32) error {
	return e.addFloatSample(dbName, metricName, ts, val, false, true)
}

func resolveRollupGrace(info DBInfo, job DBManifestRollupJob) (time.Duration, error) {
	if strings.TrimSpace(job.Grace) != "" {
		return time.ParseDuration(job.Grace)
	}
	if strings.TrimSpace(info.Rollups.DefaultGrace) != "" {
		return time.ParseDuration(info.Rollups.DefaultGrace)
	}
	return time.ParseDuration(info.Grace)
}

func initialRollupStart(sourceDB *Database, sourceRT *dbRuntime, interval time.Duration) Timestamp {
	var earliest Timestamp
	for _, page := range sourceRT.openDays {
		if page == nil {
			continue
		}
		if earliest == 0 || page.Start < earliest {
			earliest = page.Start
		}
	}
	entries, err := os.ReadDir(sourceDB.RootDataDir)
	if err == nil {
		for _, ent := range entries {
			name := ent.Name()
			if ent.IsDir() || !strings.HasPrefix(name, "data-") || !strings.HasSuffix(name, ".dat") {
				continue
			}
			stats, err := WalkDataFileHeaders(filepath.Join(sourceDB.RootDataDir, name), nil)
			if err != nil {
				continue
			}
			if stats.Frames == 0 {
				continue
			}
			if earliest == 0 || stats.MinStart < earliest {
				earliest = stats.MinStart
			}
		}
	}
	if earliest == 0 {
		return 0
	}
	return floorTimestamp(earliest, interval)
}

func floorTimestamp(ts Timestamp, interval time.Duration) Timestamp {
	if interval <= 0 {
		return ts
	}
	step := int64(interval)
	if step <= 0 {
		return ts
	}
	return Timestamp((int64(ts) / step) * step)
}

func loadRollupCheckpoints(rootDir, fileName string) (map[string]Timestamp, error) {
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		fileName = defaultRollupCheckpointFile
	}
	path := filepath.Join(rootDir, fileName)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(map[string]Timestamp), nil
	}
	if err != nil {
		return nil, err
	}
	checkpoints := make(map[string]Timestamp)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			continue
		}
		jobID := strings.TrimSpace(parts[0])
		if jobID == "" {
			continue
		}
		ts, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			continue
		}
		checkpoints[jobID] = Timestamp(ts)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return checkpoints, nil
}

func appendRollupCheckpoint(rootDir, fileName, jobID string, completed Timestamp) error {
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		fileName = defaultRollupCheckpointFile
	}
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(rootDir, fileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s,%d\n", jobID, completed); err != nil {
		return err
	}
	return f.Sync()
}
