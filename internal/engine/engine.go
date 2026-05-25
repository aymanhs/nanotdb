package engine

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
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
type OpenPageStats struct {
	Day          string        `json:"day"`
	Records      int           `json:"records"`
	MetricSlots  int           `json:"metric_slots"`
	UniqueMetric int           `json:"unique_metrics"`
	ValueBytes   int           `json:"value_bytes"`
	StartTS      Timestamp     `json:"start_timestamp_ns"`
	EndTS        Timestamp     `json:"end_timestamp_ns"`
	MaxRecords   int           `json:"max_records"`
	MaxBytes     int           `json:"max_bytes"`
	MaxAge       time.Duration `json:"max_age_ns"`
	Age          time.Duration `json:"age_ns"`
	WALSegmentID uint16        `json:"wal_segment_id"`
	Full         bool          `json:"full"`
	Persisted    bool          `json:"persisted"`
}

type DBRuntimeInspect struct {
	Database    string          `json:"database"`
	MetricCount int             `json:"metric_count"`
	Manifest    DBInfo          `json:"manifest"`
	Stats       DBStats         `json:"stats"`
	OpenPages   []OpenPageStats `json:"open_pages"`
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
	Enabled               bool                  `toml:"enabled"`
	CheckpointFile        string                `toml:"checkpoint_file"`
	DefaultGrace          string                `toml:"default_grace"`
	DefaultInterval       string                `toml:"default_interval"`
	DefaultDestinationDB  string                `toml:"default_destination_db"`
	DefaultAggregates     []string              `toml:"default_aggregates"`
	GlobalExcludePatterns []string              `toml:"global_exclude_patterns"`
	Jobs                  []DBManifestRollupJob `toml:"jobs"`
}

type DBManifestRollupJob struct {
	ID                      string   `toml:"id"`
	SourceMetric            string   `toml:"source_metric"`
	SourcePattern           string   `toml:"source_pattern"`
	ExcludePatterns         []string `toml:"exclude_patterns"`
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
	Metrics          EngineConfigMetrics          `toml:"metrics"`
	Logging          EngineConfigLogging          `toml:"logging"`
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

type EngineConfigMetrics struct {
	Enabled         bool   `toml:"enabled"`
	Compression     string `toml:"compression"`
	RawIngestAction string `toml:"raw_ingest_action"`
	TimeCacheSlots  int    `toml:"time_cache_slots"`
}

type EngineConfigLogging struct {
	Loggers []EngineConfigLogger `toml:"logger"`
}

type EngineConfigLogger struct {
	Output string `toml:"output"`
	Level  string `toml:"level"`
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

const (
	LogLevelInfo  = "info"
	LogLevelDebug = "debug"
	LogLevelTrace = "trace"
)

//go:embed default_engine.toml
var defaultEngineConfigTOML string

// Engine is the top-level coordinator for all databases in a root data directory.
// It is safe for concurrent use. Open with OpenEngine; always call Close when done.
type Engine struct {
	RootDataDir           string
	WALMaxSegSize         int64
	WALFsyncPolicy        string
	Durability            string
	PreferMetricFiles     bool
	AutoCreateMetricFiles bool
	MetricFileCompression string
	MetricRawIngestAction string
	MetricTimeCacheSlots  int
	Logging               EngineConfigLogging
	logger                *slog.Logger
	SyncDataFile          bool
	SyncCatalog           bool
	StatsEnabled          bool
	StatsInterval         time.Duration
	dbDefaults            DBInfo

	mu             sync.RWMutex
	writeMu        sync.Mutex
	dbs            map[string]*Database
	runtimes       map[string]*dbRuntime
	rollupBackfill sync.Mutex
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

var pageCompressedBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 64*1024)
		return &buf
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
			Enabled:               false,
			CheckpointFile:        defaultRollupCheckpointFile,
			DefaultGrace:          "",
			DefaultInterval:       "",
			DefaultDestinationDB:  "",
			DefaultAggregates:     nil,
			GlobalExcludePatterns: nil,
			Jobs:                  nil,
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
		Metrics: EngineConfigMetrics{
			Enabled:         false,
			Compression:     CompressionCodecZstdFastestName,
			RawIngestAction: MetricRawIngestActionKeep,
			TimeCacheSlots:  metricTimeFrameCacheMaxEntriesV2,
		},
		Logging: EngineConfigLogging{Loggers: []EngineConfigLogger{{
			Output: "console",
			Level:  LogLevelInfo,
		}}},
		Stats: EngineConfigStats{Enabled: true, Interval: "30s"},
		Defaults: EngineConfigDefaults{
			Databases: []string{},
		},
		ManifestDefaults: engineConfigManifestDefaultsFromInfo(dbDef),
	}
}

