package engine

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
)

type DBInfo struct {
	Grace          string            `json:"grace" toml:"grace"`
	RetentionDays  int               `json:"retention_days" toml:"retention_days"`
	MaxActiveDays  int               `json:"max_active_days" toml:"max_active_days"`
	Partition      string            `json:"partition" toml:"partition"`
	WALEnabled     bool              `json:"wal_enabled" toml:"wal_enabled"`
	WALSkipBefore  string            `json:"wal_skip_before" toml:"wal_skip_before"`
	PageMaxRecords int               `json:"page_max_records" toml:"page_max_records"`
	PageMaxBytes   int               `json:"page_max_bytes" toml:"page_max_bytes"`
	PageMaxAge     string            `json:"page_max_age" toml:"page_max_age"`
	Rollups        DBManifestRollups `json:"rollups" toml:"rollups"`
}

type dbRuntime struct {
	info          DBInfo
	walSkipBefore time.Duration
	pageMaxAge    time.Duration
	openDays      map[string]*Page
	sealedDays    map[string]struct{}
}

type DBManifestTOML struct {
	Retention DBManifestRetention `toml:"retention"`
	WAL       DBManifestWAL       `toml:"wal"`
	Page      DBManifestPage      `toml:"page"`
	Rollups   DBManifestRollups   `toml:"rollups"`
}

type DBManifestRetention struct {
	Grace         string `toml:"grace"`
	RetentionDays int    `toml:"retention_days"`
	MaxActiveDays int    `toml:"max_active_days"`
	Partition     string `toml:"partition"`
}

type DBManifestWAL struct {
	Enabled    bool   `toml:"enabled"`
	SkipBefore string `toml:"skip_before"`
}

type DBManifestPage struct {
	MaxRecords int    `toml:"max_records"`
	MaxBytes   int    `toml:"max_bytes"`
	MaxAge     string `toml:"max_age"`
}

type DBManifestRollups struct {
	Enabled        bool                  `toml:"enabled"`
	CheckpointFile string                `toml:"checkpoint_file"`
	DefaultGrace   string                `toml:"default_grace"`
	Jobs           []DBManifestRollupJob `toml:"jobs"`
}

type DBManifestRollupJob struct {
	ID                      string   `toml:"id"`
	SourceMetric            string   `toml:"source_metric"`
	Interval                string   `toml:"interval"`
	Aggregates              []string `toml:"aggregates"`
	DestinationDB           string   `toml:"destination_db"`
	DestinationMetricPrefix string   `toml:"destination_metric_prefix"`
	Grace                   string   `toml:"grace"`
}

type EngineConfig struct {
	Engine           EngineConfigEngine           `toml:"engine"`
	WAL              EngineConfigWAL              `toml:"wal"`
	Durability       EngineConfigDurability       `toml:"durability"`
	Stats            EngineConfigStats            `toml:"stats"`
	Defaults         EngineConfigDefaults         `toml:"defaults"`
	ManifestDefaults EngineConfigManifestDefaults `toml:"manifest_defaults"`
}

type EngineConfigEngine struct {
	Listen string `toml:"listen"`
}

type EngineConfigWAL struct {
	MaxSegmentSize int64  `toml:"max_segment_size"`
	FsyncPolicy    string `toml:"fsync_policy"`
}

type EngineConfigDurability struct {
	Profile string `toml:"profile"`
}

type EngineConfigStats struct {
	Enabled  bool   `toml:"enabled"`
	Interval string `toml:"interval"`
}

type EngineConfigDefaults struct {
	Databases []string `toml:"databases"`
}

type EngineConfigManifestDefaults struct {
	Retention DBManifestRetention `toml:"retention"`
	WAL       DBManifestWAL       `toml:"wal"`
	Page      DBManifestPage      `toml:"page"`
	Rollups   DBManifestRollups   `toml:"rollups"`
}

const (
	internalStatsDatabase     = "internal"
	internalStatsMetricPrefix = "nanotdb"
	engineConfigFileName      = "engine.toml"
	manifestFileName          = "manifest.toml"
)

const (
	DurabilityProfileStrict     = "strict"
	DurabilityProfileBalanced   = "balanced"
	DurabilityProfileThroughput = "throughput"
)

//go:embed default_engine.toml
var defaultEngineConfigTOML string

// Engine is the top-level coordinator for all databases in a root data directory.
// It is safe for concurrent use. Open with OpenEngine; always call Close when done.
type Engine struct {
	RootDataDir    string
	WALMaxSegSize  int64
	WALFsyncPolicy string
	Durability     string
	SyncDataFile   bool
	SyncCatalog    bool
	StatsEnabled   bool
	StatsInterval  time.Duration
	dbDefaults     DBInfo

	mu             sync.RWMutex
	dbs            map[string]*Database
	runtimes       map[string]*dbRuntime
	stats          engineStatStore
	statsLastFlush time.Time
	statsLastMu    sync.Mutex
	rollupAuto     atomic.Bool
}

// Sample is one decoded data point returned by QueryRange or QueryLast.
type Sample struct {
	Database  string
	Metric    string
	TS        Timestamp
	ValueType byte
	Int32     int32
	Float32   float32
}

var pageWriteBufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 32768))
	},
}

func defaultDBInfo() DBInfo {
	return DBInfo{
		Grace:          "5m",
		RetentionDays:  30,
		MaxActiveDays:  2,
		Partition:      "day",
		WALEnabled:     true,
		WALSkipBefore:  "1h",
		PageMaxRecords: PageMaxRecords,
		PageMaxBytes:   PageMaxBytes,
		PageMaxAge:     PageMaxAge.String(),
		Rollups: DBManifestRollups{
			Enabled:        false,
			CheckpointFile: defaultRollupCheckpointFile,
			DefaultGrace:   "",
			Jobs:           nil,
		},
	}
}

func defaultEngineConfig(walMaxSegSize int64) EngineConfig {
	dbDef := defaultDBInfo()
	if walMaxSegSize <= 0 {
		walMaxSegSize = 64 * 1024 * 1024
	}
	return EngineConfig{
		Engine:     EngineConfigEngine{Listen: ":8428"},
		WAL:        EngineConfigWAL{MaxSegmentSize: walMaxSegSize, FsyncPolicy: WALFsyncPolicySegment},
		Durability: EngineConfigDurability{Profile: DurabilityProfileStrict},
		Stats:      EngineConfigStats{Enabled: true, Interval: "30s"},
		Defaults: EngineConfigDefaults{
			Databases: []string{},
		},
		ManifestDefaults: engineConfigManifestDefaultsFromInfo(dbDef),
	}
}

func engineConfigManifestDefaultsFromInfo(info DBInfo) EngineConfigManifestDefaults {
	return EngineConfigManifestDefaults{
		Retention: DBManifestRetention{
			Grace:         info.Grace,
			RetentionDays: info.RetentionDays,
			MaxActiveDays: info.MaxActiveDays,
			Partition:     info.Partition,
		},
		WAL: DBManifestWAL{
			Enabled:    info.WALEnabled,
			SkipBefore: info.WALSkipBefore,
		},
		Page: DBManifestPage{
			MaxRecords: info.PageMaxRecords,
			MaxBytes:   info.PageMaxBytes,
			MaxAge:     info.PageMaxAge,
		},
		Rollups: info.Rollups,
	}
}

func dbInfoDefaultsFromEngineConfig(cfg EngineConfigManifestDefaults) (DBInfo, error) {
	info := dbInfoFromManifest(DBManifestTOML{
		Retention: cfg.Retention,
		WAL:       cfg.WAL,
		Page:      cfg.Page,
		Rollups:   cfg.Rollups,
	})
	return normalizeDBInfo(info, defaultDBInfo())
}

