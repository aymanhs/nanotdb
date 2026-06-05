# Concepts

The friendly walkthrough. If you've used a TSDB before, none of this will
surprise you — it's just plain-language coverage of what the moving parts
are, why they exist, and how they fit together.

For the canonical, do-not-drift definitions, see [GLOSSARY.md](GLOSSARY.md).
For the deeper storage and query walkthrough, see [ARCHITECTURE.md](ARCHITECTURE.md).

---

## A database is a namespace

A NanoTDB **database** is just an isolated namespace on disk:

```text
~/nanotdb-data/
  engine.toml          ← engine-wide config
  prod/                ← one database
    catalog.json
    manifest.toml
    prod.wal
    data-2026-06-01.dat
    data-2026-06-02.dat
    ...
  sensors/             ← another database, totally independent
    catalog.json
    manifest.toml
    sensors.wal
    data-2026-06.dat   ← uses month partitioning
    ...
  weather/             ← yet another
    ...
```

Each database has its own WAL, catalog, manifest, partition files, and
retention policy. Metrics never cross database boundaries. You can delete
the whole `sensors/` folder and nothing else cares.

Databases are created **automatically** on first write. You don't have to
declare them up front.

### Why namespaces instead of one big bucket?

- **Retention is per-database.** `weather` can keep a year while `debug`
  keeps a day.
- **Backup and copy are per-database.** Just `rsync prod/` somewhere.
- **Failure isolation.** A corrupted `debug.wal` doesn't risk `prod` data.
- **Logical separation maps to physical separation.** Easier to reason about,
  easier to inspect.

A typical edge setup has 1–3 databases. The default `drip` collector dumps
everything into one called `metrics`.

---

## A metric is one numeric stream

Inside a database, each **metric** is one named, time-ordered stream of
numbers. So you might have:

```text
sensors/                          ← database
  catalog.json
  → cpu.user                     ← metric (one stream)
  → cpu.system                   ← metric
  → cpu.busy_pct                 ← metric
  → mem.available                ← metric
  → temp.cpu                     ← metric
  → temp.office_dry.mdeg         ← metric
  → disk.sda.read_kbps           ← metric
  → ...
```

A metric is `(timestamp, value)` pairs ordered by time:

```text
temp.office_dry.mdeg:
  2026-06-01 10:00:00.000  21450
  2026-06-01 10:00:10.000  21460
  2026-06-01 10:00:20.000  21470
  ...
```

Each metric has a fixed value type (`int32` or `float32`) decided on first
write. You can't change it later — that's what makes the on-disk format
predictable.

Names are arbitrary strings. NanoTDB doesn't care if you use dots, slashes,
or underscores — `cpu.user`, `cpu_user`, `cpu/user` are all valid metric
names. (`drip` happens to use dot-separated names like `disk.sda.iops`.)

### One database, many metrics

A database with 100 metrics is one folder with one WAL and one set of
partition files. The 100 metrics share the same physical storage, just
identified by a small internal **MetricID** (a `uint16`). The catalog file
maps friendly names like `cpu.user` to their internal IDs.