func normalizeLoggingConfig(cfg EngineConfigLogging, def EngineConfigLogging) (EngineConfigLogging, error) {
	if len(cfg.Loggers) == 0 {
		cfg.Loggers = append([]EngineConfigLogger(nil), def.Loggers...)
	}
	if len(cfg.Loggers) == 0 {
		return EngineConfigLogging{}, fmt.Errorf("logging.logger must contain at least one logger")
	}

	hasConsole := false
	for i := range cfg.Loggers {
		entry := &cfg.Loggers[i]
		entry.Output = strings.TrimSpace(entry.Output)
		entry.Level = strings.ToLower(strings.TrimSpace(entry.Level))
		if entry.Output == "" {
			return EngineConfigLogging{}, fmt.Errorf("invalid logging.logger[%d].output: empty", i)
		}
		if entry.Level == "" {
			entry.Level = LogLevelInfo
		}
		switch entry.Level {
		case LogLevelInfo, LogLevelDebug, LogLevelTrace:
		default:
			return EngineConfigLogging{}, fmt.Errorf("invalid logging.logger[%d].level: %q", i, entry.Level)
		}
		if entry.Output == "console" {
			if hasConsole {
				return EngineConfigLogging{}, fmt.Errorf("invalid logging.logger[%d].output: duplicate console logger", i)
			}
			hasConsole = true
		}
	}

	return cfg, nil
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
	info := dbInfoFromManifest(DBManifestTOML(cfg))
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
	if strings.TrimSpace(cfg.Metrics.Compression) == "" {
		cfg.Metrics.Compression = def.Metrics.Compression
	}
	cfg.Metrics.Compression = strings.ToLower(strings.TrimSpace(cfg.Metrics.Compression))
	if _, err := BlockCompressionCodecByName(cfg.Metrics.Compression); err != nil {
		return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("invalid metrics.compression: %w", err)
	}
	if strings.TrimSpace(cfg.Metrics.RawIngestAction) == "" {
		cfg.Metrics.RawIngestAction = def.Metrics.RawIngestAction
	}
	cfg.Metrics.RawIngestAction = strings.ToLower(strings.TrimSpace(cfg.Metrics.RawIngestAction))
	if !isValidMetricRawIngestAction(cfg.Metrics.RawIngestAction) {
		return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("invalid metrics.raw_ingest_action: %q", cfg.Metrics.RawIngestAction)
	}
	if cfg.Metrics.TimeCacheSlots <= 0 {
		cfg.Metrics.TimeCacheSlots = def.Metrics.TimeCacheSlots
	}
	if cfg.Metrics.TimeCacheSlots <= 0 {
		return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("invalid metrics.time_cache_slots: must be > 0")
	}
	loggingCfg, err := normalizeLoggingConfig(cfg.Logging, def.Logging)
	if err != nil {
		return EngineConfig{}, 0, DBInfo{}, fmt.Errorf("invalid logging: %w", err)
	}
	cfg.Logging = loggingCfg
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
	cfg, statsInterval, dbDefaults, err := LoadEngineConfig(rootDataDir, walMaxSegSize)
	if err != nil {
		return nil, err
	}
	return OpenEngineWithConfig(rootDataDir, cfg, statsInterval, dbDefaults, nil)
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
	e.logInfo("engine closing", "data_dir", e.RootDataDir)
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
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
				delete(rt.openDays, day)
			}
		}
		if db.wal != nil && db.wal.Stats().BufferBytes > 0 {
			e.logDebug("wal reset", "database", name, "buffer_bytes", db.wal.Stats().BufferBytes)
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
		e.logDebug("database closed", "database", name)
	}
	e.logInfo("engine closed", "data_dir", e.RootDataDir)
	return nil
}

