# NanoTDB - Design Summary

NanoTDB is a small, append-only, embedded time-series database designed for:
- Raspberry Pi / SD card environments
- low cardinality (typically hundreds of metrics, not tens of thousands)
- irregular write-heavy patterns
- simple durability and correctness guarantees
- easy reasoning and crash safety

This document summarizes the architecture decisions for v0.

---

## Core Characteristics

- **Append-only on disk**
  - No updates
  - No in-place modification of persisted page frames
  - Once a page frame is durable, it is immutable

- **Configurable UTC-partitioned raw storage**
  - Raw data is grouped by configured partition file (`data-<partition>.dat`)
  - Partition mode is per-database via manifest: `day|month|year|forever`
  - Retention is enforced by removing old partition files

- **No separate index file**
  - Queries scan day files directly; the data files are self-describing and index-free
  - Partial lookups use page headers to avoid unnecessary decompression
  - No sidecar index file is maintained, now or in the future
  - Rationale: lower write-path complexity and fast enough query behavior for day-file scans at target scale

- **Reactive**
  - The DB does not collect data on its own
  - Writes occur only on explicit insert requests

- **Ordered inserts per metric**
  - Inserts are time-ordered per metric stream
  - Out-of-order inserts are rejected
  - Equal timestamps are valid and preserve append order

---

## Terminology

- **Metric**
  - A metric is a monotonic, time-ordered stream of numeric samples
  - One metric represents exactly one numeric value over time
  - Metrics are independent; no multi-field points exist
  - Metric value type is fixed at metric creation (`int32` or `float32`)

- **Metric identity model**
  - API uses string metric identifiers (for example: `cpu_temperature`)
  - Storage uses a 2-byte internal `MetricID` (`uint16`)
  - Metric string -> `MetricID` mapping is persisted per database
  - Source metric ids are constrained to `1..1023` (10-bit base metric space)
  - Unknown metric strings are auto-assigned a new source metric id in `1..1023`
  - If the source metric id space is exhausted, metric creation is rejected
  - `MetricID`s are never deleted and never reused

- **Page frame**
  - A variable-length on-disk record in a raw metric-day `.dat` file
  - Consists of a fixed header and one compressed block of points
  - Serves as the unit of scanning, filtering, and decompression

---

## Storage Layout

Per database root:

```text
<db>/
  catalog.json
  manifest.json
  data-2026-05.dat
  data-2026.dat
  <db>.wal
```

### Raw data file (`data-<partition>.dat`)
- Append-only stream of variable-length page frames
- Each frame may contain records from multiple metrics
- Each frame header includes enough metadata for header-only filtering
- Each frame payload is one independent compressed block
- No frame depends on another frame for decompression

Recommended frame metadata fields:
- format `version`
- `metric_id`
- `start_time`, `end_time`
- `point_count`
- `codec_id`
- `compressed_len` (and optionally uncompressed length)
- integrity checks (header and/or payload checksum)

### Catalog file
- Database-scoped catalog store
- Persists metric identifier string -> source metric id mapping
- Persists fixed metric value type per metric
- Source metric ids are auto-assigned in `1..1023`, never deleted, never reused

### Manifest file
- Database-scoped operational metadata
- Stores settings such as retention, partitioning, active-day limits, and rollups
- Example fields:
  - `retention_days`: how long to keep historical data
  - `max_active_days`: maximum number of simultaneous open partition files (default 2)
  - `partition`: partition mode (`day|month|year|forever`)
  - `grace`: grace period for out-of-order sample tolerance (v0 placeholder)
  - `rollups`: source-defined rollup jobs + checkpoint settings, including selector-based jobs with wildcard exclusions and per-DB defaults
- Auto-created rollup destination manifests are specialized from normal DB defaults: WAL disabled, coarser partitions (`month` for sub-daily, `year` for daily-or-larger), and longer page age to reduce sparse tiny files.

---

## Write Path Overview (v0)

1. Insert request arrives
2. API metric string is resolved to `MetricID` via catalog
3. Unknown metric strings are created in catalog with a new `MetricID`
4. Value type must match metric type; mismatched writes are rejected
5. If the metric id space is exhausted, insert fails by rejection
6. Target UTC partition file (`data-<partition>.dat`) is selected from sample timestamp + partition mode
7. Sample is appended to the active in-memory page buffer for that database-partition stream
8. On page seal (size/time policy), write one page frame to the selected partition `.dat` file
9. Optional batching/fsync policy is applied

Out-of-order inserts for a metric are rejected in v0.

---

## WAL Status