func normalizeEngineConfig(cfg EngineConfig, fallbackWalMaxSegSize int64) (EngineConfig, time.Duration, DBInfo, error) {
	def := defaultEngineConfig(fallbackWalMaxSegSize)
	if cfg.WAL.MaxSegmentSize <= 0 {
		cfg.WAL.MaxSegmentSize = def.WAL.MaxSegmentSize
	}
	if strings.TrimSpace(cfg.Engine.Listen) == "" {
		cfg.Engine.Listen = def.Engine.Listen
	}
	if strings.TrimSpace(cfg.WAL.FsyncPolicy) == "" {
		cfg.WAL.FsyncPolicy = def.WAL.FsyncPolicy
	}
	if cfg.WAL.FsyncPolicy != WALFsyncPolicySegment && cfg.WAL.FsyncPolicy != WALFsyncPolicyAlways {
		return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("invalid wal.fsync_policy: %q", cfg.WAL.FsyncPolicy)
	}
	if strings.TrimSpace(cfg.Durability.Profile) == "" {
		cfg.Durability.Profile = def.Durability.Profile
	}
	switch cfg.Durability.Profile {
	case DurabilityProfileStrict, DurabilityProfileBalanced, DurabilityProfileThroughput:
	default:
		return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("invalid durability.profile: %q", cfg.Durability.Profile)
	}
	if strings.TrimSpace(cfg.Stats.Interval) == "" {
		cfg.Stats.Interval = def.Stats.Interval
	}
	statsInterval, err := time.ParseDuration(cfg.Stats.Interval)
	if err != nil {
		return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("invalid stats_interval: %w", err)
	}
	if statsInterval < 0 {
		return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("invalid stats_interval: must be >= 0")
	}
	if cfg.Defaults.Databases == nil {
		cfg.Defaults.Databases = []string{}
	}
	dbDefaults, err := dbInfoDefaultsFromEngineConfig(cfg.ManifestDefaults)
	if err != nil {
		return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("invalid manifest_defaults: %w", err)
	}
	return cfg, statsInterval, dbDefaults, nil
}

func loadOrCreateEngineConfig(rootDataDir string, fallbackWalMaxSegSize int64) (EngineConfig, time.Duration, DBInfo, error) {
	path := filepath.Join(rootDataDir, engineConfigFileName)
	if raw, err := os.ReadFile(path); err == nil {
		cfg := defaultEngineConfig(fallbackWalMaxSegSize)
		if _, err := toml.Decode(string(raw), &cfg); err != nil {
			return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("parse %s: %w", path, err)
		}
		cfg, interval, dbDefaults, err := normalizeEngineConfig(cfg, fallbackWalMaxSegSize)
		if err != nil {
			return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("invalid %s: %w", path, err)
		}
		return cfg, interval, dbDefaults, nil
	} else if !os.IsNotExist(err) {
		return EngineConfig{}, 0, DBInfo{}, err
	}

	cfg := defaultEngineConfig(fallbackWalMaxSegSize)
	if _, err := toml.Decode(defaultEngineConfigTOML, &cfg); err != nil {
		return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("parse embedded default engine config: %w", err)
	}
	cfg, interval, dbDefaults, err := normalizeEngineConfig(cfg, fallbackWalMaxSegSize)
	if err != nil {
		return EngineConfig{}, 0, DBInfo{}, err
	}
	if err := writeEngineConfigTOML(path, cfg); err != nil {
		return EngineConfig{}, 0, DBInfo{}, err
	}
	return cfg, interval, dbDefaults, nil
}

func writeEngineConfigTOML(path string, cfg EngineConfig) error {
	buf := bytes.NewBuffer(nil)
	if err := toml.NewEncoder(buf).Encode(cfg); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}

// OpenEngine opens or creates the engine rooted at rootDataDir.
// If engine.toml does not exist it is written from the embedded defaults.
// walMaxSegSize sets the per-database WAL segment size; pass 0 for the 64 MiB default.
func OpenEngine(rootDataDir string, walMaxSegSize int64) (*Engine, error) {
	if err := os.MkdirAll(rootDataDir, 0755); err != nil {
		return nil, err
	}
	cfg, statsInterval, dbDefaults, err := loadOrCreateEngineConfig(rootDataDir, walMaxSegSize)
	if err != nil {
		return nil, err
	}
	syncData, syncCatalog := durabilitySyncPolicy(cfg.Durability.Profile)
	e := &Engine{
		RootDataDir:    rootDataDir,
		WALMaxSegSize:  cfg.WAL.MaxSegmentSize,
		WALFsyncPolicy: cfg.WAL.FsyncPolicy,
		Durability:     cfg.Durability.Profile,
		SyncDataFile:   syncData,
		SyncCatalog:    syncCatalog,
		StatsEnabled:   cfg.Stats.Enabled,
		StatsInterval:  statsInterval,
		dbDefaults:     dbDefaults,
		dbs:            make(map[string]*Database),
		runtimes:       make(map[string]*dbRuntime),
		stats:          newEngineStatStore(),
	}
	e.rollupAuto.Store(true)
	for _, dbName := range cfg.Defaults.Databases {
		dbName = strings.TrimSpace(dbName)
		if dbName == "" || dbName == internalStatsDatabase {
			continue
		}
		if _, _, err := e.getOrCreateDB(dbName); err != nil {
			return nil, fmt.Errorf("create default database %q: %w", dbName, err)
		}
	}
	return e, nil
}

// SetAutoRollupTrigger controls whether ingest-time flushes automatically
// trigger rollup computation for the source database.
func (e *Engine) SetAutoRollupTrigger(enabled bool) {
	e.rollupAuto.Store(enabled)
}

func durabilitySyncPolicy(profile string) (syncDataFile bool, syncCatalog bool) {
	switch profile {
	case DurabilityProfileThroughput:
		return false, false
	case DurabilityProfileBalanced:
		return true, false
	case DurabilityProfileStrict:
		fallthrough
	default:
		return true, true
	}
}

// Close flushes all open day-pages, resets WAL files, emits a final stats snapshot,
// and closes every open database. Always call Close before the process exits.
func (e *Engine) Close() error {
	e.mu.Lock()
	for name, db := range e.dbs {
		if name == internalStatsDatabase {
			continue
		}
		rt := e.runtimes[name]
		if rt == nil {
			continue
		}
		for day, p := range rt.openDays {
			if p != nil {
				if err := e.writePageToDailyFile(db, name, day, p); err != nil {
					e.mu.Unlock()
					return err
				}
				rt.openDays[day] = nil
			}
		}
		if err := maybeResetWAL(db, rt); err != nil {
			e.mu.Unlock()
			return err
		}
		e.captureWALStats(db, name)
	}
	e.mu.Unlock()

	// Write final stats snapshot directly to internal DB page (no AddLine, no lock needed).
	e.flushStatsToInternal(Timestamp(time.Now().UnixNano()))

	e.mu.Lock()
	defer e.mu.Unlock()
	for name, db := range e.dbs {
		if db.catalog != nil && db.catalog.IsDirty() {
			if err := db.catalog.WriteCatalog(); err != nil {
				return fmt.Errorf("write catalog for database %q: %w", name, err)
			}
		}
		if err := db.Close(); err != nil {
			return fmt.Errorf("close database %q: %w", name, err)
		}
	}
	return nil
}

// GetAllDatabaseNames returns all database names managed by this engine.
func (e *Engine) GetAllDatabaseNames() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make([]string, 0, len(e.dbs))
	for name := range e.dbs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ListMetrics returns all known metrics for a database in stable name order.
