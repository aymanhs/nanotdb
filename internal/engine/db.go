package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

// MaxMetricsPerDatabase is the hard limit on distinct metrics per database (uint16 address space).
const MaxMetricsPerDatabase = 65535

// MetricID is a compact uint16 identifier assigned to each metric within a database.
type MetricID uint16

// Timestamp is a Unix nanosecond epoch value (int64). All sample timestamps
// must be monotonically non-decreasing within a metric.
type Timestamp int64

var ErrTooManyMetrics = fmt.Errorf("too many metrics in database")

// SampleType constrains the numeric types supported as metric values.
type SampleType = interface{ int32 | float32 }

const (
	Int32Sample   byte = 1
	Float32Sample byte = 2
)

// Database represents one named time-series database on disk.
// Each database has its own WAL, catalog, and set of daily data files.
// When events are enabled (via [events] in the manifest), it also has
// its own events catalog, events WAL, and set of events-<partition>.dat
// files — independent of the metric storage layer.
// Use Engine.AddLine / Engine.QueryRange / Engine.AddEvent / Engine.QueryEvents
// for typical access; Database is exposed for low-level tooling (nanocli
// inspect, export, etc.).
type Database struct {
	Name        string
	RootDataDir string
	wal         *WAL
	catalog     *Catalog

	// Events layer. Both nil when [events].enabled is false for this DB.
	// Opened together (or not at all) by NewDatabaseWithWALConfig / the
	// engine's open path when the manifest opts in.
	eventsWAL    *EventsWAL
	eventCatalog *EventCatalog

	page *Page // single active page for all metrics
}

type DataRuntimeStats struct {
	FlushCount           int64
	TotalFlushBytes      int64
	TotalFlushRecords    int64
	TotalFlushCompressed int64
	MinFlushBytes        int64
	MaxFlushBytes        int64
	MinFlushRecords      int64
	MaxFlushRecords      int64
	MinFlushCompressed   int64
	MaxFlushCompressed   int64
	FlushDurationTotal   time.Duration
	MinFlushDuration     time.Duration
	MaxFlushDuration     time.Duration
	SyncCount            int64
	SyncDurationTotal    time.Duration
	MinSyncDuration      time.Duration
	MaxSyncDuration      time.Duration
}

type DBStats struct {
	DataFile DataRuntimeStats
	WAL      WALStats
}

func resolveDBPaths(name string) (rootDir, baseName, catPath, walPath string) {
	rootDir = filepath.Dir(name)
	if rootDir == "" {
		rootDir = "."
	}
	baseName = filepath.Base(name)
	catPath = filepath.Join(rootDir, "catalog.json")
	walPath = filepath.Join(rootDir, baseName+".wal")
	return
}

// resolveEventsPaths returns the two file paths used by the events layer
// for a database. eventsCatalogPath is the events.json sibling of
// catalog.json; eventsWALPath is the <db>.events.wal sibling of <db>.wal.
func resolveEventsPaths(rootDir, baseName string) (eventsCatalogPath, eventsWALPath string) {
	eventsCatalogPath = filepath.Join(rootDir, "events.json")
	eventsWALPath = filepath.Join(rootDir, baseName+".events.wal")
	return
}

// OpenEventsForDatabase opens (creating if missing) the events catalog
// and events WAL files for db, attaching them to the receiver. Idempotent:
// calling on a database whose events resources are already open is a
// no-op. Mirrors NewDatabaseWithWALConfig's WAL+catalog setup pattern but
// is split out because events are an opt-in lifecycle the engine drives
// only when the manifest's [events].enabled is true.
func (db *Database) OpenEventsForDatabase(walMaxSegSize int64, fsyncPolicy string) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	if db.eventCatalog != nil && db.eventsWAL != nil {
		return nil
	}
	if db.RootDataDir == "" || db.Name == "" {
		return fmt.Errorf("database paths not initialized")
	}
	catPath, walPath := resolveEventsPaths(db.RootDataDir, db.Name)

	if db.eventCatalog == nil {
		cat, err := LoadEventCatalog(catPath)
		if err != nil {
			return fmt.Errorf("open events catalog: %w", err)
		}
		db.eventCatalog = cat
	}
	if db.eventsWAL == nil {
		wal, err := OpenAndRecoverEventsWAL(walPath, fsyncPolicy)
		if err != nil {
			// If we successfully opened the catalog this call but the WAL
			// failed, release the catalog so we don't leak the fd.
			if db.eventCatalog != nil {
				_ = db.eventCatalog.Close()
				db.eventCatalog = nil
			}
			return fmt.Errorf("open events wal: %w", err)
		}
		wal.maxSegSize = walMaxSegSize
		db.eventsWAL = wal
	}
	return nil
}