- WAL is active and used for crash recovery
- Startup replays WAL records into in-memory open day pages
- WAL is reset only after page data is flushed and no open day pages remain
- If restart is quick and unflushed data remains, WAL stays on disk and replay keeps pages open in memory until normal flush/reset flow
- Engine emits WAL replay metrics:
  - `nanotdb/{db}/wal/replay_records`
  - `nanotdb/{db}/wal/replay_bytes`
  - `nanotdb/{db}/wal/replay_success_count`
  - `nanotdb/{db}/wal/replay_error_count`

---

## Durability & Acknowledgment (v0)

- v0 accepts potential data loss after acknowledgment
- Acknowledged writes may be lost after process or OS crash depending on flush/fsync timing
- The design goal is simplicity first; stronger durability can be added later

---

## Query Model

### Range Queries (Callback-Driven Streaming)

- Range queries accept a callback function instead of buffering results
- Callback is invoked once per matching sample; caller controls buffering/output
- **Stride parameter** enables downsampling: stride=1 (every sample), stride=N (every Nth sample)
- Example use cases:
  - Stride=1: exact data export
  - Stride=288: visualize full day in ~300 samples (pixel-friendly downsampling)
  - Stride > 1: reduce memory and I/O for dashboards/analytics

Benefits:
- Unbounded result sets never allocate large slices
- Callbacks can write JSON-lines to sockets or file streams in real-time
- Downsampling happens during scan, not post-query

### Query Patterns

- Full-day scans are the dominant query pattern
- Day-file scan cost is expected to be negligible for target workloads
- For narrower lookups, all matching frames are decompressed (no index-based skipping in v0)

---

## Engine API (v0)

### Ingest

**AddLine(line string)** – Parse and ingest a single line in line protocol format:
- Format: `database/metric value [timestamp]`
- Value: int32 or float32 (parsed and stored as is)
- Timestamp: optional Unix nanoseconds; defaults to current time if omitted
- Metric string determines value type on first write; subsequent writes to same metric must match
- Out-of-order samples for a metric are rejected (must be >= last timestamp)

**ImportFile(path string)** – Bulk ingest from a file with one line-protocol line per line

### Query

**QueryLast(database, metric string) → (Sample, bool, error)** – Return the last known sample for a metric
- Metadata is resolved from catalog state
- On startup, WAL replay updates catalog last-values for recovered samples

**QueryRange(database, metric string, fromTS, toTS Timestamp, stride int, callback SampleCallback) → error** – Streaming range query
- stride=1: every sample (default)
- stride=N: every Nth sample for downsampling
- Callback is invoked once per matching sample
- Callback can return error to terminate scan early
- Results span active pages and all historical partition files in range

### Export/Import

**ExportFile(database, outPath string)** – Write all samples for a database to line protocol file

---

Recovery is deterministic and per database:
1. Load source metric catalog (source metric ids and fixed metric types)
2. For each raw `.dat`, sequentially validate frame boundaries/integrity
3. Truncate invalid trailing tail if needed (crash-tail handling)
4. Resume appends from the last valid frame

---

## Operational Notes

- Raw retention: remove old `data-<partition>.dat` files
- Deleting a day while files are open is implementation-defined and must be handled carefully
- This architecture assumes low cardinality (hundreds of metrics). If cardinality increases significantly, file-count and scan costs should be re-evaluated

---

## Engine Config Mapping (`engine.toml`)

This maps config keys to runtime fields in `internal/engine/engine.go`.