func (e *Engine) ListMetrics(database string) ([]MetricInfo, error) {
	database = strings.TrimSpace(database)
	if database == "" {
		return nil, fmt.Errorf("database cannot be empty")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	db, ok := e.dbs[database]
	if !ok {
		return nil, fmt.Errorf("database not found: %s", database)
	}
	return db.catalog.ListMetrics(), nil
}

// AddLine ingests one sample in line-protocol format: "DB/metric value [ts]"
// where value is an integer or float literal and ts is optional.
// ts can be Unix nanoseconds or a human-readable timestamp accepted by ParseTimestamp.
// AddLine is safe for concurrent use.
func (e *Engine) AddLine(line string) error {
	dbName, metric, ts, vType, i32, f32, err := parseLineProtocol(line)
	if err != nil {
		return err
	}
	if vType == Int32Sample {
		return e.AddSample(dbName, metric, ts, i32)
	}
	return e.AddSample(dbName, metric, ts, f32)
}

// AddSample ingests one typed sample directly.
// This is the canonical ingest API used by all write paths.
func (e *Engine) AddSample(database, metric string, ts Timestamp, value any) error {
	if strings.TrimSpace(database) == "" {
		return fmt.Errorf("database cannot be empty")
	}
	if strings.TrimSpace(metric) == "" {
		return fmt.Errorf("metric cannot be empty")
	}

	switch v := value.(type) {
	case int32:
		return e.addParsedSample(database, metric, ts, Int32Sample, v, 0, true, false)
	case float32:
		return e.addParsedSample(database, metric, ts, Float32Sample, 0, v, true, false)
	default:
		return fmt.Errorf("unsupported sample type")
	}
}

func (e *Engine) addParsedSample(dbName, metric string, ts Timestamp, vType byte, i32 int32, f32 float32, triggerRollups bool, forceWAL bool) error {
	db, rt, err := e.getOrCreateDB(dbName)
	if err != nil {
		return err
	}

	entry, exists := db.catalog.GetMetricEntry(metric)
	if exists && entry.LastValid && ts < entry.LastTS {
		return fmt.Errorf("stale sample rejected for %s/%s: ts=%d < last=%d", dbName, metric, ts, entry.LastTS)
	}

	day := partitionKey(rt, ts)
	if err := e.ensureDayOpen(db, rt, dbName, day, ts); err != nil {
		return err
	}

	useWAL := forceWAL || shouldWriteWAL(rt, ts, time.Now())

	var metricID MetricID
	var raw [4]byte
	var walSegment uint16
	if vType == Int32Sample {
		metricID, err = GetMetricID[int32](db.catalog, metric)
		if err != nil {
			return err
		}
		if useWAL {
			if !exists {
				walSegment, err = AppendSampleWithMetricName[int32](db.wal, metricID, metric, ts, i32)
			} else {
				walSegment, err = AppendSample[int32](db.wal, metricID, ts, i32)
			}
			if err != nil {
				return err
			}
		}
		binary.LittleEndian.PutUint32(raw[:], uint32(i32))
	} else {
		metricID, err = GetMetricID[float32](db.catalog, metric)
		if err != nil {
			return err
		}
		if useWAL {
			if !exists {
				walSegment, err = AppendSampleWithMetricName[float32](db.wal, metricID, metric, ts, f32)
			} else {
				walSegment, err = AppendSample[float32](db.wal, metricID, ts, f32)
			}
			if err != nil {
				return err
			}
		}
		binary.LittleEndian.PutUint32(raw[:], math.Float32bits(f32))
	}

	if err := e.addToOpenDay(db, rt, day, ts, metricID, raw[:], walSegment); err != nil {
		// Rollup writes can revisit older period starts for a day after another
		// rollup job already appended newer timestamps into that open day page.
		// In that case, flush the existing page and retry once.
		if err == ErrOutOfOrderTimestamp && forceWAL {
			if existing := rt.openDays[day]; existing != nil {
				if werr := e.writePageToDailyFile(db, dbName, day, existing); werr != nil {
					return werr
				}
				rt.openDays[day] = nil
			}
			if rerr := e.addToOpenDay(db, rt, day, ts, metricID, raw[:], walSegment); rerr != nil {
				return rerr
			}
		} else {
			return err
		}
	}
	p := rt.openDays[day]

	if useWAL && walSegment != 0 {
		e.stats.incr(dbName+"/wal/append_count", 1)
	}

	if p.IsFull() {
		if err := e.writePageToDailyFile(db, dbName, day, p); err != nil {
			return err
		}
		if triggerRollups && e.rollupAuto.Load() {
			e.triggerRollups(dbName)
		}
		rt.openDays[day] = nil
		if err := maybeResetWAL(db, rt); err != nil {
			return err
		}
		e.captureWALStats(db, dbName)
	}
	e.maybeFlushStats(dbName)
	return nil
}

func (e *Engine) addFloatSample(dbName, metric string, ts Timestamp, val float32, triggerRollups bool, forceWAL bool) error {
	return e.addParsedSample(dbName, metric, ts, Float32Sample, 0, val, triggerRollups, forceWAL)
}

// addToOpenDay appends a sample to the active day page and updates catalog last-value.
// WAL append must be performed by the caller before this method is invoked.
func (e *Engine) addToOpenDay(db *Database, rt *dbRuntime, day string, ts Timestamp, metricID MetricID, raw []byte, walSegment uint16) error {
	if rt.openDays[day] == nil {
		rt.openDays[day] = NewPageWithLimits(ts, rt.info.PageMaxRecords, rt.info.PageMaxBytes, rt.pageMaxAge)
	}
	p := rt.openDays[day]
	if len(p.Times) > 0 && ts < p.Times[len(p.Times)-1] {
		return ErrOutOfOrderTimestamp
	}
	if walSegment != 0 {
		p.SetWalSegmentID(walSegment)
	}
	if err := p.AddSample(metricID, ts, raw); err != nil {
		return err
	}
	if err := db.catalog.UpdateLastByMetricID(metricID, ts, raw); err != nil {
		return err
	}
	return nil
}

// ImportFile imports LP lines in the format: DB/metric value [ts].
// ts can be Unix nanoseconds or a human-readable value accepted by ParseTimestamp.
func (e *Engine) ImportFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := e.AddLine(line); err != nil {
			return fmt.Errorf("import %s line %d: %w", path, lineNo, err)
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	return nil
}

// ExportFile exports one database to a LP file using: DB/metric value ts.
// Exported timestamps use FormatTimestamp (UTC, YYYY-MM-DD HH:MM:SS.nnnnnnnnn).
func (e *Engine) ExportFile(database, outPath string) error {
	outFile, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	return e.ExportToWriter(database, outFile)
}

// ExportToWriter exports one database to an arbitrary writer using line protocol.
// Timestamps are written with FormatTimestamp (UTC, YYYY-MM-DD HH:MM:SS.nnnnnnnnn).
func (e *Engine) ExportToWriter(database string, out io.Writer) error {
	db, rt, err := e.getOrCreateDB(database)
	if err != nil {
		return err
	}

	w := bufio.NewWriterSize(out, 64*1024)
	wroteAny := false

	entries, err := os.ReadDir(db.RootDataDir)
	if err != nil {
		return err
	}
	dayFiles := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, "data-") && strings.HasSuffix(name, ".dat") {
			dayFiles = append(dayFiles, filepath.Join(db.RootDataDir, name))
		}
	}
	sort.Strings(dayFiles)

	for _, path := range dayFiles {
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := exportLinesFromBlob(database, db, blob, w, &wroteAny); err != nil {
			return fmt.Errorf("export from %s: %w", path, err)
		}
	}

	openDays := make([]string, 0, len(rt.openDays))
	for day, p := range rt.openDays {
		if p != nil {
			openDays = append(openDays, day)
		}
	}
	sort.Strings(openDays)
	for _, day := range openDays {
		if err := exportLinesFromPage(database, db, rt.openDays[day], w, &wroteAny); err != nil {
			return err
		}
	}
	if wroteAny {
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	return w.Flush()
}

