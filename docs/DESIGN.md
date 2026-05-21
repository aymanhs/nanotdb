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
  manifest.toml
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

### Query-optimized metric file (`metric-<partition>.dat`)
- Read-optimized rewrite output for one partition file
- Read-optimized layout: metric samples are grouped by metric instead of interleaved ingest order
- Contains data identical to the source `data-<partition>.dat`, reordered into consolidated metric-local frames for faster metric scans and better compression
- Built by `BuildMetricFileV1`, which flushes the target database, reads `data-<partition>.dat`, and writes `metric-<partition>.dat`
- Query use is controlled by `engine.toml` via `[metrics].enabled`
- Compression codec is selected from `engine.toml` via `[metrics].compression`
- Source raw ingest file handling after metric-file build is controlled by `[metrics].raw_ingest_action` with `keep|rename|delete`
- `CompareDataAndMetricPartitionV1` is the correctness check: it verifies the raw data file and metric file produce the same per-metric sample stream
- Uses an explicit file-format version so future layout changes can be detected before reads

Final binary format (`metric-<partition>.dat`, v1):
- Endianness: little-endian for all integer fields
- Time encoding: signed `int64` Unix nanoseconds (UTC)
- Compression codec ids:
  - `1`: `s2`
  - `2`: `s2_better`
  - `3`: `zstd_fastest`
  - `4`: `zstd_default`
- Checksum: IEEE CRC32 (`crc32.ChecksumIEEE`)
- File shape:
  - fixed 64-byte file header
  - metric-local page frames (one frame per metric in the current builder)
  - trailer payload
  - fixed 16-byte EOF footer

#### 1) File Header (64 bytes at offset `0`)

| Offset | Size | Field | Type | Value / Rule |
|---|---:|---|---|---|
| 0 | 4 | `magic` | `[4]byte` | ASCII `NTMF` |
| 4 | 2 | `version` | `uint16` | `1` |
| 6 | 2 | `header_len` | `uint16` | `64` |
| 8 | 4 | `flags` | `uint32` | bit0=`sealed` (must be `1`) |
| 12 | 1 | `partition_kind` | `uint8` | `1=day, 2=month, 3=year, 4=forever` |
| 13 | 3 | `reserved0` | bytes | must be `0` |
| 16 | 8 | `file_min_ts` | `int64` | min sample timestamp in file |
| 24 | 8 | `file_max_ts` | `int64` | max sample timestamp in file |
| 32 | 4 | `metric_count` | `uint32` | number of metrics written to the file |
| 36 | 4 | `page_count` | `uint32` | total page frames in file (currently equal to `metric_count`) |
| 40 | 8 | `reserved1` | `uint64` | must be `0` |
| 48 | 8 | `reserved2` | `uint64` | must be `0` |
| 56 | 4 | `reserved3` | `uint32` | must be `0` |
| 60 | 4 | `header_crc32` | `uint32` | CRC32 over bytes `0..59` (CRC field always last) |

Current writer behavior:
- Each raw persisted page is decoded and split by metric, then all slices for the same metric across the whole source partition file are merged into one metric-local frame
- Frames are sorted by `MetricID`
- Within each merged frame, source appearance order is preserved and timestamps remain monotonic

#### 2) Metric Page Frame (48-byte header + payload)

Frames are contiguous from offset `header_len` until trailer start.

| Offset (in frame) | Size | Field | Type | Value / Rule |
|---|---:|---|---|---|
| 0 | 4 | `frame_magic` | `[4]byte` | ASCII `MPG1` |
| 4 | 2 | `frame_header_len` | `uint16` | `48` |
| 6 | 2 | `codec_id` | `uint16` | compression codec id (`1=s2, 2=s2_better, 3=zstd_fastest, 4=zstd_default`) |
| 8 | 2 | `metric_id` | `uint16` | source metric id (`1..1023`) |
| 10 | 1 | `value_type` | `uint8` | `1=int32`, `2=float32` |
| 11 | 1 | `reserved0` | `uint8` | `0` |
| 12 | 8 | `start_ts` | `int64` | page minimum timestamp |
| 20 | 8 | `end_ts` | `int64` | page maximum timestamp |
| 28 | 4 | `point_count` | `uint32` | `>=1` |
| 32 | 4 | `payload_len` | `uint32` | compressed payload length |
| 36 | 4 | `uncompressed_len` | `uint32` | decoded payload bytes |
| 40 | 4 | `reserved1` | `uint32` | `0` |
| 44 | 4 | `frame_header_crc32` | `uint32` | CRC32 over header bytes `0..43` (CRC field always last) |

Payload encoding before compression:
- `times`: `point_count` x `int64` (little-endian)
- `values`: `point_count` x scalar value bytes (`int32` or `float32`, little-endian)

The payload is compressed as one independent block per frame using the codec declared by `codec_id`. Frames never reference other frames.

Immediately after each compressed payload:
- `payload_crc32` (`uint32`): CRC32 of compressed payload bytes (payload CRC is last field for the payload record)