| TOML key | Config struct field | Runtime field / effect | Notes |
|---|---|---|---|
| `engine.listen` | `EngineConfig.Engine.Listen` | Used by CLI runtime loader as server bind address | Default `:8428` if empty |
| `wal.max_segment_size` | `EngineConfig.WAL.MaxSegmentSize` | `Engine.WALMaxSegSize` | If `<= 0`, falls back to default (`64 MiB`) |
| `wal.fsync_policy` | `EngineConfig.WAL.FsyncPolicy` | `Engine.WALFsyncPolicy` | Valid values: `segment`, `always` |
| `durability.profile` | `EngineConfig.Durability.Profile` | `Engine.Durability`, plus sync policy | Valid values: `strict`, `balanced`, `throughput` |
| `stats.enabled` | `EngineConfig.Stats.Enabled` | `Engine.StatsEnabled` | Enables internal metrics emission |
| `stats.interval` | `EngineConfig.Stats.Interval` | `Engine.StatsInterval` | Parsed with Go duration (`time.ParseDuration`) |
| `defaults.databases` | `EngineConfig.Defaults.Databases` | Startup DB auto-creation list | Empty names and `internal` are ignored |
| `manifest_defaults.retention.grace` | `EngineConfig.ManifestDefaults.Retention.Grace` | Copied into new DB manifest via DB defaults | Duration string; validated |
| `manifest_defaults.retention.retention_days` | `EngineConfig.ManifestDefaults.Retention.RetentionDays` | Per-DB partition-file retention policy | If `<= 0`, default is applied |
| `manifest_defaults.retention.max_active_days` | `EngineConfig.ManifestDefaults.Retention.MaxActiveDays` | Per-DB open-partition memory window | If `<= 0`, default is applied |
| `manifest_defaults.retention.partition` | `EngineConfig.ManifestDefaults.Retention.Partition` | Per-DB partition mode (`day|month|year|forever`) | If empty, default `day` is applied |
| `manifest_defaults.wal.enabled` | `EngineConfig.ManifestDefaults.WAL.Enabled` | Per-DB WAL enable flag for new DB manifests | Copied only when DB is created |
| `manifest_defaults.wal.skip_before` | `EngineConfig.ManifestDefaults.WAL.SkipBefore` | Per-DB WAL backfill skip window | Duration string; validated |
| `manifest_defaults.page.max_records` | `EngineConfig.ManifestDefaults.Page.MaxRecords` | Per-DB page flush threshold (records) | If `<= 0`, default is applied |
| `manifest_defaults.page.max_bytes` | `EngineConfig.ManifestDefaults.Page.MaxBytes` | Per-DB page flush threshold (bytes) | If `<= 0`, default is applied |
| `manifest_defaults.page.max_age` | `EngineConfig.ManifestDefaults.Page.MaxAge` | Per-DB page rollover age | Duration string; validated |
| `manifest_defaults.rollups.enabled` | `EngineConfig.ManifestDefaults.Rollups.Enabled` | Per-DB rollups toggle | Copied only when DB is created |
| `manifest_defaults.rollups.checkpoint_file` | `EngineConfig.ManifestDefaults.Rollups.CheckpointFile` | Per-DB source checkpoint log file | Defaults to `rollup.checkpoints.log` |
| `manifest_defaults.rollups.default_grace` | `EngineConfig.ManifestDefaults.Rollups.DefaultGrace` | Per-DB default rollup grace | Duration string or empty |
| `manifest_defaults.rollups.default_interval` | `EngineConfig.ManifestDefaults.Rollups.DefaultInterval` | Per-DB default rollup interval | Duration string or empty |
| `manifest_defaults.rollups.default_destination_db` | `EngineConfig.ManifestDefaults.Rollups.DefaultDestinationDB` | Per-DB default rollup target DB | Empty means per-job required |
| `manifest_defaults.rollups.default_aggregates` | `EngineConfig.ManifestDefaults.Rollups.DefaultAggregates` | Per-DB default rollup aggregate list | Subset of `min|max|sum|avg|count` |
| `manifest_defaults.rollups.global_exclude_patterns` | `EngineConfig.ManifestDefaults.Rollups.GlobalExcludePatterns` | Per-DB wildcard exclusions for selector jobs | Applied before job-specific exclusions |

Durability profile to runtime sync behavior:

| `durability.profile` | `Engine.SyncDataFile` | `Engine.SyncCatalog` |
|---|---|---|
| `strict` | `true` | `true` |
| `balanced` | `true` | `false` |
| `throughput` | `false` | `false` |

Notes:
- `default_engine.toml` is embedded (`//go:embed`) and written to `<root_data_dir>/engine.toml` when missing.
- Existing per-database manifests are not retroactively rewritten by `manifest_defaults.*`; those defaults apply at DB creation time.
- Rollup backfill is engine-owned. Both `nanocli rollup` and `POST /api/v1/rollup/backfill` call the same engine workflow to clear rebuildable destination state, recompute chained rollups, and flush rebuilt destination data to disk before returning.

---

## Non-Goals (v0)

- No separate external index file
- No WAL durability guarantees
- No compaction implementation
- No background collection
- No distributed operation
- No transactional semantics beyond stated crash behavior

---

NanoTDB prioritizes clarity, predictability, and operational simplicity over features.

---

## Databases

NanoTDB supports multiple databases.

A **Database** is an isolated storage unit consisting of:
- its own partitioned `data-<partition>.dat` files
- its own source metric catalog file
- operational manifest metadata file
- optional WAL files (future)

Properties:
- Databases do not share data, WAL state, or catalog mappings
- Metric string -> `MetricID` mapping is database-scoped
- Crash recovery is performed per database
- Retention and lifecycle policies are applied per database
- Dropping a database requires only filesystem removal