// QueryLast returns the most recently written sample for a metric.
// Returns (sample, true, nil) if a sample exists, (zero, false, nil) if not.
func (e *Engine) QueryLast(database, metric string) (Sample, bool, error) {
	db, _, err := e.getOrCreateDB(database)
	if err != nil {
		return Sample{}, false, err
	}

	entry, ok := db.catalog.GetMetricEntry(metric)
	if !ok || !entry.LastValid {
		return Sample{}, false, nil
	}

	s := Sample{
		Database:  database,
		Metric:    metric,
		TS:        entry.LastTS,
		ValueType: entry.ValueType,
	}
	if entry.ValueType == Int32Sample {
		s.Int32 = int32(binary.LittleEndian.Uint32(entry.LastRaw[:]))
	} else {
		s.Float32 = math.Float32frombits(binary.LittleEndian.Uint32(entry.LastRaw[:]))
	}
	return s, true, nil
}

// DBStats returns a snapshot of engine-level stats for the given database.
// Values for data flushes come from the engine stat store; WAL stats are read
// directly from the WAL so they are always current.
func (e *Engine) DBStats(database string) (DBStats, bool) {
	e.mu.RLock()
	db := e.dbs[database]
	e.mu.RUnlock()
	if db == nil {
		return DBStats{}, false
	}
	pfx := database + "/"
	snap := e.stats.snapshot()
	var ds DBStats
	ds.DataFile.FlushCount = int64(snap[pfx+"data/flush_count"])
	ds.DataFile.TotalFlushBytes = int64(snap[pfx+"data/flush_bytes"])
	ds.DataFile.TotalFlushRecords = int64(snap[pfx+"data/flush_records"])
	ds.DataFile.TotalFlushCompressed = int64(snap[pfx+"data/flush_compressed_bytes"])
	ds.DataFile.MinFlushBytes = int64(snap[pfx+"data/min_flush_bytes"])
	ds.DataFile.MaxFlushBytes = int64(snap[pfx+"data/max_flush_bytes"])
	ds.DataFile.MinFlushRecords = int64(snap[pfx+"data/min_flush_records"])
	ds.DataFile.MaxFlushRecords = int64(snap[pfx+"data/max_flush_records"])
	ds.DataFile.MinFlushCompressed = int64(snap[pfx+"data/min_flush_compressed_bytes"])
	ds.DataFile.MaxFlushCompressed = int64(snap[pfx+"data/max_flush_compressed_bytes"])
	ds.DataFile.FlushDurationTotal = time.Duration(int64(snap[pfx+"data/flush_duration_total_ns"]))
	ds.DataFile.MinFlushDuration = time.Duration(int64(snap[pfx+"data/min_flush_duration_ns"]))
	ds.DataFile.MaxFlushDuration = time.Duration(int64(snap[pfx+"data/max_flush_duration_ns"]))
	ds.DataFile.SyncCount = int64(snap[pfx+"data/fsync_count"])
	ds.DataFile.SyncDurationTotal = time.Duration(int64(snap[pfx+"data/fsync_duration_total_ns"]))
	ds.DataFile.MinSyncDuration = time.Duration(int64(snap[pfx+"data/min_fsync_duration_ns"]))
	ds.DataFile.MaxSyncDuration = time.Duration(int64(snap[pfx+"data/max_fsync_duration_ns"]))
	if db.wal != nil {
		ds.WAL = db.wal.Stats()
	}
	return ds, true
}

// SampleCallback is invoked for each sample in a range query.
type SampleCallback func(Sample) error

// QueryRange scans samples for a metric within a time range.
// Stride controls downsampling: stride=1 returns every sample, stride=N returns every Nth.
// Each matching sample is passed to the callback; callback errors terminate early.
func (e *Engine) QueryRange(database, metric string, fromTS, toTS Timestamp, stride int, fn SampleCallback) error {
	if toTS < fromTS {
		return fmt.Errorf("invalid range: toTS < fromTS")
	}
	if stride < 1 {
		stride = 1
	}

	db, rt, err := e.getOrCreateDB(database)
	if err != nil {
		return err
	}

	entry, ok := db.catalog.GetMetricEntry(metric)
	if !ok {
		return nil
	}

	count := 0
	lastPath := ""
	for d := dayStartUTC(fromTS); !d.After(dayStartUTC(toTS)); d = d.Add(24 * time.Hour) {
		part := partitionKey(rt, Timestamp(d.UnixNano()))
		path := filepath.Join(db.RootDataDir, "data-"+part+".dat")
		if path == lastPath {
			continue
		}
		lastPath = path
		if err := collectMetricFromFile(database, metric, entry, path, fromTS, toTS, stride, &count, fn); err == nil {
			// persisted frames processed
		} else if os.IsNotExist(err) {
			// no persisted file for this partition
		} else {
			return fmt.Errorf("read %s: %w", path, err)
		}

		if p := rt.openDays[part]; p != nil {
			if err := collectMetricFromPage(database, metric, entry, p, fromTS, toTS, stride, &count, fn); err != nil {
				return err
			}
		}
	}

	return nil
}

func collectMetricFromFile(database, metric string, entry MetricEntry, path string, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	for {
		var header [HeaderSize]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			if err == io.ErrUnexpectedEOF {
				return fmt.Errorf("truncated frame header")
			}
			return err
		}

		start := Timestamp(binary.LittleEndian.Uint64(header[0:8]))
		end := Timestamp(binary.LittleEndian.Uint64(header[8:16]))

		compressedLen, err := binary.ReadUvarint(r)
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("truncated frame length")
			}
			return err
		}

		if end < fromTS || start > toTS {
			if _, err := io.CopyN(io.Discard, r, int64(compressedLen)+4); err != nil {
				return fmt.Errorf("truncated frame payload")
			}
			continue
		}

		compressed := make([]byte, compressedLen)
		if _, err := io.ReadFull(r, compressed); err != nil {
			return fmt.Errorf("truncated compressed payload")
		}

		var crcBytes [4]byte
		if _, err := io.ReadFull(r, crcBytes[:]); err != nil {
			return fmt.Errorf("truncated frame checksum")
		}

		var frame bytes.Buffer
		frame.Grow(HeaderSize + binary.MaxVarintLen64 + int(compressedLen) + 4)
		if _, err := frame.Write(header[:]); err != nil {
			return err
		}
		var varintBuf [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(varintBuf[:], compressedLen)
		if _, err := frame.Write(varintBuf[:n]); err != nil {
			return err
		}
		if _, err := frame.Write(compressed); err != nil {
			return err
		}
		if _, err := frame.Write(crcBytes[:]); err != nil {
			return err
		}

		var p Page
		if err := p.DecodeFrom(bytes.NewReader(frame.Bytes())); err != nil {
			return fmt.Errorf("decode page: %w", err)
		}
		if err := collectMetricFromPage(database, metric, entry, &p, fromTS, toTS, stride, count, fn); err != nil {
			return err
		}
	}
}

func collectMetricFromBlob(database, metric string, entry MetricEntry, blob []byte, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	for pos := 0; pos < len(blob); {
		if pos+HeaderSize > len(blob) {
			return fmt.Errorf("truncated frame header at offset %d", pos)
		}
		start := Timestamp(binary.LittleEndian.Uint64(blob[pos : pos+8]))
		end := Timestamp(binary.LittleEndian.Uint64(blob[pos+8 : pos+16]))

		compressedLen, n := binary.Uvarint(blob[pos+HeaderSize:])
		if n <= 0 {
			return fmt.Errorf("invalid compressed length varint at offset %d", pos+HeaderSize)
		}

		frameSize := HeaderSize + n + int(compressedLen) + 4
		if pos+frameSize > len(blob) {
			return fmt.Errorf("truncated frame at offset %d", pos)
		}

		if end < fromTS || start > toTS {
			pos += frameSize
			continue
		}

		reader := bytes.NewReader(blob[pos : pos+frameSize])
		startLen := reader.Len()
		var p Page
		if err := p.DecodeFrom(reader); err != nil {
			return fmt.Errorf("decode page at offset %d: %w", pos, err)
		}
		consumed := startLen - reader.Len()
		if consumed <= 0 {
			return fmt.Errorf("invalid page decoding at offset %d", pos)
		}
		pos += frameSize
		if err := collectMetricFromPage(database, metric, entry, &p, fromTS, toTS, stride, count, fn); err != nil {
			return err
		}
	}
	return nil
}

