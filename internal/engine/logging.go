package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

const TraceSlogLevel slog.Level = slog.LevelDebug - 4

type multiHandler struct {
	handlers []slog.Handler
}

func (h multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, child := range h.handlers {
		if child.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h multiHandler) Handle(ctx context.Context, rec slog.Record) error {
	var errs []error
	for _, child := range h.handlers {
		if !child.Enabled(ctx, rec.Level) {
			continue
		}
		if err := child.Handle(ctx, rec.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	children := make([]slog.Handler, 0, len(h.handlers))
	for _, child := range h.handlers {
		children = append(children, child.WithAttrs(attrs))
	}
	return multiHandler{handlers: children}
}

func (h multiHandler) WithGroup(name string) slog.Handler {
	children := make([]slog.Handler, 0, len(h.handlers))
	for _, child := range h.handlers {
		children = append(children, child.WithGroup(name))
	}
	return multiHandler{handlers: children}
}

func LoadEngineConfig(rootDataDir string, fallbackWalMaxSegSize int64) (EngineConfig, time.Duration, DBInfo, error) {
	return loadOrCreateEngineConfig(rootDataDir, fallbackWalMaxSegSize)
}

func OpenEngineWithConfig(rootDataDir string, cfg EngineConfig, statsInterval time.Duration, dbDefaults DBInfo, logger *slog.Logger) (*Engine, error) {
	if err := os.MkdirAll(rootDataDir, 0755); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = defaultEngineLogger()
	}
	syncData, syncCatalog := durabilitySyncPolicy(cfg.Durability.Profile)
	e := &Engine{
		RootDataDir:           rootDataDir,
		WALMaxSegSize:         cfg.WAL.MaxSegmentSize,
		WALFsyncPolicy:        cfg.WAL.FsyncPolicy,
		Durability:            cfg.Durability.Profile,
		PreferMetricFiles:     true,
		AutoCreateMetricFiles: cfg.Metrics.Enabled,
		MetricFileCompression: cfg.Metrics.Compression,
		MetricRawIngestAction: cfg.Metrics.RawIngestAction,
		MetricTimeCacheSlots:  cfg.Metrics.TimeCacheSlots,
		Logging:               cfg.Logging,
		logger:                logger,
		SyncDataFile:          syncData,
		SyncCatalog:           syncCatalog,
		StatsEnabled:          cfg.Stats.Enabled,
		StatsInterval:         statsInterval,
		dbDefaults:            dbDefaults,
		dbs:                   make(map[string]*Database),
		runtimes:              make(map[string]*dbRuntime),
		stats:                 newEngineStatStore(),
	}
	configureMetricTimeFrameCacheSlotsV2(cfg.Metrics.TimeCacheSlots)
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
	e.logInfo("engine opened", "data_dir", rootDataDir, "stats_enabled", cfg.Stats.Enabled, "durability", cfg.Durability.Profile, "metric_auto_create", cfg.Metrics.Enabled, "prefer_metric_files", true, "metric_file_compression", cfg.Metrics.Compression, "metric_raw_ingest_action", cfg.Metrics.RawIngestAction, "metric_time_cache_slots", cfg.Metrics.TimeCacheSlots)
	return e, nil
}

func NewLogger(cfg EngineConfigLogging) (*slog.Logger, func() error, error) {
	handlers := make([]slog.Handler, 0, len(cfg.Loggers))
	closers := make([]func() error, 0, len(cfg.Loggers))

	for i, entry := range cfg.Loggers {
		level, err := slogLevelForName(entry.Level)
		if err != nil {
			return nil, nil, fmt.Errorf("logger %d: %w", i, err)
		}
		writer, closer, err := openLogOutput(entry.Output)
		if err != nil {
			return nil, nil, fmt.Errorf("logger %d: %w", i, err)
		}
		if closer != nil {
			closers = append(closers, closer)
		}
		handlers = append(handlers, slog.NewTextHandler(writer, &slog.HandlerOptions{
			Level: level,
			ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
				if attr.Key == slog.LevelKey {
					if lv, ok := attr.Value.Any().(slog.Level); ok {
						attr.Value = slog.StringValue(slogLevelName(lv))
					}
				}
				return attr
			},
		}))
	}

	if len(handlers) == 0 {
		return defaultEngineLogger(), func() error { return nil }, nil
	}

	closeFn := func() error {
		var errs []error
		for i := len(closers) - 1; i >= 0; i-- {
			if err := closers[i](); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	return slog.New(multiHandler{handlers: handlers}), closeFn, nil
}

func defaultEngineLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func slogLevelForName(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case LogLevelInfo:
		return slog.LevelInfo, nil
	case LogLevelDebug:
		return slog.LevelDebug, nil
	case LogLevelTrace:
		return TraceSlogLevel, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", name)
	}
}

func slogLevelName(level slog.Level) string {
	switch {
	case level <= TraceSlogLevel:
		return strings.ToUpper(LogLevelTrace)
	case level <= slog.LevelDebug:
		return strings.ToUpper(LogLevelDebug)
	default:
		return strings.ToUpper(LogLevelInfo)
	}
}

func openLogOutput(output string) (*os.File, func() error, error) {
	output = strings.TrimSpace(output)
	if output == "console" {
		return os.Stderr, nil, nil
	}
	f, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

func (e *Engine) logInfo(msg string, args ...any) {
	if e != nil && e.logger != nil {
		e.logger.Info(msg, args...)
	}
}

func (e *Engine) logDebug(msg string, args ...any) {
	if e != nil && e.logger != nil {
		e.logger.Debug(msg, args...)
	}
}

func (e *Engine) logTrace(msg string, args ...any) {
	if e != nil && e.logger != nil {
		e.logger.Log(context.Background(), TraceSlogLevel, msg, args...)
	}
}