func (e *Engine) flushDatabases(databaseNames []string) error {
	seen := make(map[string]struct{}, len(databaseNames))
	names := make([]string, 0, len(databaseNames))
	for _, name := range databaseNames {
		name = strings.TrimSpace(name)
		if name == "" || name == internalStatsDatabase {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, name := range names {
		db := e.dbs[name]
		rt := e.runtimes[name]
		if db == nil || rt == nil {
			continue
		}
		for day, p := range rt.openDays {
			if p == nil {
				continue
			}
			if err := e.writePageToDailyFile(db, name, day, p); err != nil {
				return fmt.Errorf("flush database %q day %s: %w", name, day, err)
			}
			delete(rt.openDays, day)
		}
		if db.wal != nil && db.wal.Stats().BufferBytes > 0 {
			e.logDebug("wal reset", "database", name, "buffer_bytes", db.wal.Stats().BufferBytes)
		}
		if err := maybeResetWAL(db, rt); err != nil {
			return fmt.Errorf("reset wal for database %q: %w", name, err)
		}
		if !rt.info.WALEnabled && db.wal != nil {
			if err := db.wal.Reset(); err != nil {
				return fmt.Errorf("reset disabled wal for database %q: %w", name, err)
			}
		}
		e.captureWALStats(db, name)
		if db.catalog != nil && db.catalog.IsDirty() {
			if err := db.catalog.WriteCatalog(); err != nil {
				return fmt.Errorf("write catalog for database %q: %w", name, err)
			}
		}
	}
	return nil
}

// GetAllDatabaseNames returns all database names managed by this engine.
func (e *Engine) GetAllDatabaseNames() []string {
	nameSet := make(map[string]struct{})
	e.mu.RLock()
	for name := range e.dbs {
		nameSet[name] = struct{}{}
	}
	e.mu.RUnlock()
	for _, name := range e.discoverDatabaseNames() {
		nameSet[name] = struct{}{}
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsDatabaseActive reports whether a database is currently loaded in memory.
func (e *Engine) IsDatabaseActive(database string) bool {
	database = strings.TrimSpace(database)
	if database == "" {
		return false
	}
	e.mu.RLock()
	_, ok := e.dbs[database]
	e.mu.RUnlock()
	return ok
}

// ListMetrics returns all known metrics for a database in stable name order.
func (e *Engine) ListMetrics(database string) ([]MetricInfo, error) {
	database = strings.TrimSpace(database)
	if database == "" {
		return nil, fmt.Errorf("database cannot be empty")
	}
	if !e.hasDatabase(database) {
		return nil, fmt.Errorf("database not found: %s", database)
	}
	db, _, err := e.getOrCreateDB(database)
	if err != nil {
		return nil, err
	}
	return db.catalog.ListMetrics(), nil
}

func (e *Engine) discoverDatabaseNames() []string {
	entries, err := os.ReadDir(e.RootDataDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name := strings.TrimSpace(ent.Name())
		if name == "" {
			continue
		}
		if databaseDirLooksReal(filepath.Join(e.RootDataDir, name), name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (e *Engine) hasDatabase(database string) bool {
	database = strings.TrimSpace(database)
	if database == "" {
		return false
	}
	e.mu.RLock()
	_, ok := e.dbs[database]
	e.mu.RUnlock()
	if ok {
		return true
	}
	return databaseDirLooksReal(filepath.Join(e.RootDataDir, database), database)
}

func databaseDirLooksReal(dirPath, database string) bool {
	st, err := os.Stat(dirPath)
	if err != nil || !st.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(dirPath, manifestFileName)); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dirPath, "catalog.json")); err == nil {
		return true
	}
	if matches, err := filepath.Glob(filepath.Join(dirPath, "data-*.dat")); err == nil && len(matches) > 0 {
		return true
	}
	if matches, err := filepath.Glob(filepath.Join(dirPath, database+".wal")); err == nil && len(matches) > 0 {
		return true
	}
	return false
}

type MetricRollupDownstream struct {
	Hop       int
	JobID     string
	Interval  string
	Aggregate string
	Database  string
	Metric    string
}

// GetMetricRollupDownstream returns bounded downstream rollup lineage for one metric.
// Lineage is derived from configured rollup jobs in loaded database manifests.
func (e *Engine) GetMetricRollupDownstream(database, metric string, maxHops int) ([]MetricRollupDownstream, bool, error) {
	database = strings.TrimSpace(database)
	metric = strings.TrimSpace(metric)
	if database == "" {
		return nil, false, fmt.Errorf("database cannot be empty")
	}
	if metric == "" {
		return nil, false, fmt.Errorf("metric cannot be empty")
	}
	if maxHops < 1 {
		return nil, false, fmt.Errorf("max_hops must be >= 1")
	}

	type lineageNode struct {
		db     string
		metric string
	}
	type queueItem struct {
		node lineageNode
		hop  int
	}

	e.mu.RLock()
	defer e.mu.RUnlock()
	if _, ok := e.dbs[database]; !ok {
		return nil, false, fmt.Errorf("database not found: %s", database)
	}

	visited := map[lineageNode]struct{}{{db: database, metric: metric}: {}}
	q := []queueItem{{node: lineageNode{db: database, metric: metric}, hop: 0}}
	steps := make([]MetricRollupDownstream, 0)
	stepSeen := make(map[string]struct{})
	truncated := false

	for len(q) > 0 {
		cur := q[0]
		q = q[1:]

		if cur.hop >= maxHops {
			rt, ok := e.runtimes[cur.node.db]
			if ok && rt.info.Rollups.Enabled {
				for _, job := range rt.info.Rollups.Jobs {
					if strings.TrimSpace(job.SourceMetric) != cur.node.metric {
						continue
					}
					for _, aggRaw := range job.Aggregates {
						if _, ok := getAggregator(strings.TrimSpace(aggRaw)); ok {
							truncated = true
							break
						}
					}
					if truncated {
						break
					}
				}
			}
			continue
		}

		rt, ok := e.runtimes[cur.node.db]
		if !ok || !rt.info.Rollups.Enabled || len(rt.info.Rollups.Jobs) == 0 {
			continue
		}

		for _, job := range rt.info.Rollups.Jobs {
			if strings.TrimSpace(job.SourceMetric) != cur.node.metric {
				continue
			}

			nextHop := cur.hop + 1
			if nextHop > maxHops {
				truncated = true
				continue
			}

			for _, aggRaw := range job.Aggregates {
				agg := strings.TrimSpace(aggRaw)
				aggFn, ok := getAggregator(agg)
				if !ok {
					continue
				}

				destMetric := rollupDestinationMetricName(job, aggFn.Name())
				destDB := strings.TrimSpace(job.DestinationDB)
				if destDB == "" || destMetric == "" {
					continue
				}

				step := MetricRollupDownstream{
					Hop:       nextHop,
					JobID:     strings.TrimSpace(job.ID),
					Interval:  strings.TrimSpace(job.Interval),
					Aggregate: aggFn.Name(),
					Database:  destDB,
					Metric:    destMetric,
				}
				stepKey := fmt.Sprintf("%d|%s|%s|%s|%s|%s", step.Hop, step.JobID, step.Aggregate, step.Database, step.Metric, step.Interval)
				if _, ok := stepSeen[stepKey]; !ok {
					stepSeen[stepKey] = struct{}{}
					steps = append(steps, step)
				}

				nextNode := lineageNode{db: destDB, metric: destMetric}
				if _, seen := visited[nextNode]; !seen {
					visited[nextNode] = struct{}{}
					q = append(q, queueItem{node: nextNode, hop: nextHop})
				}
			}
		}
	}

	sort.Slice(steps, func(i, j int) bool {
		if steps[i].Hop != steps[j].Hop {
			return steps[i].Hop < steps[j].Hop
		}
		if steps[i].Database != steps[j].Database {
			return steps[i].Database < steps[j].Database
		}
		if steps[i].Metric != steps[j].Metric {
			return steps[i].Metric < steps[j].Metric
		}
		if steps[i].Aggregate != steps[j].Aggregate {
			return steps[i].Aggregate < steps[j].Aggregate
		}
		return steps[i].JobID < steps[j].JobID
	})

	return steps, truncated, nil
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

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	switch v := value.(type) {
	case int32:
		return e.addParsedSample(database, metric, ts, Int32Sample, v, 0, true, false, false)
	case float32:
		return e.addParsedSample(database, metric, ts, Float32Sample, 0, v, true, false, false)
	default:
		return fmt.Errorf("unsupported sample type")
	}
}

func (e *Engine) addParsedSample(dbName, metric string, ts Timestamp, vType byte, i32 int32, f32 float32, triggerRollups bool, forceWAL bool, allowOutOfOrderRetry bool) error {
	db, rt, err := e.getOrCreateDB(dbName)
	if err != nil {
		return err
	}

	entry, exists := db.catalog.GetMetricEntry(metric)
	if exists && entry.LastValid && ts < entry.LastTS {
		e.logTrace("stale sample rejected", "database", dbName, "metric", metric, "timestamp", ts, "last_timestamp", entry.LastTS)
		return fmt.Errorf("stale sample rejected for %s/%s: ts=%d < last=%d", dbName, metric, ts, entry.LastTS)
	}

	day := partitionKey(rt, ts)
	if err := e.ensureDayOpen(db, rt, dbName, day, ts, triggerRollups); err != nil {
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
		if !exists {
			e.logTrace("metric registered", "database", dbName, "metric", metric, "metric_id", metricID, "sample_type", "int32")
		}
		if useWAL {
			if !exists {
				walSegment, err = AppendSampleWithMetricName(db.wal, metricID, metric, ts, i32)
			} else {
				walSegment, err = AppendSample(db.wal, metricID, ts, i32)
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
		if !exists {
			e.logTrace("metric registered", "database", dbName, "metric", metric, "metric_id", metricID, "sample_type", "float32")
		}
		if useWAL {
			if !exists {
				walSegment, err = AppendSampleWithMetricName(db.wal, metricID, metric, ts, f32)
			} else {
				walSegment, err = AppendSample(db.wal, metricID, ts, f32)
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
		if err == ErrOutOfOrderTimestamp && allowOutOfOrderRetry {
			e.logTrace("out-of-order sample retry", "database", dbName, "metric", metric, "timestamp", ts, "day", day)
			if existing := rt.openDays[day]; existing != nil {
				if werr := e.writePageToDailyFile(db, dbName, day, existing); werr != nil {
					return werr
				}
				delete(rt.openDays, day)
			}
			if rerr := e.addToOpenDay(db, rt, day, ts, metricID, raw[:], walSegment); rerr != nil {
				return rerr
			}
		} else {
			if err == ErrOutOfOrderTimestamp {
				e.logTrace("out-of-order sample rejected", "database", dbName, "metric", metric, "timestamp", ts, "day", day)
			}
			return err
		}
	}
	p := rt.openDays[day]

	if useWAL && walSegment != 0 {
		e.stats.incr(dbName+"/wal/append_count", 1)
	}

	if err := e.flushEligibleOpenDays(db, rt, dbName, day, triggerRollups); err != nil {
		return err
	}

	if p.IsFull() {
		if err := e.writePageToDailyFile(db, dbName, day, p); err != nil {
			return err
		}
		delete(rt.openDays, day)
		if db.wal != nil && db.wal.Stats().BufferBytes > 0 {
			e.logDebug("wal reset", "database", dbName, "buffer_bytes", db.wal.Stats().BufferBytes)
		}
		if err := maybeResetWAL(db, rt); err != nil {
			return err
		}
		e.captureWALStats(db, dbName)
	}
	e.logTrace("sample ingested", "database", dbName, "metric", metric, "timestamp", ts, "day", day, "wal", useWAL)
	e.maybeFlushStats(dbName)
	return nil
}

func (e *Engine) flushEligibleOpenDays(db *Database, rt *dbRuntime, dbName, currentDay string, triggerRollups bool) error {
	if db == nil || rt == nil {
		return nil
	}
	for day, p := range rt.openDays {
		if day == currentDay || p == nil || !p.IsFull() {
			continue
		}
		if err := e.writePageToDailyFile(db, dbName, day, p); err != nil {
			return err
		}
		delete(rt.openDays, day)
	}
	return nil
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
		if err := exportLinesFromFile(database, db, path, w, &wroteAny); err != nil {
			return fmt.Errorf("export from %s: %w", path, err)
		}
	}

	openPages := e.snapshotOpenPages(rt, false)
	for _, openPage := range openPages {
		if err := exportLinesFromPage(database, db, openPage.page, w, &wroteAny); err != nil {
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
		if !e.hasDatabase(database) {
			return DBStats{}, false
		}
		var err error
		db, _, err = e.getOrCreateDB(database)
		if err != nil {
			return DBStats{}, false
		}
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

func (e *Engine) InspectDBRuntime(database string) (DBRuntimeInspect, bool) {
	e.mu.RLock()
	db := e.dbs[database]
	rt := e.runtimes[database]
	e.mu.RUnlock()
	if db == nil || rt == nil {
		if !e.hasDatabase(database) {
			return DBRuntimeInspect{}, false
		}
		var err error
		db, rt, err = e.getOrCreateDB(database)
		if err != nil {
			return DBRuntimeInspect{}, false
		}
	}

	stats, _ := e.DBStats(database)
	inspect := DBRuntimeInspect{
		Database:    database,
		MetricCount: len(db.catalog.ListMetrics()),
		Manifest:    rt.info,
		Stats:       stats,
		OpenPages:   make([]OpenPageStats, 0),
	}

	for _, openPage := range e.snapshotOpenPages(rt, true) {
		if openPage.page == nil {
			inspect.OpenPages = append(inspect.OpenPages, OpenPageStats{Day: openPage.day, Persisted: true})
			continue
		}
		p := openPage.page
		metricSet := make(map[MetricID]struct{}, len(p.Metrics))
		for _, mid := range p.Metrics {
			metricSet[mid] = struct{}{}
		}
		inspect.OpenPages = append(inspect.OpenPages, OpenPageStats{
			Day:          openPage.day,
			Records:      len(p.Times),
			MetricSlots:  len(p.Metrics),
			UniqueMetric: len(metricSet),
			ValueBytes:   p.Values.Len(),
			StartTS:      p.Start,
			EndTS:        p.End,
			MaxRecords:   p.MaxRecords,
			MaxBytes:     p.MaxBytes,
			MaxAge:       p.MaxAge,
			Age:          time.Since(p.createdAt),
			WALSegmentID: p.WALSegmentID,
			Full:         p.IsFull(),
			Persisted:    false,
		})
	}

	return inspect, true
}

func (e *Engine) InspectDBWAL(database string) ([]WALRecord, bool, error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	e.mu.RLock()
	db := e.dbs[database]
	e.mu.RUnlock()
	if db == nil {
		if !e.hasDatabase(database) {
			return nil, false, nil
		}
		var err error
		db, _, err = e.getOrCreateDB(database)
		if err != nil {
			return nil, false, err
		}
	}
	if db.wal == nil {
		return []WALRecord{}, true, nil
	}
	records, err := db.wal.RecordsWithCatalog(db.catalog)
	if err != nil {
		return nil, true, err
	}
	for i := range records {
		if strings.TrimSpace(records[i].MetricName) != "" {
			continue
		}
		if name, _, ok := db.catalog.GetMetricByID(records[i].MetricID); ok {
			records[i].MetricName = name
		}
	}
	return records, true, nil
}

// SampleCallback is invoked for each sample in a range query.
type SampleCallback func(Sample) error

func collectInt32Samples(database, metric string, times []Timestamp, values []int32, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	sample := Sample{Database: database, Metric: metric, ValueType: Int32Sample}
	for i, ts := range times {
		if ts < fromTS || ts > toTS {
			continue
		}
		if *count%stride != 0 {
			*count++
			continue
		}
		*count++
		sample.TS = ts
		sample.Int32 = values[i]
		if err := fn(sample); err != nil {
			return err
		}
	}
	return nil
}

func collectFloat32Samples(database, metric string, times []Timestamp, values []float32, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	sample := Sample{Database: database, Metric: metric, ValueType: Float32Sample}
	for i, ts := range times {
		if ts < fromTS || ts > toTS {
			continue
		}
		if *count%stride != 0 {
			*count++
			continue
		}
		*count++
		sample.TS = ts
		sample.Float32 = values[i]
		if err := fn(sample); err != nil {
			return err
		}
	}
	return nil
}

// QueryRange scans samples for a metric within a time range.
// Stride controls downsampling: stride=1 returns every sample, stride=N returns every Nth.
// Each matching sample is passed to the callback; callback errors terminate early.
func (e *Engine) QueryRange(database, metric string, fromTS, toTS Timestamp, stride int, fn SampleCallback) error {
	return e.queryRange(database, metric, fromTS, toTS, stride, fn, false)
}

func (e *Engine) queryRange(database, metric string, fromTS, toTS Timestamp, stride int, fn SampleCallback, writeLockHeld bool) error {
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
		path := metricRawPartitionPath(db.RootDataDir, part)
		if path == lastPath {
			continue
		}
		lastPath = path
		if e.PreferMetricFiles {
			metricPath := filepath.Join(db.RootDataDir, "metric-"+part+".dat")
			if err := collectMetricFromMetricFile(database, metric, entry, metricPath, fromTS, toTS, stride, &count, fn); err == nil {
				// metric frames processed, skip raw file to avoid double counting
			} else if os.IsNotExist(err) {
				rawPath, rawErr := resolveMetricRawPartitionPath(db.RootDataDir, part)
				if rawErr == nil {
					if err := collectMetricFromFile(database, metric, entry, rawPath, fromTS, toTS, stride, &count, fn); err == nil {
						// persisted raw frames processed
					} else if os.IsNotExist(err) {
						// no persisted file for this partition
					} else {
						return fmt.Errorf("read %s: %w", rawPath, err)
					}
				} else if os.IsNotExist(rawErr) {
					// no persisted file for this partition
				} else {
					return fmt.Errorf("resolve raw partition %s: %w", part, rawErr)
				}
			} else {
				return fmt.Errorf("read %s: %w", metricPath, err)
			}
		} else {
			rawPath, rawErr := resolveMetricRawPartitionPath(db.RootDataDir, part)
			if rawErr == nil {
				if err := collectMetricFromFile(database, metric, entry, rawPath, fromTS, toTS, stride, &count, fn); err == nil {
					// persisted raw frames processed
				} else if os.IsNotExist(err) {
					// no persisted file for this partition
				} else {
					return fmt.Errorf("read %s: %w", rawPath, err)
				}
			} else if os.IsNotExist(rawErr) {
				// no persisted file for this partition
			} else {
				return fmt.Errorf("resolve raw partition %s: %w", part, rawErr)
			}
		}

		if p := e.snapshotOpenPage(rt, part, writeLockHeld); p != nil {
			if err := collectMetricFromPage(database, metric, entry, p, fromTS, toTS, stride, &count, fn); err != nil {
				return err
			}
		}
	}

	return nil
}

type openDaySnapshot struct {
	day  string
	page *Page
}

func (e *Engine) snapshotOpenPage(rt *dbRuntime, day string, writeLockHeld bool) *Page {
	if rt == nil {
		return nil
	}
	if !writeLockHeld {
		e.writeMu.Lock()
		defer e.writeMu.Unlock()
	}
	return clonePage(rt.openDays[day])
}

func (e *Engine) snapshotOpenPages(rt *dbRuntime, includePersisted bool) []openDaySnapshot {
	if rt == nil {
		return nil
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	out := make([]openDaySnapshot, 0, len(rt.openDays))
	for day, p := range rt.openDays {
		if p == nil {
			if includePersisted {
				out = append(out, openDaySnapshot{day: day})
			}
			continue
		}
		out = append(out, openDaySnapshot{day: day, page: clonePage(p)})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].day < out[j].day
	})
	return out
}

func collectMetricFromFile(database, metric string, entry MetricEntry, path string, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	compressedBuf := pageCompressedBufferPool.Get().(*[]byte)
	defer pageCompressedBufferPool.Put(compressedBuf)

	var p Page
	for {
		header, compressed, crc, err := readCompressedPageFrame(r, compressedBuf)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		if header.EndTime < fromTS || header.StartTime > toTS {
			continue
		}

		if err := p.DecodeCompressedFrame(header, compressed, crc); err != nil {
			return fmt.Errorf("decode page: %w", err)
		}
		if err := collectMetricFromPage(database, metric, entry, &p, fromTS, toTS, stride, count, fn); err != nil {
			return err
		}
	}
}

func exportLinesFromFile(database string, db *Database, path string, w *bufio.Writer, wroteAny *bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	compressedBuf := pageCompressedBufferPool.Get().(*[]byte)
	defer pageCompressedBufferPool.Put(compressedBuf)

	var p Page
	for {
		header, compressed, crc, err := readCompressedPageFrame(r, compressedBuf)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := p.DecodeCompressedFrame(header, compressed, crc); err != nil {
			return fmt.Errorf("decode page: %w", err)
		}
		if err := exportLinesFromPage(database, db, &p, w, wroteAny); err != nil {
			return err
		}
	}
}

func readCompressedPageFrame(r *bufio.Reader, compressedBuf *[]byte) (PageHeader, []byte, uint32, error) {
	var headerBuf [HeaderSize]byte
	if _, err := io.ReadFull(r, headerBuf[:]); err != nil {
		if err == io.EOF {
			return PageHeader{}, nil, 0, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return PageHeader{}, nil, 0, fmt.Errorf("truncated frame header")
		}
		return PageHeader{}, nil, 0, err
	}

	header := PageHeader{
		StartTime:  Timestamp(binary.LittleEndian.Uint64(headerBuf[0:8])),
		EndTime:    Timestamp(binary.LittleEndian.Uint64(headerBuf[8:16])),
		NumRecords: binary.LittleEndian.Uint16(headerBuf[16:18]),
	}

	compressedLen, err := binary.ReadUvarint(r)
	if err != nil {
		if err == io.EOF {
			return PageHeader{}, nil, 0, fmt.Errorf("truncated frame length")
		}
		return PageHeader{}, nil, 0, err
	}

	if cap(*compressedBuf) < int(compressedLen) {
		*compressedBuf = make([]byte, int(compressedLen))
	}
	compressed := (*compressedBuf)[:int(compressedLen)]
	if _, err := io.ReadFull(r, compressed); err != nil {
		return PageHeader{}, nil, 0, fmt.Errorf("truncated compressed payload")
	}

	var crcBytes [4]byte
	if _, err := io.ReadFull(r, crcBytes[:]); err != nil {
		return PageHeader{}, nil, 0, fmt.Errorf("truncated frame checksum")
	}

	return header, compressed, binary.LittleEndian.Uint32(crcBytes[:]), nil
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
	if entry.ValueType == Int32Sample {
		sample := Sample{Database: database, Metric: metric, ValueType: Int32Sample}
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
			sample.TS = ts
			sample.Int32 = int32(binary.LittleEndian.Uint32(values[off : off+4]))
			if err := fn(sample); err != nil {
				return err
			}
		}
		return nil
	}

	sample := Sample{Database: database, Metric: metric, ValueType: Float32Sample}
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
		sample.TS = ts
		sample.Float32 = math.Float32frombits(binary.LittleEndian.Uint32(values[off : off+4]))
		if err := fn(sample); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) getOrCreateDB(database string) (*Database, *dbRuntime, error) {
	return e.getOrCreateDBWithDefaults(database, e.dbDefaults, false, 0)
}

func (e *Engine) getOrCreateDBWithDefaults(database string, defaults DBInfo, rollupManifest bool, rollupInterval time.Duration) (*Database, *dbRuntime, error) {
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
	info, err := loadOrCreateDBInfoWithOptions(db.RootDataDir, defaults, rollupManifest, rollupInterval)
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
	if info.WALEnabled {
		if err := e.replayWALIntoRuntime(db, rt, database); err != nil {
			e.recordWALReplayMetrics(database, replayRecords, replayBytes, false)
			_ = db.Close()
			return nil, nil, fmt.Errorf("replay wal for database %q: %w", database, err)
		}
	} else if db.wal != nil {
		if err := db.wal.Reset(); err != nil {
			e.recordWALReplayMetrics(database, replayRecords, replayBytes, false)
			_ = db.Close()
			return nil, nil, fmt.Errorf("reset disabled wal for database %q: %w", database, err)
		}
	}
	if database != internalStatsDatabase {
		e.recordWALReplayMetrics(database, replayRecords, replayBytes, true)
		e.captureWALStats(db, database)
		e.logInfo("database opened", "database", database, "wal_enabled", info.WALEnabled, "wal_replay_records", replayRecords, "wal_replay_bytes", replayBytes)
	}
	e.dbs[database] = db
	e.runtimes[database] = rt
	return db, rt, nil
}

func defaultRollupDestinationDBInfo(base DBInfo, interval time.Duration) DBInfo {
	info := base
	info.WALEnabled = false
	info.Rollups.Enabled = false
	info.Rollups.Jobs = nil

	if interval >= 24*time.Hour {
		info.Partition = "year"
		info.PageMaxAge = "168h"
	} else {
		info.Partition = "month"
		info.PageMaxAge = "6h"
	}

	return info
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
// Callers must already serialize writes through writeMu. Skips for the internal DB itself.
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
	e.captureRuntimeStats()
	e.flushStatsToInternal(Timestamp(now.UnixNano()))
}

// flushStatsToInternal writes the current engine stat snapshot through addParsedSample
// while the caller already holds writeMu, so it does not recurse through AddLine/AddSample.
func (e *Engine) flushStatsToInternal(ts Timestamp) {
	if !e.StatsEnabled {
		return
	}
	e.captureRuntimeStats()
	snap := e.stats.snapshot()
	if len(snap) == 0 {
		return
	}

	for k, v := range snap {
		metric := internalStatsMetricPrefix + "/" + k
		_ = e.addParsedSample(internalStatsDatabase, metric, ts, Float32Sample, 0, float32(v), false, false, false)
	}
}

func (e *Engine) captureRuntimeStats() {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	cacheStats := metricTimeFrameCacheStatsSnapshotV2()
	indexCacheStats := metricFileIndexCacheStatsSnapshotV2()
	e.stats.set("runtime/goroutines", float64(runtime.NumGoroutine()))
	e.stats.set("runtime/heap_alloc_bytes", float64(mem.HeapAlloc))
	e.stats.set("runtime/heap_inuse_bytes", float64(mem.HeapInuse))
	e.stats.set("runtime/heap_idle_bytes", float64(mem.HeapIdle))
	e.stats.set("runtime/heap_objects", float64(mem.HeapObjects))
	e.stats.set("runtime/stack_inuse_bytes", float64(mem.StackInuse))
	e.stats.set("runtime/sys_bytes", float64(mem.Sys))
	e.stats.set("runtime/next_gc_bytes", float64(mem.NextGC))
	e.stats.set("runtime/num_gc", float64(mem.NumGC))
	e.stats.set("runtime/last_gc_unix_ns", float64(mem.LastGC))
	e.stats.set("metric_file/time_cache_entries", float64(cacheStats.Entries))
	e.stats.set("metric_file/time_cache_bytes", float64(cacheStats.Bytes))
	e.stats.set("metric_file/time_cache_max_entries", float64(cacheStats.MaxEntries))
	e.stats.set("metric_file/time_cache_hits", float64(cacheStats.Hits))
	e.stats.set("metric_file/time_cache_misses", float64(cacheStats.Misses))
	e.stats.set("metric_file/time_cache_evictions", float64(cacheStats.Evictions))
	e.stats.set("metric_file/index_cache_entries", float64(indexCacheStats.Entries))
	e.stats.set("metric_file/index_cache_bytes", float64(indexCacheStats.Bytes))
	e.stats.set("metric_file/index_cache_max_entries", float64(indexCacheStats.MaxEntries))
	e.stats.set("metric_file/index_cache_hits", float64(indexCacheStats.Hits))
	e.stats.set("metric_file/index_cache_misses", float64(indexCacheStats.Misses))
	e.stats.set("metric_file/index_cache_evictions", float64(indexCacheStats.Evictions))
	if mem.NumGC > 0 {
		idx := (mem.NumGC - 1) % uint32(len(mem.PauseNs))
		e.stats.set("runtime/last_gc_pause_ns", float64(mem.PauseNs[idx]))
	}
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
		if err := e.ensureDayOpen(db, rt, dbName, day, rec.Timestamp, false); err != nil {
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

func (e *Engine) ensureDayOpen(db *Database, rt *dbRuntime, dbName, day string, ts Timestamp, triggerRollups bool) error {
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
		if err := sealDay(e, db, rt, dbName, oldest, ts, triggerRollups); err != nil {
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

func sealDay(e *Engine, db *Database, rt *dbRuntime, dbName, day string, nowTS Timestamp, triggerRollups bool) error {
	if p := rt.openDays[day]; p != nil {
		if err := e.writePageToDailyFile(db, dbName, day, p); err != nil {
			return err
		}
	}
	delete(rt.openDays, day)
	if triggerRollups && e != nil && e.rollupAuto.Load() {
		e.triggerRollups(dbName)
	}
	if db.wal != nil && db.wal.Stats().BufferBytes > 0 {
		e.logDebug("wal reset", "database", dbName, "buffer_bytes", db.wal.Stats().BufferBytes)
	}
	if err := maybeResetWAL(db, rt); err != nil {
		return err
	}
	e.captureWALStats(db, dbName)
	if e != nil && e.AutoCreateMetricFiles {
		if _, err := e.buildMetricFileFromSealedPartition(db, rt, day); err != nil {
			e.logInfo("metric file auto-build failed", "database", dbName, "partition", day, "error", err)
		}
	}
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
	e.logDebug("page flushed", "database", dbName, "day", day, "records", len(page.Times), "frame_bytes", st.FrameBytes, "compressed_bytes", st.CompressedBytes)
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
	if strings.TrimSpace(info.Rollups.DefaultInterval) == "" {
		info.Rollups.DefaultInterval = strings.TrimSpace(def.Rollups.DefaultInterval)
	}
	if strings.TrimSpace(info.Rollups.DefaultDestinationDB) == "" {
		info.Rollups.DefaultDestinationDB = strings.TrimSpace(def.Rollups.DefaultDestinationDB)
	}
	if len(info.Rollups.DefaultAggregates) == 0 {
		info.Rollups.DefaultAggregates = append([]string(nil), def.Rollups.DefaultAggregates...)
	}
	if len(info.Rollups.GlobalExcludePatterns) == 0 {
		info.Rollups.GlobalExcludePatterns = append([]string(nil), def.Rollups.GlobalExcludePatterns...)
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
	if strings.TrimSpace(info.Rollups.DefaultInterval) != "" {
		if _, err := time.ParseDuration(info.Rollups.DefaultInterval); err != nil {
			return DBInfo{}, fmt.Errorf("invalid rollups.default_interval: %w", err)
		}
	}
	for i, pat := range info.Rollups.GlobalExcludePatterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if _, err := path.Match(pat, "metric"); err != nil {
			return DBInfo{}, fmt.Errorf("invalid rollups.global_exclude_patterns[%d]: %w", i, err)
		}
		info.Rollups.GlobalExcludePatterns[i] = pat
	}
	for idx := range info.Rollups.Jobs {
		job := &info.Rollups.Jobs[idx]
		job.ID = strings.TrimSpace(job.ID)
		job.SourceMetric = strings.TrimSpace(job.SourceMetric)
		job.SourcePattern = strings.TrimSpace(job.SourcePattern)
		job.Interval = strings.TrimSpace(job.Interval)
		job.DestinationDB = strings.TrimSpace(job.DestinationDB)
		job.DestinationMetricPrefix = strings.TrimSpace(job.DestinationMetricPrefix)
		job.Grace = strings.TrimSpace(job.Grace)
		if job.ID == "" {
			return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].id: empty", idx)
		}
		hasSourceMetric := job.SourceMetric != ""
		hasSourcePattern := job.SourcePattern != ""
		if hasSourceMetric == hasSourcePattern {
			return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d]: exactly one of source_metric or source_pattern must be set", idx)
		}
		if hasSourcePattern {
			if _, err := path.Match(job.SourcePattern, "metric"); err != nil {
				return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].source_pattern: %w", idx, err)
			}
		}

		for patIdx, pat := range job.ExcludePatterns {
			pat = strings.TrimSpace(pat)
			if pat == "" {
				continue
			}
			if _, err := path.Match(pat, "metric"); err != nil {
				return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].exclude_patterns[%d]: %w", idx, patIdx, err)
			}
			job.ExcludePatterns[patIdx] = pat
		}

		if job.Interval == "" {
			job.Interval = strings.TrimSpace(info.Rollups.DefaultInterval)
		}
		if job.Interval == "" {
			return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].interval: empty", idx)
		}
		if _, err := time.ParseDuration(job.Interval); err != nil {
			return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].interval: %w", idx, err)
		}
		if job.DestinationDB == "" {
			job.DestinationDB = strings.TrimSpace(info.Rollups.DefaultDestinationDB)
		}
		if job.DestinationDB == "" {
			return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].destination_db: empty", idx)
		}
		if job.DestinationMetricPrefix == "" {
			if job.SourceMetric != "" {
				job.DestinationMetricPrefix = job.SourceMetric
			}
		}
		if job.Grace != "" {
			if _, err := time.ParseDuration(job.Grace); err != nil {
				return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].grace: %w", idx, err)
			}
		}
		if len(job.Aggregates) == 0 {
			if len(info.Rollups.DefaultAggregates) > 0 {
				job.Aggregates = append([]string(nil), info.Rollups.DefaultAggregates...)
			} else {
				job.Aggregates = defaultRollupAggregates()
			}
		}
		for aggIdx, agg := range job.Aggregates {
			agg = strings.TrimSpace(strings.ToLower(agg))
			if !isSupportedAggregate(agg) {
				return DBInfo{}, fmt.Errorf("invalid rollups.jobs[%d].aggregates[%d]: %q (supported: %s)", idx, aggIdx, job.Aggregates[aggIdx], strings.Join(supportedAggregates(), ","))
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

func writeRollupDBInfoTOML(path string, info DBInfo, interval time.Duration) error {
	buf := bytes.NewBuffer(nil)
	partitionWhy := "month"
	pageAgeWhy := "6h"
	if interval >= 24*time.Hour {
		partitionWhy = "year"
		pageAgeWhy = "168h"
	}
	fmt.Fprintf(buf, "# Auto-created rollup destination manifest.\n")
	fmt.Fprintf(buf, "# WAL is disabled because rollup data is derived from source metrics and can be rebuilt.\n")
	fmt.Fprintf(buf, "# partition = \"%s\" keeps sparse rollup outputs out of many tiny daily files.\n", partitionWhy)
	fmt.Fprintf(buf, "# page.max_age = \"%s\" allows more derived samples to accumulate before flush, reducing tiny pages.\n\n", pageAgeWhy)
	if err := toml.NewEncoder(buf).Encode(dbManifestFromInfo(info)); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}

func loadExistingDBInfo(root string, defaults DBInfo) (DBInfo, bool, error) {
	path := filepath.Join(root, manifestFileName)
	raw, err := os.ReadFile(path)
	if err == nil {
		manifest := DBManifestTOML{}
		if _, err := toml.Decode(string(raw), &manifest); err != nil {
			return DBInfo{}, false, fmt.Errorf("parse %s: %w", path, err)
		}
		info := dbInfoFromManifest(manifest)
		info, err = normalizeDBInfo(info, defaults)
		if err != nil {
			return DBInfo{}, false, fmt.Errorf("invalid %s: %w", path, err)
		}
		return info, true, nil
	}
	if !os.IsNotExist(err) {
		return DBInfo{}, false, err
	}
	return DBInfo{}, false, nil
}

func loadOrCreateDBInfoWithOptions(root string, defaults DBInfo, rollupManifest bool, rollupInterval time.Duration) (DBInfo, error) {
	path := filepath.Join(root, manifestFileName)
	if info, exists, err := loadExistingDBInfo(root, defaults); err != nil {
		return DBInfo{}, err
	} else if exists {
		if rollupManifest {
			if err := writeRollupDBInfoTOML(path, info, rollupInterval); err != nil {
				return DBInfo{}, err
			}
		}
		return info, nil
	}

	info, err := normalizeDBInfo(defaults, defaults)
	if err != nil {
		return DBInfo{}, err
	}
	if rollupManifest {
		if err := writeRollupDBInfoTOML(path, info, rollupInterval); err != nil {
			return DBInfo{}, err
		}
	} else {
		if err := writeDBInfoTOML(path, info); err != nil {
			return DBInfo{}, err
		}
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