func exportLinesFromBlob(database string, db *Database, blob []byte, w *bufio.Writer, wroteAny *bool) error {
	for pos := 0; pos < len(blob); {
		reader := bytes.NewReader(blob[pos:])
		startLen := reader.Len()
		var p Page
		if err := p.DecodeFrom(reader); err != nil {
			return fmt.Errorf("decode page at offset %d: %w", pos, err)
		}
		consumed := startLen - reader.Len()
		if consumed <= 0 {
			return fmt.Errorf("invalid page decoding at offset %d", pos)
		}
		pos += consumed

		if err := exportLinesFromPage(database, db, &p, w, wroteAny); err != nil {
			return err
		}
	}
	return nil
}

func exportLinesFromPage(database string, db *Database, p *Page, w *bufio.Writer, wroteAny *bool) error {
	if len(p.Metrics) != len(p.Times) {
		return fmt.Errorf("page corruption: metrics/times length mismatch")
	}
	values := p.Values.Bytes()
	if len(values) < len(p.Metrics)*4 {
		return fmt.Errorf("page corruption: values blob too short")
	}

	var numBuf [32]byte
	for i := 0; i < len(p.Metrics); i++ {
		off := i * 4
		raw := values[off : off+4]
		name, entry, ok := db.catalog.GetMetricByID(p.Metrics[i])
		if !ok {
			return fmt.Errorf("unknown MetricID %d", p.Metrics[i])
		}

		if *wroteAny {
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		} else {
			*wroteAny = true
		}
		if _, err := w.WriteString(database); err != nil {
			return err
		}
		if err := w.WriteByte('/'); err != nil {
			return err
		}
		if _, err := w.WriteString(name); err != nil {
			return err
		}
		if err := w.WriteByte(' '); err != nil {
			return err
		}

		if entry.ValueType == Int32Sample {
			v := int64(int32(binary.LittleEndian.Uint32(raw)))
			buf := strconv.AppendInt(numBuf[:0], v, 10)
			if _, err := w.Write(buf); err != nil {
				return err
			}
		} else {
			f := math.Float32frombits(binary.LittleEndian.Uint32(raw))
			buf := strconv.AppendFloat(numBuf[:0], float64(f), 'f', -1, 32)
			if _, err := w.Write(buf); err != nil {
				return err
			}
		}
		if err := w.WriteByte(' '); err != nil {
			return err
		}
		if _, err := w.WriteString(FormatTimestamp(p.Times[i])); err != nil {
			return err
		}
	}
	return nil
}

func collectMetricFromPage(database, metric string, entry MetricEntry, p *Page, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	if len(p.Metrics) != len(p.Times) {
		return fmt.Errorf("page corruption: metrics/times length mismatch")
	}
	if len(p.Values.Bytes()) < len(p.Metrics)*4 {
		return fmt.Errorf("page corruption: values blob too short")
	}

	values := p.Values.Bytes()
	for i := 0; i < len(p.Metrics); i++ {
		if p.Metrics[i] != entry.MetricID {
			continue
		}
		ts := p.Times[i]
		if ts < fromTS || ts > toTS {
			continue
		}
		off := i * 4
		if off+4 > len(values) {
			return fmt.Errorf("page corruption: value offset out of range")
		}
		if *count%stride != 0 {
			*count++
			continue
		}
		*count++

		raw := values[off : off+4]
		s := Sample{Database: database, Metric: metric, TS: ts, ValueType: entry.ValueType}
		if entry.ValueType == Int32Sample {
			s.Int32 = int32(binary.LittleEndian.Uint32(raw))
		} else {
			s.Float32 = math.Float32frombits(binary.LittleEndian.Uint32(raw))
		}
		if err := fn(s); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) getOrCreateDB(database string) (*Database, *dbRuntime, error) {
	e.mu.RLock()
	db, ok := e.dbs[database]
	rt := e.runtimes[database]
	e.mu.RUnlock()
	if ok && rt != nil {
		return db, rt, nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if db, ok := e.dbs[database]; ok {
		if rt := e.runtimes[database]; rt != nil {
			return db, rt, nil
		}
	}

	dbDir := filepath.Join(e.RootDataDir, database)
	fullName := filepath.Join(dbDir, database)
	db, err := NewDatabaseWithWALConfig(fullName, e.WALMaxSegSize, e.WALFsyncPolicy)
	if err != nil {
		return nil, nil, err
	}
	var replayRecords int64
	var replayBytes int64
	if db.wal != nil {
		count, bytes, err := scanWALAppendStats(db.wal.path)
		if err != nil {
			e.recordWALReplayMetrics(database, 0, 0, false)
			_ = db.Close()
			return nil, nil, fmt.Errorf("recover wal counters for database %q: %w", database, err)
		}
		db.wal.stats.AppendCount = count
		db.wal.stats.AppendBytes = bytes
		replayRecords = count
		replayBytes = bytes
	}
	info, err := loadOrCreateDBInfo(db.RootDataDir, e.dbDefaults)
	if err != nil {
		return nil, nil, err
	}
	walSkipBefore, err := time.ParseDuration(info.WALSkipBefore)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid wal_skip_before for database %q: %w", database, err)
	}
	pageMaxAge, err := time.ParseDuration(info.PageMaxAge)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid page_max_age for database %q: %w", database, err)
	}
	rt = &dbRuntime{info: info, walSkipBefore: walSkipBefore, pageMaxAge: pageMaxAge, openDays: make(map[string]*Page), sealedDays: make(map[string]struct{})}
	if err := e.replayWALIntoRuntime(db, rt, database); err != nil {
		e.recordWALReplayMetrics(database, replayRecords, replayBytes, false)
		_ = db.Close()
		return nil, nil, fmt.Errorf("replay wal for database %q: %w", database, err)
	}
	if database != internalStatsDatabase {
		e.recordWALReplayMetrics(database, replayRecords, replayBytes, true)
		e.captureWALStats(db, database)
	}
	e.dbs[database] = db
	e.runtimes[database] = rt
	return db, rt, nil
}

// captureWALStats copies cumulative WAL counters into the engine stat store.
// Call this after WAL mutations (including Reset) to snapshot the latest counters.
func (e *Engine) captureWALStats(db *Database, dbName string) {
	if db == nil || db.wal == nil {
		return
	}
	ws := db.wal.Stats()
	pfx := dbName + "/wal/"
	e.stats.set(pfx+"append_count", float64(ws.AppendCount))
	e.stats.set(pfx+"append_bytes", float64(ws.AppendBytes))
	e.stats.set(pfx+"buffer_bytes", float64(ws.BufferBytes))
	e.stats.set(pfx+"fsync_count", float64(ws.FsyncCount))
	e.stats.set(pfx+"fsync_duration_total_ns", float64(ws.FsyncDurationTotal.Nanoseconds()))
	e.stats.set(pfx+"min_fsync_duration_ns", float64(ws.MinFsyncDuration.Nanoseconds()))
	e.stats.set(pfx+"max_fsync_duration_ns", float64(ws.MaxFsyncDuration.Nanoseconds()))
	e.stats.set(pfx+"flush_count", float64(ws.FlushCount))
	e.stats.set(pfx+"flushed_bytes", float64(ws.FlushedBytes))
	e.stats.set(pfx+"min_flush_bytes", float64(ws.MinFlushBytes))
	e.stats.set(pfx+"max_flush_bytes", float64(ws.MaxFlushBytes))
	e.stats.set(pfx+"flush_interval_count", float64(ws.FlushIntervalCount))
	e.stats.set(pfx+"flush_interval_total_ns", float64(ws.FlushIntervalTotal.Nanoseconds()))
	e.stats.set(pfx+"min_flush_interval_ns", float64(ws.MinFlushInterval.Nanoseconds()))
	e.stats.set(pfx+"max_flush_interval_ns", float64(ws.MaxFlushInterval.Nanoseconds()))
	e.stats.set(pfx+"reset_duration_total_ns", float64(ws.ResetDurationTotal.Nanoseconds()))
	e.stats.set(pfx+"min_reset_duration_ns", float64(ws.MinResetDuration.Nanoseconds()))
	e.stats.set(pfx+"max_reset_duration_ns", float64(ws.MaxResetDuration.Nanoseconds()))
}

