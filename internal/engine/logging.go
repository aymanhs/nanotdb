package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		writer, closer, err := openLogOutput(entry)
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

func openLogOutput(entry EngineConfigLogger) (io.Writer, func() error, error) {
	output := strings.TrimSpace(entry.Output)
	if output == "console" {
		return os.Stderr, nil, nil
	}
	if entry.MaxFileBytes > 0 {
		maxBackups := entry.MaxBackups
		if maxBackups <= 0 {
			maxBackups = 5
		}
		w, err := newRotatingFileWriter(output, entry.MaxFileBytes, maxBackups)
		if err != nil {
			return nil, nil, err
		}
		return w, w.Close, nil
	}
	f, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

type rotatingFileWriter struct {
	mu         sync.Mutex
	path       string
	file       *os.File
	size       int64
	maxBytes   int64
	maxBackups int
}

func newRotatingFileWriter(path string, maxBytes int64, maxBackups int) (*rotatingFileWriter, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("log output path cannot be empty")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("max_file_bytes must be > 0")
	}
	if maxBackups < 0 {
		return nil, fmt.Errorf("max_backups must be >= 0")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rotatingFileWriter{
		path:       path,
		file:       f,
		size:       st.Size(),
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
	}, nil
}

func (w *rotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return 0, fmt.Errorf("log writer is closed")
	}

	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *rotatingFileWriter) rotateLocked() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}

	if w.maxBackups > 0 {
		oldest := fmt.Sprintf("%s.%d", w.path, w.maxBackups)
		_ = os.Remove(oldest)
		for i := w.maxBackups - 1; i >= 1; i-- {
			src := fmt.Sprintf("%s.%d", w.path, i)
			dst := fmt.Sprintf("%s.%d", w.path, i+1)
			if _, err := os.Stat(src); err == nil {
				if err := os.Rename(src, dst); err != nil {
					return err
				}
			}
		}
		if _, err := os.Stat(w.path); err == nil {
			if err := os.Rename(w.path, fmt.Sprintf("%s.1", w.path)); err != nil {
				return err
			}
		}
	} else {
		_ = os.Remove(w.path)
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	w.file = f
	w.size = 0
	return nil
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
