# NanoTDB

A small, embedded time-series database designed for resource-constrained hosts
(Raspberry Pi, edge nodes, IoT gateways). No external dependencies at runtime.
All data lives in plain files under a single root directory.

> **Important:** NanoTDB supports automatic aggregates and rollups.
> When you ingest time-series samples, the engine can automatically maintain
> aggregate summaries for higher-level time buckets, reducing query cost and
> improving query performance for long-range or downsampled queries.
> 
> This behavior is built into the storage engine and is one of the core
> differentiators of NanoTDB.

---

## 🚀 Quick Start

**New to NanoDB?** See [**Getting Started**](GETTING_STARTED.md) for installation, compilation, and practical examples using shell scripts and Python.

Prefer ready-to-use binaries? Download the latest release assets from
[GitHub Releases](https://github.com/aymanhs/nanotdb/releases/latest):

- Old Raspberry Pi (Pi 0/1): `nanotdb-linux-armv6-rpi0-rpi1` and `nanocli-linux-armv6-rpi0-rpi1`
- Newer 32-bit Raspberry Pi OS (Pi 2/3/4): `nanotdb-linux-armv7-rpi3-rpi4` and `nanocli-linux-armv7-rpi3-rpi4`
- 64-bit Raspberry Pi OS: `nanotdb-linux-arm64` and `nanocli-linux-arm64`

Release/change history:

- Published release notes and downloads: [Releases](https://github.com/aymanhs/nanotdb/releases)
- Detailed change log in-repo: [CHANGELOG.md](CHANGELOG.md)

For technical deep-dives, continue below.

---

## Architecture overview

```
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

---

## Data flow

### Ingest (`AddLine`)

```
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
data file is behind. The catalog is used to resolve ValueTypes for metrics that
omit them (compact format optimization). After a full replay the engine is
ready to accept new writes.

### Query (`QueryRange`)

```
QueryRange("prod", "room.temp", fromTS, toTS, stride, callback)
  │
  ├─ iterate UTC days in [fromTS, toTS]
  │    ├─ open data-<partition>.dat  →  scan page frame headers
  │    │    skip frames outside time window (no decompression)
  │    │    decompress + scan matching frames
  │    └─ check in-memory page for today's data
  └─ call callback for each sample (every Nth if stride > 1)
```

---

## Line protocol

```
DB/metric.name value [ts]
```

- `DB` — database name (created automatically on first write)
- `metric.name` — arbitrary metric identifier (slash-separated namespaces work well)
- `value` — integer (`42`, `-7`) or float (`3.14`, `1e-3`). An integer literal
  always creates an `int32` metric; a float literal creates a `float32` metric.
  Type is fixed on first write; mixing types for the same metric is an error.
- `ts` — Unix nanosecond timestamp (optional; defaults to `time.Now()`)

Examples:

```
prod/room.temp 21.5 1715000000000000000
sensors/pressure.hpa 1013
internal/batch.size 256i
```

The `i` suffix forces integer interpretation for values that look like floats.

---

## WAL format (compact v2)

Each record is a **uvarint length prefix** followed by a fixed-layout payload:

```
[uvarint: payload_len] [payload]
```

Payload layout:

```
Offset  Size  Field
  0      2    MetricID          uint16 LE
  2      3    TS delta          uint24 LE nanoseconds from baseline
  5      1    CompactTL flags   bit 7 = new baseline, bit 6 = new metric
  6      8    Baseline TS       int64 LE  (only when bit 7 set)
  —      var  name_len+name+vtype         (only when bit 6 set)
  —      4    Value             int32 or float32 LE, always present
```

- **Hot path** (known metric, same baseline): `2+3+1+4 = 10 bytes` + 1 varint = **11 bytes**.
- A new baseline is emitted on the first record of each WAL and whenever the
  timestamp gap exceeds ~16.7 ms (2²⁴ ns). Typical sensor streams fit hundreds
  of seconds between baseline resets.
- Known metrics (previously seen in the session) omit the name and value type;
  those fields are recovered from the catalog during replay.

---

## On-disk layout

```
<root>/
  engine.toml          — engine configuration (auto-created on first start)
  <db>/
    catalog.json       — metric registry: name → id + type
    manifest.toml      — per-database settings (retention, WAL, page limits)
    <db>.wal           — write-ahead log (single reusable file)
    data-<partition>.dat — compressed page frames for completed partitions
```

Data files are append-only sequences of page frames:

```
Frame = PageHeader(18 bytes) + compressed_len(uvarint) + S2-compressed payload + CRC32(4 bytes)
```

The payload is a flat array of interleaved (MetricID, Timestamp, Value) triples,
sorted by timestamp. S2 compression typically achieves 3–4× on realistic sensor
data.

---

## Configuration (`engine.toml`)

Created automatically at `<root>/engine.toml` on first start. Key settings:

| Key | Default | Effect |
|---|---|---|
| `engine.listen` | `:8428` | HTTP server address |
| `wal.max_segment_size` | `67108864` (64 MiB) | WAL size before reset after a page flush |
| `wal.fsync_policy` | `segment` | `segment` = fsync on WAL reset; `always` = fsync every append |
| `durability.profile` | `strict` | `strict` / `balanced` / `throughput` (see below) |
| `stats.enabled` | `true` | Emit engine self-metrics to the `internal` database |
| `stats.interval` | `30s` | How often stats are flushed |

**Durability profiles:**

| Profile | Page file fsync | Catalog fsync |
|---|---|---|
| `strict` | yes | yes |
| `balanced` | yes | no |
| `throughput` | no | no |

Per-database settings (retention, partitioning, WAL skip window, page flush thresholds, rollups) live in
`<db>/manifest.toml` and default values can be set in `engine.toml` under
`[manifest_defaults]`.

Partition options in `[retention]`:
- `partition = "day"` (default): `data-YYYY-MM-DD.dat`
- `partition = "month"`: `data-YYYY-MM.dat`
- `partition = "year"`: `data-YYYY.dat`
- `partition = "forever"`: `data-forever.dat`

### Rollups (`manifest.toml`)

Rollup jobs are defined in the **source** database manifest under `[rollups]`.

Example:

```toml
[rollups]
enabled = true
checkpoint_file = "rollup.checkpoints.log"
default_grace = "5m"

[[rollups.jobs]]
id = "outside_temp_1h"
source_metric = "temp.out_dry"
interval = "1h"
aggregates = ["min", "max", "sum", "avg", "count"]
destination_db = "sensors_rollup_1h"
destination_metric_prefix = "temp.out_dry"
```

Rollup config reference:

| Field | Scope | Required | Valid / Default | Notes |
|---|---|---|---|---|
| `rollups.enabled` | DB | no | `true|false` (default `false`) | Enables rollup processing for this DB as a source. |
| `rollups.checkpoint_file` | DB | no | string (default `rollup.checkpoints.log`) | Checkpoint log path, relative to source DB directory. |
| `rollups.default_grace` | DB | no | Go duration or empty | Used when job `grace` is omitted. |
| `rollups.jobs[].id` | Job | yes | non-empty string | Unique per source DB for checkpoint tracking. |
| `rollups.jobs[].source_metric` | Job | yes | non-empty string | Metric to read from source DB. |
| `rollups.jobs[].interval` | Job | yes | valid Go duration (`>0`) | Rollup bucket size (for example `1h`, `24h`). |
| `rollups.jobs[].aggregates` | Job | no | `min|max|sum|avg|count` (default all five) | Aggregate outputs written as suffix metrics. |
| `rollups.jobs[].destination_db` | Job | yes | non-empty string | Target DB receiving rollup samples. |
| `rollups.jobs[].destination_metric_prefix` | Job | no | string (default `source_metric`) | Output names are `<prefix>.<agg>`. |
| `rollups.jobs[].grace` | Job | no | Go duration or empty | Overrides `default_grace` for this job. |

Notes:
- Checkpoints are stored in the source DB (default `rollup.checkpoints.log`).
- Destination DBs can also define their own rollup jobs to create cascades (for example `1h -> 1d`).
- For low-frequency rollup outputs, use coarser partitions on destination DBs (for example `month` or `year`) to avoid many tiny day files.

---

## Binaries

### `nanotdb` — server

```
nanotdb --config <path>      start server using given engine.toml
nanotdb --init --config <path>   write default engine.toml and exit
```

Exposes a small HTTP API compatible with the VictoriaMetrics instant/range query
wire format (`/api/v1/import`, `/api/v1/import/prometheus`, `/api/v1/query`, `/api/v1/query_range`).

API quick reference:

- `GET /health` - health check
- `POST /api/v1/import` - import line protocol (raw body or JSON payload)
- `POST /api/v1/import/prometheus` - Prometheus-compatible import endpoint
- `GET /api/v1/query` - instant query (latest point)
- `GET /api/v1/query_range` - range query

Also exposes discovery endpoints:

- `GET /api/v1/databases` (use `?include_internal=true` to include the internal DB)
- `GET /api/v1/metrics?db=<name>` (use `&details=true` for id/type metadata)

### `nanocli` — offline CLI tool

Operates directly on the data directory without a running server.

```
nanocli inspect db  --root <dir> [--db <name>] [--json]  — overview of all/one database
nanocli inspect dat --root <dir>  --db <name>  [--json]  — page frame headers in .dat files
nanocli inspect wal --root <dir>  --db <name>  [--json]  — WAL record dump

nanocli import --root <dir> --in <file.lp>  [--json]     — bulk import line-protocol file
nanocli export --root <dir> --db <name> [--out <file.lp>] — export database to line protocol (stdout when --out is omitted)

nanocli query  --root <dir> --db <name> --metric <regex>
               [--start <time>] [--end <time>] [--format table|json]
```

LP timestamps (import and exported files) accept / use: `YYYY-MM-DD HH:MM:SS.nnnnnnnnn` (UTC)
and also accept raw Unix nanoseconds on import.

`--start` / `--end` accept RFC3339 strings, `YYYY-MM-DD [HH[:MM[:SS[.nnnnnnnnn]]]]`,
or Unix timestamps (seconds or nanoseconds).

### Rollup full-cycle check script

For deterministic end-to-end verification (generate LP -> import -> rollups -> export -> compare expected), run:

```bash
./scripts/rollup_full_cycle_check.sh
```

Optional arguments:
- `./scripts/rollup_full_cycle_check.sh <root-dir> <duration-hours> <metrics> <cadence-seconds> <gap-metrics>`
- Defaults: `root-dir=test-data/full-cycle-check`, `duration-hours=30`, `metrics=10`, `cadence-seconds=10`, `gap-metrics=2`

Generated artifacts are placed in `<root-dir>/work` for easy discovery:
- `scenario_summary.json` (duration, rates, counts, per-metric stats)
- `known_gaps.csv` (deterministic missing windows for `temp.gap_probeXX` metrics)
- `SCENARIO.md` (quick human-readable summary)

---

## Engine API (embedding)

```go
e, err := engine.OpenEngine("/data", 0)   // 0 = default WAL segment size
defer e.Close()

// Ingest
err = e.AddLine("sensors/temp 22.1 " + strconv.FormatInt(time.Now().UnixNano(), 10))

// Range query
err = e.QueryRange("sensors", "temp", fromTS, toTS, 1, func(s engine.Sample) error {
    fmt.Println(s.TS, s.Float32)
    return nil
})

// Last value (from in-memory catalog cache)
sample, ok, err := e.QueryLast("sensors", "temp")

// Bulk import / export
err = e.ImportFile("backup.lp")
err = e.ExportFile("sensors", "backup.lp")
```

Key types:

| Type | Description |
|---|---|
| `Engine` | Top-level coordinator; safe for concurrent use |
| `Database` | One named DB with WAL + catalog + data files |
| `Catalog` | Metric name ↔ ID registry; persisted as JSON |
| `Page` | In-memory buffer of interleaved samples; flushed when full |
| `WAL` | Single-file write-ahead log with compact v2 encoding |
| `Sample` | Decoded data point from a query |
| `Timestamp` | `int64` Unix nanoseconds |
| `MetricID` | `uint16` per-database metric address |