// maybeFlushStats writes engine stats to the internal DB at most once per StatsInterval.
// Safe to call without holding any lock. Skips for the internal DB itself.
func (e *Engine) maybeFlushStats(dbName string) {
	if !e.StatsEnabled || dbName == internalStatsDatabase {
		return
	}
	e.statsLastMu.Lock()
	now := time.Now()
	if e.StatsInterval > 0 && !e.statsLastFlush.IsZero() && now.Sub(e.statsLastFlush) < e.StatsInterval {
		e.statsLastMu.Unlock()
		return
	}
	e.statsLastFlush = now
	e.statsLastMu.Unlock()
	e.flushStatsToInternal(Timestamp(now.UnixNano()))
}

// flushStatsToInternal writes the current engine stat snapshot through AddLine,
// so internal stats follow the same WAL/page/data-file path as external ingestion.
// Safe to call without holding e.mu.
func (e *Engine) flushStatsToInternal(ts Timestamp) {
	if !e.StatsEnabled {
		return
	}
	snap := e.stats.snapshot()
	if len(snap) == 0 {
		return
	}

	tsText := strconv.FormatInt(int64(ts), 10)
	for k, v := range snap {
		metric := internalStatsMetricPrefix + "/" + k
		valText := strconv.FormatFloat(float64(float32(v)), 'f', -1, 32)
		line := internalStatsDatabase + "/" + metric + " " + valText + " " + tsText
		_ = e.AddLine(line)
	}
}

// getOrCreateDBLocked is like getOrCreateDB but assumes the caller holds e.mu write lock.
func (e *Engine) getOrCreateDBLocked(database string) (*Database, *dbRuntime, error) {
	if db, ok := e.dbs[database]; ok {
		if rt := e.runtimes[database]; rt != nil {
			return db, rt, nil
		}
	}
	dbDir := filepath.Join(e.RootDataDir, database)
	fullName := filepath.Join(dbDir, database)
	db, err := NewDatabaseWithWALConfig(fullName, e.WALMaxSegSize, e.WALFsyncPolicy)
	if err != nil {
		return nil, nil, err
	}
	var replayRecords int64
	var replayBytes int64
	if db.wal != nil {
		count, bytes, err := scanWALAppendStats(db.wal.path)
		if err != nil {
			e.recordWALReplayMetrics(database, 0, 0, false)
			_ = db.Close()
			return nil, nil, fmt.Errorf("recover wal counters for database %q: %w", database, err)
		}
		db.wal.stats.AppendCount = count
		db.wal.stats.AppendBytes = bytes
		replayRecords = count
		replayBytes = bytes
	}
	info, err := loadOrCreateDBInfo(db.RootDataDir, e.dbDefaults)
	if err != nil {
		return nil, nil, err
	}
	walSkipBefore, _ := time.ParseDuration(info.WALSkipBefore)
	pageMaxAge, _ := time.ParseDuration(info.PageMaxAge)
	rt := &dbRuntime{info: info, walSkipBefore: walSkipBefore, pageMaxAge: pageMaxAge, openDays: make(map[string]*Page), sealedDays: make(map[string]struct{})}
	if err := e.replayWALIntoRuntime(db, rt, database); err != nil {
		e.recordWALReplayMetrics(database, replayRecords, replayBytes, false)
		_ = db.Close()
		return nil, nil, fmt.Errorf("replay wal for database %q: %w", database, err)
	}
	if database != internalStatsDatabase {
		e.recordWALReplayMetrics(database, replayRecords, replayBytes, true)
		e.captureWALStats(db, database)
	}
	e.dbs[database] = db
	e.runtimes[database] = rt
	return db, rt, nil
}

func (e *Engine) recordWALReplayMetrics(database string, records, bytes int64, success bool) {
	if database == internalStatsDatabase {
		return
	}
	pfx := database + "/wal/"
	e.stats.set(pfx+"replay_records", float64(records))
	e.stats.set(pfx+"replay_bytes", float64(bytes))
	if success {
		e.stats.incr(pfx+"replay_success_count", 1)
	} else {
		e.stats.incr(pfx+"replay_error_count", 1)
	}
}

// replayWALIntoRuntime rebuilds open in-memory day pages from WAL records.
// Replay uses the same day open/seal policy as normal ingestion.
func (e *Engine) replayWALIntoRuntime(db *Database, rt *dbRuntime, dbName string) error {
	if db == nil || db.wal == nil || rt == nil {
		return nil
	}
	recs, err := db.wal.RecordsWithCatalog(db.catalog)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}

	for _, rec := range recs {
		if rec.MetricName != "" {
			if err := db.catalog.EnsureMetricEntry(rec.MetricName, rec.MetricID, rec.ValueType); err != nil {
				return err
			}
		}
		if _, _, ok := db.catalog.GetMetricByID(rec.MetricID); !ok {
			return fmt.Errorf("missing catalog entry for wal metric id %d", rec.MetricID)
		}

		day := partitionKey(rt, rec.Timestamp)
		if err := e.ensureDayOpen(db, rt, dbName, day, rec.Timestamp); err != nil {
			return err
		}

		var raw [4]byte
		switch rec.ValueType {
		case Int32Sample:
			v, ok := rec.Value.(int32)
			if !ok {
				return fmt.Errorf("invalid wal int32 value type %T", rec.Value)
			}
			binary.LittleEndian.PutUint32(raw[:], uint32(v))
		case Float32Sample:
			v, ok := rec.Value.(float32)
			if !ok {
				return fmt.Errorf("invalid wal float32 value type %T", rec.Value)
			}
			binary.LittleEndian.PutUint32(raw[:], math.Float32bits(v))
		default:
			return fmt.Errorf("invalid wal value type %d", rec.ValueType)
		}

		if err := e.addToOpenDay(db, rt, day, rec.Timestamp, rec.MetricID, raw[:], rec.SegmentID); err != nil {
			return err
		}
	}

	return nil
}

func shouldWriteWAL(rt *dbRuntime, ts Timestamp, now time.Time) bool {
	if rt == nil || !rt.info.WALEnabled {
		return false
	}
	if rt.walSkipBefore <= 0 {
		return true
	}
	cutoff := Timestamp(now.Add(-rt.walSkipBefore).UnixNano())
	return ts >= cutoff
}

func (e *Engine) ensureDayOpen(db *Database, rt *dbRuntime, dbName, day string, ts Timestamp) error {
	if _, sealed := rt.sealedDays[day]; sealed {
		return fmt.Errorf("day %s already sealed for database %s", day, db.Name)
	}
	if _, ok := rt.openDays[day]; ok {
		return nil
	}
	if rt.info.MaxActiveDays < 1 {
		rt.info.MaxActiveDays = 2
	}
	if len(rt.openDays) >= rt.info.MaxActiveDays {
		oldest, err := oldestOpenDay(rt.openDays)
		if err != nil {
			return err
		}
		if err := sealDay(e, db, rt, dbName, oldest, ts); err != nil {
			return err
		}
	}
	rt.openDays[day] = nil
	return nil
}

