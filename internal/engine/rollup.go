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
const rollupCheckpointCompactBytes = 100 * 1024

type RollupBackfillReport struct {
	RequestedSources       []string `json:"requested_sources"`
	SourceDatabases        []string `json:"source_databases"`
	DestinationDatabases   []string `json:"destination_databases"`
	ClearedCheckpointFiles []string `json:"cleared_checkpoint_files"`
	ClearedDataFiles       []string `json:"cleared_data_files"`
	ClearedWALFiles        []string `json:"cleared_wal_files"`
	ClearedCatalogFiles    []string `json:"cleared_catalog_files"`
	ReplayPasses           int      `json:"replay_passes"`
}

type rollupBackfillPlan struct {
	sources         []string
	destinations    []string
	checkpointPaths map[string]string
}

// TriggerRollupsForSource computes rollups for one source database using jobs
// configured in the source database manifest.
func (e *Engine) TriggerRollupsForSource(sourceDBName string) {
	e.triggerRollupsForSource(sourceDBName, false)
}

func (e *Engine) triggerRollupsForSource(sourceDBName string, writeLockHeld bool) {
	sourceDBName = strings.TrimSpace(sourceDBName)
	if sourceDBName == "" {
		return
	}
	if !writeLockHeld {
		e.writeMu.Lock()
		defer e.writeMu.Unlock()
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
	e.logDebug("rollup trigger started", "source_database", sourceDBName, "jobs", len(jobs))

	checkpoints, err := loadRollupCheckpoints(sourceDB.RootDataDir, sourceRT.info.Rollups.CheckpointFile)
	if err != nil {
		return
	}
	updated := e.processRollupJobGroups(sourceDB, sourceRT, jobs, checkpoints)
	for jobID, completed := range updated {
		if completed <= checkpoints[jobID] {
			continue
		}
		if err := appendRollupCheckpoint(sourceDB.RootDataDir, sourceRT.info.Rollups.CheckpointFile, jobID, completed); err != nil {
			continue
		}
		checkpoints[jobID] = completed
	}
	e.logDebug("rollup trigger finished", "source_database", sourceDBName, "updated_jobs", len(updated))
}

type rollupJobGroup struct {
	destinationDB string
	interval      time.Duration
	jobs          []DBManifestRollupJob
}

type rollupJobState struct {
	job       DBManifestRollupJob
	safeTS    Timestamp
	nextStart Timestamp
	completed Timestamp
}

func (e *Engine) processRollupJobGroups(sourceDB *Database, sourceRT *dbRuntime, jobs []DBManifestRollupJob, checkpoints map[string]Timestamp) map[string]Timestamp {
	updated := make(map[string]Timestamp, len(jobs))
	for _, group := range groupRollupJobs(jobs) {
		groupUpdated := e.processRollupJobGroup(sourceDB, sourceRT, group, checkpoints)
		for jobID, completed := range groupUpdated {
			updated[jobID] = completed
		}
	}
	return updated
}

func groupRollupJobs(jobs []DBManifestRollupJob) []rollupJobGroup {
	groupsByKey := make(map[string]*rollupJobGroup)
	order := make([]string, 0, len(jobs))

	for _, job := range jobs {
		interval, err := time.ParseDuration(job.Interval)
		if err != nil || interval <= 0 {
			continue
		}
		key := job.DestinationDB + "\x00" + strconv.FormatInt(int64(interval), 10)
		group := groupsByKey[key]
		if group == nil {
			group = &rollupJobGroup{destinationDB: job.DestinationDB, interval: interval}
			groupsByKey[key] = group
			order = append(order, key)
		}
		group.jobs = append(group.jobs, job)
	}

	out := make([]rollupJobGroup, 0, len(order))
	for _, key := range order {
		group := groupsByKey[key]
		sort.Slice(group.jobs, func(i, j int) bool {
			return group.jobs[i].ID < group.jobs[j].ID
		})
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].destinationDB == out[j].destinationDB {
			return out[i].interval < out[j].interval
		}
		return out[i].destinationDB < out[j].destinationDB
	})
	return out
}

