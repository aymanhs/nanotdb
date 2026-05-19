# NanoTDB Architecture

This page contains the deeper storage and query walkthrough that used to live on
the main README.

## Architecture Overview

```text
Engine
 ├── "prod"    Database  → WAL (prod.wal) + Catalog (catalog.json) + partitioned .dat files
 ├── "sensors" Database  → WAL + Catalog + partitioned .dat files
 └── "internal"          → engine self-metrics (same layout, never exposed to users)
```

The `Engine` is the single entry point. It owns a collection of named databases
and routes ingested samples to the right one based on the line-protocol prefix.

Each `Database` has three storage layers:

| Layer | File | Purpose |
|---|---|---|
| WAL | `<db>.wal` | Crash-safety: records every sample before it enters the page |
| Catalog | `catalog.json` | Maps metric names ↔ compact MetricIDs + value types |
| Data files | `data-<partition>.dat` | Immutable compressed pages flushed from memory |

## Data Flow

### Ingest (`AddLine`)

```text
AddLine("prod/room.temp 21.5 1715000000000000000")
  │
  ├─ parse line protocol  →  dbName="prod"  metric="room.temp"  ts=…  value=21.5
  ├─ getOrCreateDB        →  open or reuse prod Database
  ├─ WAL append           →  write compact record to prod.wal  (crash-safe)
  ├─ addToOpenDay         →  append to in-memory Page for today's bucket
  └─ if page full         →  compress + write page frame to data-<partition>.dat
                              reset WAL (replay no longer needed)
```

Timestamps must be monotonically non-decreasing per metric across the entire
write stream. Out-of-order or stale samples are rejected.

### Replay (on engine open)

When a database is opened, the WAL is replayed into the in-memory page if the
data file is behind. The catalog is used to resolve value types for metrics that
omit them in the compact WAL format. After replay the engine is ready to accept
new writes.

### Query (`QueryRange`)

```text
QueryRange("prod", "room.temp", fromTS, toTS, stride, callback)
  │
  ├─ iterate UTC days in [fromTS, toTS]
  │    ├─ open data-<partition>.dat  →  scan page frame headers
  │    │    skip frames outside time window (no decompression)
  │    │    decompress + scan matching frames
  │    └─ check in-memory page for today's data
  └─ call callback for each sample (every Nth if stride > 1)
```

## Line Protocol

```text
DB/metric.name value [ts]
```

- `DB` — database name (created automatically on first write)
- `metric.name` — arbitrary metric identifier (slash-separated namespaces work well)
- `value` — integer (`42`, `-7`) or float (`3.14`, `1e-3`). An integer literal
  always creates an `int32` metric; a float literal creates a `float32` metric.
  Type is fixed on first write; mixing types for the same metric is an error.
- `ts` — Unix nanosecond timestamp (optional; defaults to `time.Now()`)

Examples:

```text
prod/room.temp 21.5 1715000000000000000
sensors/pressure.hpa 1013
internal/batch.size 256i
```

The `i` suffix forces integer interpretation for values that look like floats.

## WAL Format (compact v2)

Each record is a uvarint length prefix followed by a fixed-layout payload:

```text
[uvarint: payload_len] [payload]
```

Payload layout:

```text
Offset  Size  Field
  0      2    MetricID          uint16 LE
  2      3    TS delta          uint24 LE nanoseconds from baseline
  5      1    CompactTL flags   bit 7 = new baseline, bit 6 = new metric
  6      8    Baseline TS       int64 LE  (only when bit 7 set)
  —      var  name_len+name+vtype         (only when bit 6 set)
  —      4    Value             int32 or float32 LE, always present
```

- Hot path (known metric, same baseline): `2+3+1+4 = 10 bytes` plus one varint = `11 bytes`.
- A new baseline is emitted on the first record of each WAL and whenever the
  timestamp gap exceeds about 16.7 ms (`2^24` ns).
- Known metrics omit the name and value type in the compact record; those fields
  are recovered from the catalog during replay.

## On-Disk Layout

```text
<root>/
  engine.toml            — engine configuration (auto-created on first start)
  <db>/
    catalog.json         — metric registry: name → id + type
    manifest.toml        — per-database settings (retention, WAL, page limits)
    <db>.wal             — write-ahead log (single reusable file)
    data-<partition>.dat — compressed page frames for completed partitions
```

Data files are append-only sequences of page frames:

```text
Frame = PageHeader(18 bytes) + compressed_len(uvarint) + S2-compressed payload + CRC32(4 bytes)
```

The payload is a flat array of interleaved `(MetricID, Timestamp, Value)` triples,
sorted by timestamp. S2 compression typically achieves 3–4x on realistic sensor
data.