func oldestOpenDay(open map[string]*Page) (string, error) {
	if len(open) == 0 {
		return "", fmt.Errorf("no open days")
	}
	days := make([]string, 0, len(open))
	for day := range open {
		days = append(days, day)
	}
	sort.Strings(days)
	return days[0], nil
}

func sealDay(e *Engine, db *Database, rt *dbRuntime, dbName, day string, nowTS Timestamp) error {
	if p := rt.openDays[day]; p != nil {
		if err := e.writePageToDailyFile(db, dbName, day, p); err != nil {
			return err
		}
	}
	delete(rt.openDays, day)
	if err := maybeResetWAL(db, rt); err != nil {
		return err
	}
	e.captureWALStats(db, dbName)
	rt.sealedDays[day] = struct{}{}
	cleanupRetention(db, rt.info.RetentionDays, nowTS)
	return nil
}

func maybeResetWAL(db *Database, rt *dbRuntime) error {
	if db == nil || db.wal == nil || rt == nil {
		return nil
	}
	if !rt.info.WALEnabled {
		return nil
	}
	for _, p := range rt.openDays {
		if p != nil {
			return nil
		}
	}
	if db.catalog != nil && db.catalog.IsDirty() {
		if err := db.catalog.WriteCatalog(); err != nil {
			return err
		}
	}
	return db.wal.Reset()
}

// writePageToDailyFile encodes page to disk and updates engine-level data flush stats.
func (e *Engine) writePageToDailyFile(db *Database, dbName, day string, page *Page) error {
	start := time.Now()
	st, err := writePageWithOptions(db, day, page, e.SyncDataFile, e.SyncCatalog)
	if err != nil {
		return err
	}
	flushDurNs := float64(time.Since(start).Nanoseconds())
	records := float64(len(page.Times))
	bytes := float64(st.FrameBytes)
	compressed := float64(st.CompressedBytes)
	e.stats.incr(dbName+"/data/flush_count", 1)
	e.stats.incr(dbName+"/data/flush_bytes", bytes)
	e.stats.incr(dbName+"/data/flush_records", records)
	e.stats.incr(dbName+"/data/flush_compressed_bytes", compressed)
	e.stats.incr(dbName+"/data/flush_duration_total_ns", flushDurNs)
	e.stats.setMax(dbName+"/data/max_flush_bytes", bytes)
	e.stats.setMin(dbName+"/data/min_flush_bytes", bytes)
	e.stats.setMax(dbName+"/data/max_flush_records", records)
	e.stats.setMin(dbName+"/data/min_flush_records", records)
	e.stats.setMax(dbName+"/data/max_flush_compressed_bytes", compressed)
	e.stats.setMin(dbName+"/data/min_flush_compressed_bytes", compressed)
	e.stats.setMax(dbName+"/data/max_flush_duration_ns", flushDurNs)
	e.stats.setMin(dbName+"/data/min_flush_duration_ns", flushDurNs)
	if st.SyncDuration > 0 {
		syncDurNs := float64(st.SyncDuration.Nanoseconds())
		e.stats.incr(dbName+"/data/fsync_count", 1)
		e.stats.incr(dbName+"/data/fsync_duration_total_ns", syncDurNs)
		e.stats.setMax(dbName+"/data/max_fsync_duration_ns", syncDurNs)
		e.stats.setMin(dbName+"/data/min_fsync_duration_ns", syncDurNs)
	}
	return nil
}

// writePage is the raw disk write used by both writePageToDailyFile and flushStatsToInternal.
func writePage(db *Database, day string, page *Page) error {
	_, err := writePageWithOptions(db, day, page, true, true)
	return err
}

type pageFlushStats struct {
	FrameBytes      int64
	CompressedBytes int64
	SyncDuration    time.Duration
}

func writePageWithOptions(db *Database, day string, page *Page, syncDataFile bool, syncCatalog bool) (pageFlushStats, error) {
	bb := pageWriteBufferPool.Get().(*bytes.Buffer)
	bb.Reset()
	defer pageWriteBufferPool.Put(bb)

	if err := page.EncodeInto(bb); err != nil {
		return pageFlushStats{}, err
	}
	encoded := bb.Bytes()
	compressedLen, n := binary.Uvarint(encoded[HeaderSize:])
	if n <= 0 {
		return pageFlushStats{}, fmt.Errorf("invalid encoded page frame")
	}
	st := pageFlushStats{FrameBytes: int64(len(encoded)), CompressedBytes: int64(compressedLen)}
	path := filepath.Join(db.RootDataDir, "data-"+day+".dat")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return pageFlushStats{}, err
	}
	defer f.Close()
	if _, err := f.Write(encoded); err != nil {
		return pageFlushStats{}, err
	}
	if syncDataFile {
		syncStart := time.Now()
		if err := f.Sync(); err != nil {
			return pageFlushStats{}, err
		}
		st.SyncDuration = time.Since(syncStart)
	}
	if syncCatalog && db.catalog.IsDirty() {
		if err := db.catalog.WriteCatalog(); err != nil {
			return pageFlushStats{}, err
		}
	}
	return st, nil
}

func cleanupRetention(db *Database, retentionDays int, nowTS Timestamp) {
	if retentionDays <= 0 {
		return
	}
	entries, err := os.ReadDir(db.RootDataDir)
	if err != nil {
		return
	}
	cutoff := dayStartUTC(nowTS).AddDate(0, 0, -retentionDays)
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasPrefix(name, "data-") || !strings.HasSuffix(name, ".dat") {
			continue
		}
		part := strings.TrimSuffix(strings.TrimPrefix(name, "data-"), ".dat")
		t, err := parsePartitionStart(part)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(filepath.Join(db.RootDataDir, name))
		}
	}
}

func dayStartUTC(ts Timestamp) time.Time {
	t := time.Unix(0, int64(ts)).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func dayKey(ts Timestamp) string {
	return dayStartUTC(ts).Format("2006-01-02")
}

func monthKey(ts Timestamp) string {
	t := time.Unix(0, int64(ts)).UTC()
	return t.Format("2006-01")
}

func yearKey(ts Timestamp) string {
	t := time.Unix(0, int64(ts)).UTC()
	return t.Format("2006")
}

func parsePartitionStart(part string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", part); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01", part); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006", part); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid partition key: %s", part)
}

func partitionKey(rt *dbRuntime, ts Timestamp) string {
	if rt != nil {
		switch strings.ToLower(strings.TrimSpace(rt.info.Partition)) {
		case "month":
			return monthKey(ts)
		case "year":
			return yearKey(ts)
		case "forever":
			return "forever"
		}
	}
	return dayKey(ts)
}

