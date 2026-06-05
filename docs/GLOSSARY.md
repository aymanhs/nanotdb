# NanoTDB – Glossary

This glossary defines **canonical terminology** used throughout NanoTDB.
These terms are foundational. Their meanings must not drift.

---

## Database

A **Database** is an isolated namespace within NanoTDB.

Properties:
- Contains its own metrics, partitioned data files (`data-<partition>.dat`), catalog file, manifest, and WAL
- When `[events].enabled = true`, also contains its own event catalog (`events.json`), events WAL (`<db>.events.wal`), and partitioned event files (`events-<partition>.dat`)
- Represents a unit of retention, lifecycle, and failure isolation
- Metrics and events do not cross database boundaries
- Partition granularity (`day|month|year|forever`) is per-database via the manifest and applies to both metrics and events
- No external sidecar index file exists; raw ingest queries scan data files directly

---

## Metric

A **Metric** is a monotonically time-ordered stream of numeric samples.

Properties:
- Exactly one numeric value per timestamp
- Samples are ordered by time
- Represents a single physical or logical quantity
- Stored independently of other metrics
- Has a fixed value type set at creation (`int32` or `float32`)

Examples:
- `cpu_temperature`
- `disk_io_latency`
- `room_humidity`

---

## Metric Identifier (API)

A **Metric Identifier** is the API-visible string name for a metric.

Properties:
- Chosen by caller and provided on write/query requests
- Mapped to an internal `MetricID` within a database
- Unique within a database namespace

---

## MetricID (Storage)

A **MetricID** is the internal 2-byte identifier for a metric (`uint16`).

Properties:
- Assigned automatically when a new metric identifier is first seen
- Persisted in the per-database catalog file
- Never deleted
- Never reused

---

## Sample

A **Sample** is a single data point within a metric.

A sample consists of:
- A timestamp (`int64`, nanoseconds)
- A numeric value (`int32` or `float32`)

Samples are append-only and immutable once persisted.

---

## Page

A **Page** is a variable-length compressed frame of on-disk storage.

Properties:
- Stores interleaved samples from multiple metrics
- Append-only while in memory
- Immutable once written to disk
- Each page is fully self-contained with header (start time, end time, record count)
- Compressed payload with integrity check (CRC32)

---

## In-Memory Page

An **In-Memory Page** is a mutable page under construction.

Properties:
- Exists only in RAM
- Receives new samples from multiple metrics
- Protected by WAL replay on restart
- Flushed to daily data file once full

---

## Partition Data File (`data-<partition>.dat`)

A **Partition Data File** is the append-only raw ingest storage for one
database and one UTC partition window.

Properties:
- Named by partition mode: `data-YYYY-MM-DD.dat` (day), `data-YYYY-MM.dat` (month), `data-YYYY.dat` (year), or `data-forever.dat`
- Contains one or more compressed page frames in write order
- Frames may interleave samples from multiple metrics
- All frames are immutable once written
- Grows monotonically until the next partition window opens
- Retention is enforced by deleting old partition files

---

## Metric File (`metric-<partition>.dat`)

A **Metric File** is the optional query-optimized rewrite of a sealed
partition's raw data, grouping samples by metric instead of by ingest order.

Properties:
- Same logical content as the source `data-<partition>.dat` for the partition
- Layout is read-optimized: one frame per metric, plus shared timestamp frames in `v2`
- Built when `[metrics].enabled = true` on partition seal, or manually with `nanocli build metric`
- Read first by `QueryRange` whenever a `metric-<partition>.dat` exists; raw files are the fallback
- Carries an internal header, time-frame/metric-frame indexes, and an EOF footer

---

## Renamed Raw File (`raw-<partition>.dat`)

A **Renamed Raw File** is the source `data-<partition>.dat` after a
successful `metric-<partition>.dat` build under `[metrics].raw_ingest_action = "rename"`.

Properties:
- Same bytes as the original `data-<partition>.dat`
- Still readable by inspect, query, and rebuild paths
- Lets metric files become the primary read path while keeping the raw
  ingest layout available for verification or rebuild

---

## Catalog File

The **Catalog File** stores database catalog state required at startup.

Properties:
- Contains metric definitions and metric ids
- Contains metric identifier -> `MetricID` mapping
- Stores fixed metric value type per metric
- Scoped per database

---


## WAL (Write-Ahead Log)

The **WAL** protects data that exists only in memory.

Properties:
- Append-only
- Stored as one reusable file per database (`<db>.wal`)
- Required for crash recovery of unflushed page data
- Reset only after page data is flushed and no open pages remain
- Does not protect data already flushed to disk

---

## WAL Segment

A **WAL Segment** is a single append-only WAL file.

Properties:
- Current implementation uses a single reusable segment id in one WAL file
- Segment id metadata is retained for compatibility with future multi-segment WAL designs

---

## Sealed Page

A **Sealed Page** is a page that has been fully written to disk.