// OpenDatabase opens an existing database by base path (no WAL size limit).
// Used by read-only tooling that does not need to append samples.
func OpenDatabase(name string) (*Database, error) {
	return OpenDatabaseWithWALConfig(name, WALFsyncPolicySegment)
}

func OpenDatabaseWithWALConfig(name string, fsyncPolicy string) (*Database, error) {
	rootDir, baseName, catPath, walPath := resolveDBPaths(name)

	db := &Database{
		Name:        baseName,
		RootDataDir: rootDir,
	}

	var err error
	db.catalog, err = LoadCatalog(catPath)
	if err != nil {
		return nil, err
	}

	db.wal, err = OpenAndRecoverWAL(walPath, fsyncPolicy)
	if err != nil {
		return nil, err
	}

	return db, nil
}

// NewDatabase creates a new database at the given base path with the specified WAL segment size.
func NewDatabase(name string, walMaxSegSize int64) (*Database, error) {
	return NewDatabaseWithWALConfig(name, walMaxSegSize, WALFsyncPolicySegment)
}

func NewDatabaseWithWALConfig(name string, walMaxSegSize int64, fsyncPolicy string) (*Database, error) {
	rootDir, baseName, catPath, walPath := resolveDBPaths(name)
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, err
	}

	var err error
	wal, err := NewWAL(walPath, walMaxSegSize, fsyncPolicy)
	if err != nil {
		return nil, err
	}
	catalog, err := LoadCatalog(catPath)
	if err != nil {
		return nil, err
	}

	return &Database{
		Name:        baseName,
		RootDataDir: rootDir,
		wal:         wal,
		catalog:     catalog,
	}, nil
}

func (db *Database) Close() error {
	var errs []error
	if db.wal != nil {
		if err := db.wal.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close metric wal: %w", err))
		}
	}
	if db.eventsWAL != nil {
		if err := db.eventsWAL.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close events wal: %w", err))
		}
	}
	if db.catalog != nil && db.catalog.file != nil {
		if err := db.catalog.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close metric catalog: %w", err))
		}
	}
	if db.eventCatalog != nil {
		if err := db.eventCatalog.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close events catalog: %w", err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func addSampleToDB[T SampleType](db *Database, name string, ts Timestamp, value T) (err error) {
	_, exists := db.catalog.GetMetricEntry(name)
	metricID, err := GetMetricID[T](db.catalog, name)
	if err != nil {
		return err
	}

	if db.page == nil {
		db.page = NewPage(ts)
	}
	if len(db.page.Times) > 0 && ts < db.page.Times[len(db.page.Times)-1] {
		return ErrOutOfOrderTimestamp
	}

	var raw [4]byte
	switch v := any(value).(type) {
	case int32:
		binary.LittleEndian.PutUint32(raw[:], uint32(v))
	case float32:
		binary.LittleEndian.PutUint32(raw[:], math.Float32bits(v))
	default:
		panic("unsupported sample type")
	}

	// Append to WAL first (LAW 1 — WAL Before Memory)
	if db.wal == nil {
		return fmt.Errorf("wal is not initialized")
	}

	var walSegment uint16
	if !exists {
		walSegment, err = AppendSampleWithMetricName(db.wal, metricID, name, ts, value)
	} else {
		walSegment, err = AppendSample(db.wal, metricID, ts, value)
	}
	if err != nil {
		return err
	}

	db.page.SetWalSegmentID(walSegment)
	if err = db.page.AddSample(metricID, ts, raw[:]); err != nil {
		return err
	}
	if err = db.catalog.UpdateLastByMetricID(metricID, ts, raw[:]); err != nil {
		return err
	}

	return nil
}