func (e *Engine) processRollupJobGroup(sourceDB *Database, sourceRT *dbRuntime, group rollupJobGroup, checkpoints map[string]Timestamp) map[string]Timestamp {
	rollupDefaults := defaultRollupDestinationDBInfo(e.dbDefaults, group.interval)
	rollupDB, _, err := e.getOrCreateDBWithDefaults(group.destinationDB, rollupDefaults, true, group.interval)
	if err != nil {
		return nil
	}

	states := make([]rollupJobState, 0, len(group.jobs))
	for _, job := range group.jobs {
		if _, ok := sourceDB.catalog.GetMetricEntry(job.SourceMetric); !ok {
			continue
		}
		safeTS, ok := latestClosedMetricTimestamp(sourceDB, sourceRT, job.SourceMetric)
		if !ok {
			continue
		}
		grace, err := resolveRollupGrace(sourceRT.info, job)
		if err != nil {
			grace = 1 * time.Hour
		}
		cutoff := Timestamp(time.Now().Add(-grace).UnixNano())
		if safeTS > cutoff {
			safeTS = cutoff
		}
		startTS := checkpoints[job.ID]
		if startTS == 0 {
			startTS = initialRollupStart(sourceDB, sourceRT, group.interval)
		}
		if startTS == 0 {
			continue
		}
		states = append(states, rollupJobState{job: job, safeTS: safeTS, nextStart: startTS, completed: checkpoints[job.ID]})
	}
	if len(states) == 0 {
		return nil
	}

	updated := make(map[string]Timestamp, len(states))
	for {
		nextPeriodStart := Timestamp(0)
		for _, state := range states {
			if state.nextStart == 0 || state.nextStart+Timestamp(group.interval) > state.safeTS {
				continue
			}
			if nextPeriodStart == 0 || state.nextStart < nextPeriodStart {
				nextPeriodStart = state.nextStart
			}
		}
		if nextPeriodStart == 0 {
			break
		}

		periodEnd := nextPeriodStart + Timestamp(group.interval)
		for idx := range states {
			state := &states[idx]
			if state.nextStart != nextPeriodStart || periodEnd > state.safeTS {
				continue
			}
			if err := e.buildRollupJobPeriod(rollupDB, sourceDB, state.job, nextPeriodStart, periodEnd); err != nil {
				e.logDebug("rollup period failed", "source_database", sourceDB.Name, "destination_database", group.destinationDB, "job_id", state.job.ID, "period_start", nextPeriodStart, "period_end", periodEnd, "error", err)
				state.nextStart = 0
				continue
			}
			state.nextStart = periodEnd
			state.completed = periodEnd
			updated[state.job.ID] = periodEnd
		}
	}

	return updated
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

func (e *Engine) BackfillRollups(sourceDBNames []string) (RollupBackfillReport, error) {
	report := RollupBackfillReport{RequestedSources: normalizeDatabaseNames(sourceDBNames)}
	e.logInfo("rollup backfill started", "sources", report.RequestedSources)

	e.rollupBackfill.Lock()
	defer e.rollupBackfill.Unlock()

	plan, err := e.planRollupBackfill(report.RequestedSources)
	if err != nil {
		return report, err
	}
	report.SourceDatabases = append(report.SourceDatabases, plan.sources...)
	report.DestinationDatabases = append(report.DestinationDatabases, plan.destinations...)
	if len(plan.sources) == 0 {
		return report, nil
	}

	for _, dbName := range plan.destinations {
		dataFiles, walFiles, catalogFiles, err := e.resetRollupDestination(dbName)
		if err != nil {
			return report, err
		}
		report.ClearedDataFiles = append(report.ClearedDataFiles, dataFiles...)
		report.ClearedWALFiles = append(report.ClearedWALFiles, walFiles...)
		report.ClearedCatalogFiles = append(report.ClearedCatalogFiles, catalogFiles...)
	}

	for _, sourceDBName := range plan.sources {
		path := plan.checkpointPaths[sourceDBName]
		if path == "" {
			continue
		}
		if err := os.Remove(path); err == nil {
			report.ClearedCheckpointFiles = append(report.ClearedCheckpointFiles, path)
		} else if !os.IsNotExist(err) {
			return report, err
		}
	}

	passes := len(plan.sources)
	if passes < 1 {
		passes = 1
	}
	for pass := 0; pass < passes; pass++ {
		for _, sourceDBName := range plan.sources {
			e.TriggerRollupsForSource(sourceDBName)
		}
		if err := e.flushDatabases(plan.destinations); err != nil {
			return report, err
		}
	}
	report.ReplayPasses = passes
	e.logInfo("rollup backfill finished", "sources", report.SourceDatabases, "destinations", report.DestinationDatabases, "replay_passes", report.ReplayPasses)
	return report, nil
}

func normalizeDatabaseNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) planRollupBackfill(requested []string) (rollupBackfillPlan, error) {
	requested = normalizeDatabaseNames(requested)
	queue := append([]string(nil), requested...)
	if len(queue) == 0 {
		queue = e.discoverRollupSourceCandidates()
	}

	seen := make(map[string]struct{}, len(queue))
	sourceSet := make(map[string]struct{})
	destinationSet := make(map[string]struct{})
	checkpointPaths := make(map[string]string)

	for len(queue) > 0 {
		dbName := queue[0]
		queue = queue[1:]
		if _, ok := seen[dbName]; ok {
			continue
		}
		seen[dbName] = struct{}{}

		info, ok, err := e.rollupInfoForDB(dbName)
		if err != nil {
			return rollupBackfillPlan{}, err
		}
		if !ok || !info.Rollups.Enabled || len(info.Rollups.Jobs) == 0 {
			continue
		}

		sourceSet[dbName] = struct{}{}
		checkpointFile := strings.TrimSpace(info.Rollups.CheckpointFile)
		if checkpointFile == "" {
			checkpointFile = defaultRollupCheckpointFile
		}
		checkpointPaths[dbName] = filepath.Join(e.RootDataDir, dbName, checkpointFile)

		for _, job := range info.Rollups.Jobs {
			dest := strings.TrimSpace(job.DestinationDB)
			if dest == "" {
				continue
			}
			destinationSet[dest] = struct{}{}
			queue = append(queue, dest)
		}
	}

	plan := rollupBackfillPlan{checkpointPaths: checkpointPaths}
	for name := range sourceSet {
		plan.sources = append(plan.sources, name)
	}
	for name := range destinationSet {
		plan.destinations = append(plan.destinations, name)
	}
	sort.Strings(plan.sources)
	sort.Strings(plan.destinations)
	return plan, nil
}