#### 3) Trailer Payload (fixed-size `PAGE_INFO` array)

Trailer on disk is:
- `PAGE_INFO` entry repeated `page_info_count` times
- followed immediately by the fixed 16-byte EOF footer

`PAGE_INFO` entry (44 bytes, repeated):

| Offset (in entry) | Size | Field | Type | Value / Rule |
|---|---:|---|---|---|
| 0 | 2 | `metric_id` | `uint16` | source metric id |
| 2 | 1 | `value_type` | `uint8` | `1=int32`, `2=float32` |
| 3 | 1 | `reserved0` | `uint8` | `0` |
| 4 | 8 | `page_offset` | `uint64` | absolute file offset of metric page frame |
| 12 | 8 | `metric_min_ts` | `int64` | min metric timestamp |
| 20 | 8 | `metric_max_ts` | `int64` | max metric timestamp |
| 28 | 4 | `point_count` | `uint32` | samples in this metric-local page frame |
| 32 | 4 | `uncompressed_len` | `uint32` | duplicated decoded page length for fast planning |
| 36 | 4 | `payload_len` | `uint32` | duplicated compressed payload length |
| 40 | 4 | `reserved1` | `uint32` | `0` |

Indexing rule:
- The array position of each `PAGE_INFO` entry is the page index (0-based). No explicit `page_index` field is stored in v1.

The current builder emits one `PAGE_INFO` entry per metric-local frame and one frame per metric for the whole source partition file.

#### 4) EOF Footer (16 bytes at file end)

| Offset (from EOF-16) | Size | Field | Type | Value / Rule |
|---|---:|---|---|---|
| 0 | 4 | `footer_magic` | `[4]byte` | ASCII `NTFT` |
| 4 | 4 | `trailer_version` | `uint32` | `1` |
| 8 | 4 | `page_info_count` | `uint32` | number of `PAGE_INFO` entries |
| 12 | 4 | `footer_crc32` | `uint32` | CRC32 over bytes `0..11` of footer (CRC field always last) |

#### Reader Validation Order (required)

1. Read fixed footer at EOF and validate `footer_magic`, `trailer_version`, and `footer_crc32`.
2. Compute trailer start as `file_size - 16 - (page_info_count * 44)`; reject underflow/overflow.
3. Read `PAGE_INFO[]` and validate reserved fields and unique `page_offset` values.
4. Read file header at offset `0`; validate `magic`, `version`, `header_len`, and `header_crc32`.
5. For each `PAGE_INFO`, jump directly to `page_offset`.
6. For each page read, validate `frame_magic`, header CRC, payload CRC, resolve a supported `codec_id`, cross-check `metric_id`, `value_type`, lengths, and timestamps against `PAGE_INFO`, then decompress and verify decoded byte count equals `uncompressed_len`.

Reader note:
- The reader is tolerant of multiple frames for the same metric, but `BuildMetricFileV1` currently coalesces all samples for each metric in the partition to one frame.

#### Writer Finalization Rules (required)

- File must be written as temp + atomic rename into `metric-<partition>.dat`.
- Footer is written last; a missing/invalid footer means file is not finalized and must be ignored.
- All reserved fields are zeroed in v1.
- Builder creates the parent directory if needed and writes to `<path>.tmp` before rename.
- After successful metric-file build, the source raw ingest file is either kept in place, renamed to `raw-<partition>.dat`, or deleted according to `[metrics].raw_ingest_action`.
- Unknown `version`/`trailer_version` values are hard read failures.

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

## Logging Model

- Runtime logging uses `log/slog` with plain text handlers in v1.
- Logging config is engine-owned via `engine.toml` `[logging]` / `[[logging.logger]]` entries.
- The engine keeps thin `logInfo` / `logDebug` / `logTrace` helpers instead of exposing raw logger mutation as part of the public API.
- `trace` is a custom level below `debug`, used for noisy flow events such as per-sample ingest and HTTP request summaries.
- `nanocli` diagnostics are intentionally separate from normal command output and are file-only unless future requirements change that policy.

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
- When `[metrics].enabled = true`, persisted partition reads prefer `metric-<partition>.dat` when present and fall back to raw ingest files otherwise

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
| `metrics.enabled` | `EngineConfig.Metrics.Enabled` | `Engine.MetricFilesEnabled` and `QueryRange` metric-file routing | Default `false` |
| `metrics.compression` | `EngineConfig.Metrics.Compression` | `Engine.MetricFileCompression` and `BuildMetricFileV1` frame codec selection | Valid values: `s2`, `s2_better`, `zstd_fastest`, `zstd_default`; default `zstd_fastest` |
| `metrics.raw_ingest_action` | `EngineConfig.Metrics.RawIngestAction` | `Engine.MetricRawIngestAction` and post-build raw-file handling | Valid values: `keep`, `rename`, `delete`; `rename` uses `raw-<partition>.dat` |
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