func normalizeDBInfo(info DBInfo, defaults DBInfo) (DBInfo, error) {
	def := defaults
	if info.MaxActiveDays <= 0 {
		info.MaxActiveDays = def.MaxActiveDays
	}
	if info.RetentionDays <= 0 {
		info.RetentionDays = def.RetentionDays
	}
	if strings.TrimSpace(info.Grace) == "" {
		info.Grace = def.Grace
	}
	if strings.TrimSpace(info.WALSkipBefore) == "" {
		info.WALSkipBefore = def.WALSkipBefore
	}
	if info.PageMaxRecords <= 0 {
		info.PageMaxRecords = def.PageMaxRecords
	}
	if info.PageMaxBytes <= 0 {
		info.PageMaxBytes = def.PageMaxBytes
	}
	if strings.TrimSpace(info.PageMaxAge) == "" {
		info.PageMaxAge = def.PageMaxAge
	}
	if strings.TrimSpace(info.Partition) == "" {
		info.Partition = def.Partition
	}
	if strings.TrimSpace(info.Rollups.CheckpointFile) == "" {
		if strings.TrimSpace(def.Rollups.CheckpointFile) != "" {
			info.Rollups.CheckpointFile = def.Rollups.CheckpointFile
		} else {
			info.Rollups.CheckpointFile = defaultRollupCheckpointFile
		}
	}
	if strings.TrimSpace(info.Rollups.DefaultGrace) == "" {
		info.Rollups.DefaultGrace = strings.TrimSpace(def.Rollups.DefaultGrace)
	}
	if _, err := time.ParseDuration(info.Grace); err != nil {
		return DBInfo{}, fmt.Errorf("invalid grace: %w", err)
	}
	if _, err := time.ParseDuration(info.WALSkipBefore); err != nil {
		return DBInfo{}, fmt.Errorf("invalid wal_skip_before: %w", err)
	}
	if _, err := time.ParseDuration(info.PageMaxAge); err != nil {
		return DBInfo{}, fmt.Errorf("invalid page_max_age: %w", err)
	}
	info.Partition = strings.ToLower(strings.TrimSpace(info.Partition))
	if info.Partition == "" {
		info.Partition = "day"
	}
	if info.Partition != "day" && info.Partition != "month" && info.Partition != "year" && info.Partition != "forever" {
		return DBInfo{}, fmt.Errorf("invalid partition: %q (expected day|month|year|forever)", info.Partition)
	}
	if !info.Rollups.Enabled {
		info.Rollups.Jobs = nil
		return info, nil
	}
	if strings.TrimSpace(info.Rollups.DefaultGrace) != "" {
		if _, err := time.ParseDuration(info.Rollups.DefaultGrace); err != nil {
			return DBInfo{}, fmt.Errorf("invalid rollups.default_grace: %w", err)
		}
	}
	for idx := range info.Rollups.Jobs {
		job := &info.Rollups.Jobs[idx]
		job.ID = strings.TrimSpace(job.ID)
		job.SourceMetric = strings.TrimSpace(job.SourceMetric)
		job.Interval = strings.TrimSpace(job.Interval)
		job.DestinationDB = strings.TrimSpace(job.DestinationDB)
		job.DestinationMetricPrefix = strings.TrimSpace(job.DestinationMetricPrefix)
		job.Grace = strings.TrimSpace(job.Grace)
		if job.ID == "" {
			return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].id: empty", idx)
		}
		if job.SourceMetric == "" {
			return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].source_metric: empty", idx)
		}
		if job.Interval == "" {
			return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].interval: empty", idx)
		}
		if _, err := time.ParseDuration(job.Interval); err != nil {
			return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].interval: %w", idx, err)
		}
		if job.DestinationDB == "" {
			return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].destination_db: empty", idx)
		}
		if job.DestinationMetricPrefix == "" {
			job.DestinationMetricPrefix = job.SourceMetric
		}
		if job.Grace != "" {
			if _, err := time.ParseDuration(job.Grace); err != nil {
				return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].grace: %w", idx, err)
			}
		}
		if len(job.Aggregates) == 0 {
			job.Aggregates = defaultRollupAggregates()
		}
		for aggIdx, agg := range job.Aggregates {
			agg = strings.TrimSpace(strings.ToLower(agg))
			if !isSupportedRollupAggregate(agg) {
				return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].aggregates[%d]: %q (supported: %s)", idx, aggIdx, job.Aggregates[aggIdx], strings.Join(supportedRollupAggregates(), ","))
			}
			job.Aggregates[aggIdx] = agg
		}
	}
	return info, nil
}

func dbManifestFromInfo(info DBInfo) DBManifestTOML {
	return DBManifestTOML{
		Retention: DBManifestRetention{
			Grace:         info.Grace,
			RetentionDays: info.RetentionDays,
			MaxActiveDays: info.MaxActiveDays,
			Partition:     info.Partition,
		},
		WAL: DBManifestWAL{
			Enabled:    info.WALEnabled,
			SkipBefore: info.WALSkipBefore,
		},
		Page: DBManifestPage{
			MaxRecords: info.PageMaxRecords,
			MaxBytes:   info.PageMaxBytes,
			MaxAge:     info.PageMaxAge,
		},
		Rollups: info.Rollups,
	}
}

func dbInfoFromManifest(man DBManifestTOML) DBInfo {
	return DBInfo{
		Grace:          man.Retention.Grace,
		RetentionDays:  man.Retention.RetentionDays,
		MaxActiveDays:  man.Retention.MaxActiveDays,
		Partition:      man.Retention.Partition,
		WALEnabled:     man.WAL.Enabled,
		WALSkipBefore:  man.WAL.SkipBefore,
		PageMaxRecords: man.Page.MaxRecords,
		PageMaxBytes:   man.Page.MaxBytes,
		PageMaxAge:     man.Page.MaxAge,
		Rollups:        man.Rollups,
	}
}

func writeDBInfoTOML(path string, info DBInfo) error {
	buf := bytes.NewBuffer(nil)
	if err := toml.NewEncoder(buf).Encode(dbManifestFromInfo(info)); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}

func loadOrCreateDBInfo(root string, defaults DBInfo) (DBInfo, error) {
	path := filepath.Join(root, manifestFileName)
	if raw, err := os.ReadFile(path); err == nil {
		manifest := DBManifestTOML{}
		if _, err := toml.Decode(string(raw), &manifest); err != nil {
			return DBInfo{}, fmt.Errorf("parse %s: %w", path, err)
		}
		info := dbInfoFromManifest(manifest)
		info, err = normalizeDBInfo(info, defaults)
		if err != nil {
			return DBInfo{}, fmt.Errorf("invalid %s: %w", path, err)
		}
		return info, nil
	} else if !os.IsNotExist(err) {
		return DBInfo{}, err
	}

	info, err := normalizeDBInfo(defaults, defaults)
	if err != nil {
		return DBInfo{}, err
	}
	if err := writeDBInfoTOML(path, info); err != nil {
		return DBInfo{}, err
	}
	return info, nil
}

func parseLineProtocol(line string) (database, metric string, ts Timestamp, valueType byte, i32 int32, f32 float32, err error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		err = fmt.Errorf("invalid line protocol: expected 'DB/metric value [ts]'")
		return
	}

	path := fields[0]
	slash := strings.IndexByte(path, '/')
	if slash <= 0 || slash >= len(path)-1 {
		err = fmt.Errorf("invalid target %q: expected DB/metric", path)
		return
	}
	database = path[:slash]
	metric = path[slash+1:]
	if strings.TrimSpace(metric) == "" {
		err = fmt.Errorf("metric cannot be empty")
		return
	}

	valText := fields[1]
	if strings.HasSuffix(valText, "i") {
		intText := strings.TrimSuffix(valText, "i")
		if intText == "" {
			err = fmt.Errorf("invalid int value %q", valText)
			return
		}
		parsed, perr := strconv.ParseInt(intText, 10, 64)
		if perr != nil {
			err = fmt.Errorf("invalid int value %q", valText)
			return
		}
		if parsed < math.MinInt32 || parsed > math.MaxInt32 {
			err = fmt.Errorf("int value out of int32 range %q", valText)
			return
		}
		valueType = Int32Sample
		i32 = int32(parsed)
	} else {
		parsed, perr := strconv.ParseFloat(valText, 32)
		if perr != nil {
			err = fmt.Errorf("invalid numeric value %q", valText)
			return
		}
		valueType = Float32Sample
		f32 = float32(parsed)
	}

	if len(fields) > 2 {
		tsText := strings.Join(fields[2:], " ")
		parsedTS, perr := ParseTimestamp(tsText)
		if perr != nil {
			err = fmt.Errorf("invalid timestamp %q", tsText)
			return
		}
		ts = parsedTS
	} else {
		ts = Timestamp(time.Now().UnixNano())
	}

	return
}