func (e *Engine) discoverRollupSourceCandidates() []string {
	seen := make(map[string]struct{})

	e.mu.RLock()
	for name, rt := range e.runtimes {
		if name == internalStatsDatabase || rt == nil {
			continue
		}
		seen[name] = struct{}{}
	}
	e.mu.RUnlock()

	entries, err := os.ReadDir(e.RootDataDir)
	if err == nil {
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			name := strings.TrimSpace(ent.Name())
			if name == "" || name == internalStatsDatabase {
				continue
			}
			seen[name] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) rollupInfoForDB(database string) (DBInfo, bool, error) {
	e.mu.RLock()
	rt := e.runtimes[database]
	e.mu.RUnlock()
	if rt != nil {
		return rt.info, true, nil
	}

	root := filepath.Join(e.RootDataDir, database)
	info, exists, err := loadExistingDBInfo(root, e.dbDefaults)
	if err != nil {
		return DBInfo{}, false, err
	}
	if !exists {
		return DBInfo{}, false, nil
	}
	return info, true, nil
}

func (e *Engine) resetRollupDestination(database string) ([]string, []string, []string, error) {
	var db *Database

	e.mu.Lock()
	db = e.dbs[database]
	delete(e.dbs, database)
	delete(e.runtimes, database)
	e.mu.Unlock()

	if db != nil {
		if err := db.Close(); err != nil {
			return nil, nil, nil, fmt.Errorf("close rollup destination %q: %w", database, err)
		}
	}

	root := filepath.Join(e.RootDataDir, database)
	dataFiles, err := filepath.Glob(filepath.Join(root, "data-*.dat"))
	if err != nil {
		return nil, nil, nil, err
	}
	walFiles, err := filepath.Glob(filepath.Join(root, "*.wal"))
	if err != nil {
		return nil, nil, nil, err
	}

	removedData := make([]string, 0, len(dataFiles))
	for _, path := range dataFiles {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, nil, nil, err
		}
		removedData = append(removedData, path)
	}

	removedWAL := make([]string, 0, len(walFiles))
	for _, path := range walFiles {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, nil, nil, err
		}
		removedWAL = append(removedWAL, path)
	}

	removedCatalog := make([]string, 0, 1)
	catalogPath := filepath.Join(root, "catalog.json")
	if err := os.Remove(catalogPath); err == nil {
		removedCatalog = append(removedCatalog, catalogPath)
	} else if !os.IsNotExist(err) {
		return nil, nil, nil, err
	}

	return removedData, removedWAL, removedCatalog, nil
}

// triggerRollups is called after a file is closed/flushed.
func (e *Engine) triggerRollups(sourceDBName string) {
	e.triggerRollupsForSource(sourceDBName, true)
}

func (e *Engine) buildRollupJobPeriod(rollupDB *Database, sourceDB *Database, job DBManifestRollupJob, periodStart, periodEnd Timestamp) error {
	points := make([]float32, 0, 256)

	err := e.queryRange(sourceDB.Name, job.SourceMetric, periodStart, periodEnd-1, 1, func(s Sample) error {
		var val float32
		if s.ValueType == Int32Sample {
			val = float32(s.Int32)
		} else {
			val = s.Float32
		}
		points = append(points, val)
		return nil
	}, true)
	if err != nil || len(points) == 0 {
		return err
	}

	for _, agg := range job.Aggregates {
		aggFn, ok := getAggregator(agg)
		if !ok {
			continue
		}
		value, err := aggFn.Compute(periodStart, periodEnd, points)
		if err != nil {
			return err
		}
		if err := e.insertRollupSample(rollupDB.Name, rollupDestinationMetricName(job, aggFn.Name()), periodStart, value); err != nil {
			return err
		}
	}
	return nil
}