This is why a Raspberry Pi running 80+ metrics still produces files around
1 MB per day — interleaving metrics into shared compressed pages compresses
very well. For 12 consecutive days of real `nanocli inspect metric` output
from a Pi running `drip`, see
[METRIC_FILES.md → Real example output](METRIC_FILES.md#real-example-output).

---

## A sample is one written point

One write into NanoTDB is one **sample**: a `(timestamp, value)` pair on a
specific metric in a specific database. The line-protocol shape is:

```text
database/metric.name value [timestamp_ns]
```

```text
sensors/temp.cpu 42500 1717238400000000000
sensors/cpu.busy_pct 18.7
weather/pressure.hpa 1013
```

If you don't supply a timestamp, NanoTDB uses the current time. Within a
metric, timestamps must be **monotonically non-decreasing** — out-of-order
samples are rejected at ingest. That's a deliberate constraint: it's what
makes the storage and query paths simple.

---

## The write path: WAL → page → partition file

When you POST a sample, this happens:

```text
"sensors/temp.cpu 42500"
        │
        ▼
  ┌──────────┐    1. append a compact record to sensors.wal
  │   WAL    │       (crash-safe; ~11 bytes for known metrics)
  └────┬─────┘
       │
       ▼
  ┌──────────────────┐   2. add to the in-memory page for
  │ in-memory page   │      the current partition (e.g. today)
  └────┬─────────────┘
       │ when page is full or aged out:
       ▼
  ┌────────────────────┐  3. compress + write the page as one
  │ data-YYYY-MM-DD.dat │     frame into the partition's .dat file
  └────────────────────┘
       │
       ▼
  4. reset the WAL — the data behind it is durable now
```

Three things matter here:

1. **The WAL is small.** It's a single reusable file per database. Once the
   in-memory page lands in a `.dat` file, the WAL gets reset — it doesn't
   grow unboundedly.
2. **The in-memory page accumulates many samples across many metrics.**
   That's where the compression wins come from: one page = many metrics
   interleaved by time = lots of repetition for the compressor to eat.
3. **`.dat` files are append-only and immutable.** Each frame in a `.dat`
   file is a sealed compressed block. They never get rewritten.

---

## Partitions and partition files

A **partition** is a time bucket on disk. By default it's one UTC day:

```text
sensors/
  data-2026-05-30.dat
  data-2026-05-31.dat
  data-2026-06-01.dat
  data-2026-06-02.dat   ← today's partition (still being appended to)
```

You can pick a different partition size per database in
[manifest.toml](CONFIGURATION.md#retention):

| `partition`  | File name pattern         | Good for                          |
|--------------|---------------------------|-----------------------------------|
| `day`        | `data-YYYY-MM-DD.dat`     | Default. Edge telemetry, debug.   |
| `month`      | `data-YYYY-MM.dat`        | Sparse data, rollup destinations. |
| `year`       | `data-YYYY.dat`           | Long-term archives.               |
| `forever`    | `data-forever.dat`        | Tiny configs that never expire.   |

**Retention is just file deletion.** When a day-partitioned database hits
its retention horizon, NanoTDB removes the old `data-*.dat`. No compaction
job, no merge tree, no rewrite cycle.

### What happens when a partition seals

A partition is "sealed" when the engine stops appending to it — typically
because the next sample lands in a *newer* partition (e.g., midnight UTC for
day partitions). After that, the file is closed and only ever read from,
never written.

By default, sealed partitions just sit there in their current
`data-<partition>.dat` form, and queries read from them directly. That's
fine for most workloads.

If you turn on metric files (see next section), sealed partitions get a
second, query-optimized layout written alongside them.

---

## `data-*.dat` vs `metric-*.dat` — what they are and why both exist

This is the part people often miss. NanoTDB can store the same partition's
data in **two layouts**, and the choice is a read-side performance tradeoff.

### `data-*.dat` — the write-path source of truth

`data-*.dat` is what ingestion writes. It's a stream of compressed pages,
and each page contains an interleaved mix of *whatever metrics happened to
get appended together in time*:

```text
data-2026-06-01.dat (raw ingest layout):

  frame 1:  [cpu.user@T1, temp.cpu@T1, mem.avail@T1, cpu.user@T2, ...]
  frame 2:  [temp.cpu@T3, cpu.user@T3, disk.iops@T3, ...]
  frame 3:  [cpu.user@T4, temp.cpu@T4, mem.avail@T4, ...]
  ...
```

That's perfect for the **write** path: just buffer in memory, dump one big
compressed page when it's full, move on.

It's not ideal for **range queries on one metric**, though. If you ask
"give me `temp.cpu` for the last hour", the reader has to walk all the
frames in the partition and pull out just the `temp.cpu` samples from each
interleaved page.

For small/short queries that's fine. For long-horizon scans on one metric
across a big partition, it's wasteful.

### `metric-*.dat` — the query-optimized layout

`metric-2026-06-01.dat` rewrites the same data into per-metric runs:

```text
metric-2026-06-01.dat (query-optimized layout):

  ─── shared time vector ─── (stored once for the whole file)

  metric: cpu.user        → [v1, v2, v3, v4, ...]
  metric: temp.cpu        → [v1, v2, v3, v4, ...]
  metric: mem.available   → [v1, v2, v3, v4, ...]
  metric: disk.sda.iops   → [v1, v2, v3, v4, ...]
  ...
```

Now a range query on `temp.cpu` just walks the `temp.cpu` run directly.
The shared time vector means you don't repeat timestamps per metric (which
is a big size win over the older `v1` metric-file format, which did).

When does NanoTDB use it? At **query** time, NanoTDB always prefers
`metric-*.dat` for a partition when one exists, and falls back to
`data-*.dat` otherwise. That preference is on by default.

When does NanoTDB **build** it? That part is opt-in:

- `[metrics] enabled = false` (default): NanoTDB does **not** auto-convert
  sealed partitions. You can still build metric files explicitly with
  `nanocli build metric`.
- `[metrics] enabled = true`: when a partition is sealed out of the active
  ingest window, NanoTDB best-effort builds `metric-<partition>.dat` for it.

After a successful build, what happens to the source `data-<partition>.dat`
is controlled by `raw_ingest_action`:

| `raw_ingest_action` | Effect on `data-<partition>.dat` after build      |
|---------------------|---------------------------------------------------|
| `keep` (default)    | left in place — you have both layouts             |
| `rename`            | renamed to `raw-<partition>.dat`                  |
| `delete`            | removed                                           |

Keeping both gives you the strongest fallback story (raw is always there).
Rename is useful when you want metric files to be the primary read path
but still want the original around for verification or rebuild. Delete is
the smallest-on-disk option.

For the codec choice and a benchmark script, see
[METRIC_FILES.md](METRIC_FILES.md).

---

## The catalog and the manifest

Two more small files per database, both intentionally human-readable:

**`catalog.json`** — the metric registry. Maps metric names to their
internal `MetricID` and records the fixed value type:

```json
{
  "metrics": [
    {"name": "cpu.user",   "id": 1, "type": "int32"},
    {"name": "cpu.system", "id": 2, "type": "int32"},
    {"name": "temp.cpu",   "id": 3, "type": "int32"},
    ...
  ]
}
```

The WAL uses just the small `MetricID` (2 bytes) in its hot path. The
catalog is what reconstructs the friendly name and type. If you ever want
to know what metrics a database contains, look at `catalog.json` directly —
no server required.

**`manifest.toml`** — per-database settings: retention, partition mode,
WAL behavior, page flush thresholds, rollups. Editable by hand. See
[CONFIGURATION.md](CONFIGURATION.md).

---

## The WAL: what it protects and how to tune it

The **WAL** (`<db>.wal`) protects samples that exist only in memory —
specifically, the in-memory page that's still accumulating.

Once a page is flushed into a `.dat` partition file and no open page still
depends on the data behind the WAL, the WAL gets reset. So at any moment
the WAL only contains "the recent stuff that hasn't been durable-page'd
yet".

The two main durability questions are:

### How aggressively to fsync the WAL

`wal.fsync_policy` in `engine.toml`:

| Value     | Behavior                            | Tradeoff                                                |
|-----------|-------------------------------------|---------------------------------------------------------|
| `segment` | fsync on WAL reset (after a flush)  | Lower SD-card wear. A few seconds of writes at risk on power loss. |
| `always`  | fsync on every WAL append           | Strongest durability. More SD writes per sample.        |

### How aggressively to fsync the page/catalog files

`durability.profile` in `engine.toml`:

| Profile      | Page file fsync | Catalog fsync | Use when…                            |
|--------------|-----------------|---------------|--------------------------------------|
| `strict`     | yes             | yes           | Default. Safe across power loss.     |
| `balanced`   | yes             | no            | Lower overhead, mostly equivalent.   |
| `throughput` | no              | no            | Lowest overhead, most crash risk.    |

### Choosing a posture

**Most resilient (best on power-loss-prone setups, e.g. Pi without UPS):**

```toml
[wal]
fsync_policy = "always"

[durability]
profile = "strict"
```

This is the conservative pick. Every append goes to disk. Most expensive
on SD-card writes.

**SD-friendly middle ground (most edge boxes):**

```toml
[wal]
fsync_policy = "segment"

[durability]
profile = "balanced"
```

Fewer fsyncs, less SD wear. Risks losing a few seconds of in-memory writes
on hard power loss, but the data already flushed into `.dat` files is safe.
For 10-second-cadence telemetry on a Pi, this is usually the right pick.

**Throughput end (host-class machines, less crash risk concern):**

```toml
[wal]
fsync_policy = "segment"

[durability]
profile = "throughput"
```

Lowest write overhead. Recovery is still correct on a clean shutdown.

### Page flush thresholds also affect SD wear

The per-database `[page]` settings decide how long data sits in the WAL
before landing in `.dat` files:

```toml
[page]
max_records = 10000     # samples per in-memory page
max_bytes   = 524288    # byte limit per page
max_age     = "5m"      # wall-clock age limit
```

Larger limits → fewer page flushes → fewer `.dat` writes → less SD wear,
but more in-flight data depending on the WAL for recovery. Smaller limits
→ more frequent flushes → safer recovery posture, but more SD writes.

For a sparse Pi workload (a few dozen samples per second), bumping
`max_age` to `"5m"` or `"10m"` and `max_records` to a few thousand keeps
the SD-write profile mild.

For the full discussion and the SD-card story, see
[RECOVERY.md](RECOVERY.md).

---

## Events — a sibling layer for discrete occurrences

Everything above is about **metrics**: dense, regular, single-numeric-valued
streams. NanoTDB also has an **events** layer for the other shape of data —
discrete things that happen, sometimes carrying a value, sometimes carrying
arbitrary context.

A metric answers: *"what was the temperature every 10 seconds?"*
An event answers: *"the SD-write probe exceeded 500 ms at 14:22:07, and
here's the payload context."*

Events live alongside metrics in the same database, but with their own
files:

```text
sensors/
  catalog.json                — metric catalog
  events.json                 — event catalog                  (NEW per DB, opt-in)
  manifest.toml               — extended with [events]
  sensors.wal                 — metric WAL (unchanged)
  sensors.events.wal          — events WAL                     (NEW)
  data-<partition>.dat        — metric raw pages
  metric-<partition>.dat      — optional query-optimized metric layout
  events-<partition>.dat      — event pages                    (NEW)
```

Three things to know:

- **Events are opt-in per database** via `[events].enabled = true` in the
  manifest. Default is off, so existing databases don't suddenly grow
  events files on upgrade.
- **Each event has a name, a timestamp, an optional typed value
  (`int32`, `float32`, or `none`), and an optional opaque payload**
  (typically JSON). The value type is pinned at first write per event
  name — same rule as metrics.
- **The events layer mirrors the metric layer's crash-safety story:**
  WAL append → in-memory page → flushed partition file, with a strict
  catalog-before-WAL-reset invariant.

### What's the same as metrics

- Partition cadence: events follow the database's configured partition
  mode (`day|month|year|forever`). One `events-<partition>.dat` per window.
- Retention: events files join the partition family, so
  `retention_action = delete` removes them with their metric siblings.
- Catalog-before-WAL-reset: the events catalog is fsynced before the
  events WAL is allowed to reset.
- Inspectable: `nanocli inspect events`, `nanocli inspect events-wal`,
  `nanocli inspect events-catalog` mirror the metric inspect commands.

### What's different

- **Strings live in the payload, not the value.** Events have only
  `none`, `int32`, or `float32` for the typed value. If you want to log
  a deploy SHA or a hostname, put it in the payload — that keeps the
  on-disk value field fixed-width and avoids high-cardinality value
  spaces in future aggregate work.
- **Per-page event-id bitmap.** Every event-page frame carries a
  128-byte bitmap of which `EventID`s appear in it, so name-filtered
  queries skip whole frames without decompressing. (Equivalent to a
  per-frame "index" baked into the header, no sidecar files.)
- **Event-id space is `1..1023`**, separate from metric ID space. The
  cap is a hard architectural constant — the bitmap is sized for
  exactly that range.
- **Page-wide ts ordering is intentionally lax**, while per-event-name
  ordering is strict. The metric Page rejects any out-of-order ts; the
  events page accepts arrival-order interleaving across different event
  names. The per-event-name monotonic rule lives in the events catalog,
  not in the page.

### What you can do with them

Today (Phases 1, 2, 3 shipped):

- Ingest via `POST /api/v1/events` (JSON only — no line protocol form)
- Range-query via `GET /api/v1/events` with name filter, time window,
  and `limit`
- Time-bucketed **count** via `GET /api/v1/events/aggregate` (count is
  the only aggregate in v1; numeric aggregates on event values are
  designed, not built)
- Use `nanocli events` for the offline range query or count aggregation
- Display recent events in an `event_log` dashboard widget filtered by
  name pattern
- Plot numeric (int32/float32) events as scatter points on a
  `line_chart` widget by setting `event_name_pattern` on the series
- Overlay event timestamps as vertical markers on a metric chart via
  `event_overlays` at the widget level

Not yet shipped from the design:

- Numeric aggregates over event values (avg/min/max/sum/percentiles
  on event values) — only `count` is implemented today
- Phase 4 — handler registry for re-emit / webhook / threshold-to-event
  pipelines

For the full byte-level spec, see [EVENTS.md](EVENTS.md). For the
crash-safety properties and the chaos test that asserts them, see
[scripts/events_chaos.py](../scripts/events_chaos.py).

---

## A minute-long mental model

Reading from outermost in:

```text
engine
 └── database (one namespace, own retention)
      ├── catalog.json        — name ↔ MetricID ↔ type        (metrics)
      ├── events.json         — name ↔ EventID  ↔ type        (events, opt-in)
      ├── manifest.toml       — retention, WAL, page, rollups, events for this DB
      ├── <db>.wal            — crash safety for in-memory metric page
      ├── <db>.events.wal     — crash safety for in-memory events page  (opt-in)
      ├── data-<partition>.dat   — metric raw write-order pages
      │                            (always present; queries can read directly)
      ├── metric-<partition>.dat — optional query-optimized metric layout
      │                            (built when [metrics] enabled = true,
      │                             or manually with `nanocli build metric`)
      └── events-<partition>.dat — events pages with per-frame id bitmap
                                    (opt-in; one per partition window)
```

Metric writes go metric WAL → in-memory page → `data-*.dat`. Queries
prefer `metric-*.dat` when available, fall back to `data-*.dat`. Event
writes go events WAL → in-memory events page → `events-*.dat`. Queries
walk the per-frame bitmap to skip non-matching frames without
decompressing. Retention deletes whole partition families (metric and
event files for the same window go together). Both WALs stay small.
All files stay readable. That's the whole shape.

---

## Where to go next

- [HELLO_WORLD.md](HELLO_WORLD.md) — copy/paste the 60-second flow.
- [ARCHITECTURE.md](ARCHITECTURE.md) — the deeper storage walkthrough,
  line-protocol parsing, WAL byte layout, page frame format,
  events-layer data flow.
- [GLOSSARY.md](GLOSSARY.md) — canonical term reference (metrics + events).
- [CONFIGURATION.md](CONFIGURATION.md) — `engine.toml` + `manifest.toml`.
- [RECOVERY.md](RECOVERY.md) — durability tuning in depth.
- [METRIC_FILES.md](METRIC_FILES.md) — codecs, benchmarks, build options.
- [ROLLUPS.md](ROLLUPS.md) — downsampling jobs and backfill.
- [EVENTS.md](EVENTS.md) — events byte spec, ingest/query APIs,
  dashboard integration, crash-safety contract.
