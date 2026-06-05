# NanoTDB Architecture

This page contains the deeper storage and query walkthrough that used to live on
the main README. For a friendlier, mental-model-first introduction to the same
material, see [CONCEPTS.md](CONCEPTS.md).

## Architecture Overview

```text
Engine
 ├── "prod"    Database  → WAL (prod.wal) + Catalog (catalog.json) + partitioned .dat files
 ├── "sensors" Database  → WAL + Catalog + partitioned .dat files
 └── "internal"          → engine self-metrics (same layout, never exposed to users)
```

The `Engine` is the single entry point. It owns a collection of named databases
and routes ingested samples to the right one based on the line-protocol prefix.

Each `Database` has up to **six** storage layers, three for metrics (always
present) and three for events (present when `[events].enabled = true`):

| Layer | File | Purpose |
|---|---|---|
| Metric WAL | `<db>.wal` | Crash-safety: records every sample before it enters the page |
| Metric Catalog | `catalog.json` | Maps metric names ↔ compact MetricIDs + value types |
| Metric Data files | `data-<partition>.dat` | Immutable compressed pages flushed from memory |
| Events WAL | `<db>.events.wal` | Crash-safety for the events layer; independent of the metric WAL |
| Events Catalog | `events.json` | Maps event names ↔ compact EventIDs + value types |
| Events Data files | `events-<partition>.dat` | Immutable compressed event pages, one per partition window |

The metric and event layers share the database's partition mode and
retention policy but have independent catalogs, WALs, and page files.
Disabling events on a database leaves the metric storage untouched.

## Data Flow

### Ingest (`AddLine`)

```text
AddLine("prod/room.temp 21.5 1715000000000000000")
  │
  ├─ parse line protocol  →  dbName="prod"  metric="room.temp"  ts=…  value=21.5
  ├─ getOrCreateDB        →  open or reuse prod Database
  ├─ WAL append           →  write compact record to prod.wal  (crash-safe)
  ├─ append to open page  →  in-memory Page for the current partition
  │                          window (day/month/year/forever per manifest)
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
  ├─ iterate UTC partition windows in [fromTS, toTS]
  │    ├─ prefer metric-<partition>.dat if it exists
  │    │    use internal indexes to locate per-metric frames directly
  │    ├─ else open data-<partition>.dat → scan page frame headers
  │    │    skip frames outside time window (no decompression)
  │    │    decompress + scan matching frames
  │    └─ check the in-memory page for the current partition window
  └─ call callback for each sample (every Nth if stride > 1)
```

### Events: ingest, replay, query

The events layer follows the same write→WAL→page→file→retention shape as
metrics, but uses its own files, its own catalog, and a different
on-disk record format. See [EVENTS.md](EVENTS.md) for the byte-level
spec; the data-flow shape is:

```text
AddEvent("prod", "disc.write.slow", ts, value, payload)
  │
  ├─ resolve or assign EventID via events catalog
  ├─ events WAL append (newEvent flag on first occurrence carries
  │    the name + value_type inline so the catalog is reconstructible
  │    from the WAL alone)
  ├─ append to open events page  →  in-memory EventsPage for the
  │                                  current partition window
  └─ if page full / MustForceFlush → compress + write frame to
                                      events-<partition>.dat
                                      reset events WAL when safe
```

Per-event-name timestamps are monotonic-non-decreasing. Different event
names in the same partition page may interleave with reordered
timestamps (the rule is enforced at the catalog level, not per page —
see [LAWS.md](LAWS.md) LAW 2).

```text
QueryEvents("prod", "disc.*", fromTS, toTS, callback)
  │
  ├─ resolve name pattern to an EventID set via the events catalog
  ├─ iterate UTC partition windows in [fromTS, toTS]
  │    ├─ open events-<partition>.dat → scan frame headers
  │    │    AND the query EventID set with the per-frame 128-byte
  │    │    bitmap → skip frames that don't intersect, no decompress
  │    │    decompress + scan matching frames
  │    └─ check the in-memory events page for the current partition
  └─ call callback for each matching event
```

Replay on engine open: events WAL replay reconstructs the in-memory
events catalog (from `newEvent` records' inline `(name, value_type)`)
and the in-memory events page, in parallel with metric WAL replay. The
two replays never share state — corruption in one cannot poison the
other.

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
  engine.toml              — engine configuration (auto-created on first start)
  <db>/
    catalog.json           — metric registry: name → id + type
    manifest.toml          — per-database settings (retention, WAL, page limits, rollups, events)
    <db>.wal               — metric write-ahead log (single reusable file)
    data-<partition>.dat   — raw metric ingest: compressed page frames in write order
    metric-<partition>.dat — optional query-optimized metric layout (when built)
    raw-<partition>.dat    — renamed source raw file after a metric-file build
                             (only with [metrics].raw_ingest_action = "rename")
    events.json            — event registry: name → id + type           (when [events].enabled = true)
    <db>.events.wal        — events write-ahead log (single reusable file)
    events-<partition>.dat — events: compressed page frames, one per partition window
```

See [METRIC_FILES.md](METRIC_FILES.md) for the metric-file layout,
[EVENTS.md](EVENTS.md) for the events-file byte spec, and
[CONCEPTS.md](CONCEPTS.md) for a friendlier walkthrough.

Metric data files are append-only sequences of page frames:

```text
Frame = PageHeader(18 bytes) + compressed_len(uvarint) + S2-compressed payload + CRC32(4 bytes)
```

The payload is a flat array of interleaved `(MetricID, Timestamp, Value)` triples,
sorted by timestamp. S2 compression typically achieves 3–4x on realistic sensor
data.

Events data files use a different frame shape — fixed-size header
including a per-frame event-id presence bitmap for skip-without-decompress
on name-filtered queries:

```text
Frame = EventsFrameHeader(152 bytes) + S2-compressed payload + CRC32(4 bytes)

EventsFrameHeader = start_ts(8) + end_ts(8) + record_count(4)
                  + event_id_bitmap(128) + compressed_len(4)
```

See [EVENTS.md](EVENTS.md) for the inner record layout and the
bitmap-skip optimization.