func rollupDestinationMetricName(job DBManifestRollupJob, aggregate string) string {
	prefix := strings.TrimSpace(job.DestinationMetricPrefix)
	if prefix == "" {
		prefix = strings.TrimSpace(job.SourceMetric)
	}
	aggregate = strings.TrimSpace(aggregate)
	if prefix == "" || aggregate == "" {
		return ""
	}
	return prefix + "." + aggregate
}

func (e *Engine) insertRollupSample(dbName, metricName string, ts Timestamp, val float32) error {
	return e.addParsedSample(dbName, metricName, ts, Float32Sample, 0, val, false, false, true)
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

func latestClosedMetricTimestamp(sourceDB *Database, sourceRT *dbRuntime, metric string) (Timestamp, bool) {
	entry, ok := sourceDB.catalog.GetMetricEntry(metric)
	if !ok {
		return 0, false
	}
	if entry.LastValid {
		if isOpenRollupPartition(sourceDB, sourceRT, entry.LastTS) {
			return 0, false
		}
		return entry.LastTS, true
	}

	fromTS, toTS, ok := databaseClosedTimeBounds(sourceDB, sourceRT)
	if !ok {
		return 0, false
	}

	lastTS := Timestamp(0)
	count := 0
	lastPath := ""
	for d := dayStartUTC(fromTS); !d.After(dayStartUTC(toTS)); d = d.Add(24 * time.Hour) {
		part := partitionKey(sourceRT, Timestamp(d.UnixNano()))
		if _, open := sourceRT.openDays[part]; open {
			continue
		}
		path := filepath.Join(sourceDB.RootDataDir, "data-"+part+".dat")
		if path != lastPath {
			lastPath = path
			if err := collectMetricFromFile(sourceDB.Name, metric, entry, path, fromTS, toTS, 1, &count, func(s Sample) error {
				if s.TS > lastTS {
					lastTS = s.TS
				}
				return nil
			}); err != nil && !os.IsNotExist(err) {
				return 0, false
			}
		}
	}

	if lastTS == 0 {
		return 0, false
	}
	return lastTS, true
}

func initialRollupStart(sourceDB *Database, sourceRT *dbRuntime, interval time.Duration) Timestamp {
	earliest, _, ok := databaseClosedTimeBounds(sourceDB, sourceRT)
	if !ok {
		return 0
	}
	return floorTimestamp(earliest, interval)
}

func databaseClosedTimeBounds(sourceDB *Database, sourceRT *dbRuntime) (Timestamp, Timestamp, bool) {
	earliest := Timestamp(0)
	latest := Timestamp(0)

	entries, err := os.ReadDir(sourceDB.RootDataDir)
	if err == nil {
		for _, ent := range entries {
			name := ent.Name()
			if ent.IsDir() || !strings.HasPrefix(name, "data-") || !strings.HasSuffix(name, ".dat") {
				continue
			}
			part := strings.TrimSuffix(strings.TrimPrefix(name, "data-"), ".dat")
			if _, open := sourceRT.openDays[part]; open {
				continue
			}
			stats, err := WalkDataFileHeaders(filepath.Join(sourceDB.RootDataDir, name), nil)
			if err != nil || stats.Frames == 0 {
				continue
			}
			if earliest == 0 || stats.MinStart < earliest {
				earliest = stats.MinStart
			}
			if latest == 0 || stats.MaxEnd > latest {
				latest = stats.MaxEnd
			}
		}
	}

	if earliest == 0 || latest == 0 {
		return 0, 0, false
	}
	return earliest, latest, true
}

func isOpenRollupPartition(sourceDB *Database, sourceRT *dbRuntime, ts Timestamp) bool {
	if sourceDB == nil || sourceRT == nil || ts == 0 {
		return false
	}
	_, open := sourceRT.openDays[partitionKey(sourceRT, ts)]
	return open
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
	path := filepath.Join(rootDir, fileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s,%d\n", jobID, completed); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() <= rollupCheckpointCompactBytes {
		return nil
	}
	return compactRollupCheckpointFile(rootDir, fileName)
}

func compactRollupCheckpointFile(rootDir, fileName string) error {
	checkpoints, err := loadRollupCheckpoints(rootDir, fileName)
	if err != nil {
		return err
	}
	jobIDs := make([]string, 0, len(checkpoints))
	for jobID := range checkpoints {
		jobIDs = append(jobIDs, jobID)
	}
	sort.Strings(jobIDs)

	path := filepath.Join(rootDir, fileName)
	tmpPath := path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		if _, err := fmt.Fprintf(f, "%s,%d\n", jobID, checkpoints[jobID]); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