Properties:
- Immutable
- WAL protection no longer required

---

## Event

An **Event** is a discrete, named occurrence with an optional typed value
and an optional opaque payload.

Properties:
- One `(timestamp, value?, payload?)` tuple per occurrence
- Has a fixed value type set at first write: `none`, `int32`, or `float32`
- Payload is opaque bytes the engine never parses (typically JSON)
- Per-event-name timestamps are monotonically non-decreasing
- Stored independently from metrics; events do not appear in metric pages
- Opt-in per database via `[events].enabled` in the manifest

Examples:
- `disc.write.slow` (int32 value: latency in ms)
- `temp.office.overheat` (float32 value: temperature in C)
- `heartbeat` (no value)

---

## Event Identifier (API)

An **Event Identifier** is the API-visible string name for an event.

Properties:
- Chosen by caller and provided on write/query requests
- Mapped to an internal `EventID` within a database
- Unique within a database namespace
- Independent of the `Metric Identifier` space — a database may have a
  metric and an event with the same string name

---

## EventID (Storage)

An **EventID** is the internal 2-byte identifier for an event (`uint16`).

Properties:
- Assigned automatically when a new event identifier is first seen
- Persisted in the per-database event catalog file
- Never deleted
- Never reused
- Constrained to `1..1023` — a hard architectural constant, not a
  config knob, because the page header's event-id presence bitmap is
  sized for exactly that range (128 bytes)
- Independent of `MetricID`; the two spaces never collide

---

## Event Catalog (`events.json`)

The **Event Catalog** stores per-database event registry state.

Properties:
- Contains event definitions and event ids
- Contains event identifier -> `EventID` mapping
- Stores fixed event value type per event (`none|int32|float32` as a
  human-readable string)
- Scoped per database; lives alongside `catalog.json`
- Must be persisted to disk before the events WAL is reset (crash-safety
  rule 1 in [EVENTS.md](EVENTS.md))

---

## Events WAL (`<db>.events.wal`)

The **Events WAL** is the write-ahead log for the events layer.

Properties:
- Independent of the metric WAL — separate file, separate format
- Append-only with a per-record uvarint length prefix
- Carries a `newEvent` flag bit on first occurrence of each event name
  so the events catalog can be rebuilt from the WAL alone after a crash
- Reset only after the in-memory events page is flushed and the events
  catalog has been written
- Honors the same fsync policy and segment-cap discipline as the metric
  WAL, but its segment cap defaults smaller (16 MiB) because events are
  sparse

---

## Events Page File (`events-<partition>.dat`)

An **Events Page File** is the append-only durable storage for one
database, one events stream, and one UTC partition window.

Properties:
- Named by partition mode: `events-YYYY-MM-DD.dat` (day),
  `events-YYYY-MM.dat` (month), etc.
- Contains one or more frames; each frame has a 152-byte fixed header
  including a 128-byte event-id presence bitmap, plus an S2-compressed
  payload and a CRC32 trailer
- The bitmap lets name-filtered queries skip whole frames without
  decompressing
- Decoding requires the events catalog (the on-disk record omits
  `ValueType`; per-event value width is resolved by catalog lookup)
- Joins the partition family for retention (`keep|delete|archive`
  treats `events-<partition>.dat` alongside the matching
  `data-`/`raw-`/`metric-` files)

---

## Rollup

A **Rollup** is a downsampling job that reads a source database's metrics
and writes lower-resolution aggregate metrics (`min|max|sum|avg|count`)
into a destination database.

Properties:
- Reads only durable, already-flushed data
- Writes destination samples via the normal engine write path
- Never modifies existing source pages
- Defined in the source database's `manifest.toml` under `[rollups]`
- Tracked by a per-source checkpoint log (`rollup.checkpoints.log` by default)
- Can be chained (e.g. raw → 1h → 1d) by defining rollups on a destination DB
- Backfillable offline via `nanocli rollup` or online via `POST /api/v1/rollup/backfill`

---

## Metric File Build

A **Metric File Build** is the process of rewriting one sealed partition's
raw data into a query-optimized `metric-<partition>.dat`.

Properties:
- Reads only durable, sealed partition data
- Produces a new file via temp + atomic rename
- Never modifies existing raw pages
- After success, handles the raw source according to `[metrics].raw_ingest_action` (`keep|rename|delete`)
- Runs automatically on partition seal when `[metrics].enabled = true`, or on demand via `nanocli build metric`

---

## Crash Recovery

**Crash Recovery** reconstructs the database state after a failure.

Properties:
- Uses catalog, daily `.dat` files, and WAL
- Memory state is discarded
- Deterministic and repeatable

---

## Append-Only

**Append-Only** means that persisted data is never modified after being written.

Properties:
- No in-place updates
- No rewrites
- Truth is preserved forever

---

This glossary defines the shared language of NanoTDB.
If a term is ambiguous, it must be clarified here before code is written.

