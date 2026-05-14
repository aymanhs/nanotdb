# NanoTDB – Glossary

This glossary defines **canonical terminology** used throughout NanoTDB.
These terms are foundational. Their meanings must not drift.

---

## Database

A **Database** is an isolated namespace within NanoTDB.

Properties:
- Contains its own metrics, daily data files (`data-YYYY-MM-DD.dat`), catalog file, and operational manifest
- Represents a unit of retention, lifecycle, and failure isolation
- Metrics do not cross database boundaries
- No index file exists; queries scan data files directly

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

## Daily Data File (`data-YYYY-MM-DD.dat`)

A **Daily Data File** is the append-only storage for one database and one UTC day.

Properties:
- Named as `data-YYYY-MM-DD.dat`
- Contains one or more compressed page frames
- All frames are immutable once written
- Grows monotonically until next day
- Retention is enforced by deleting old day files

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

## Compaction

**Compaction** is the process of producing new, lower-resolution metrics
from existing data.

Properties:
- Reads only durable data
- Writes new pages via the normal write path
- Never modifies existing pages
- Not implemented in v1

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

