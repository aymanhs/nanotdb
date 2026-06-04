# Events

NanoTDB has a second per-database storage layer for **events**: discrete,
named occurrences with an optional typed value and an optional opaque
payload.

Events are designed to live alongside metrics — same database, same
partition cadence, same retention, same operational properties — while
covering a different shape of data. Where a metric is dense, regular, and
single-valued, an event is sparse, irregular, and may carry context.

This document is the design source-of-truth before implementation. The
storage layout, on-disk byte shapes, catalog rules, and crash-recovery
invariants below are all part of the v1 contract.

For the friendly intro to where events fit alongside metrics, see
[CONCEPTS.md](CONCEPTS.md) (to be extended once events ship).

---

## Why events

Three things metrics don't model well, and that pushed this design:

1. **One-off occurrences.** "An SD-write latency probe crossed 500 ms at
   10:42:17." A metric would record the latency every cycle whether
   anything happened or not; an event records exactly the thing that
   happened, with the value that mattered.
2. **Context attached to a point in time.** A deployment, a config change,
   a sensor calibration. The numeric value is often secondary to "what,
   when, with what context."
3. **Naturally-sparse counts.** "How many `disc.write.slow` per hour" —
   modelable as a metric only by emitting heartbeats; modelable as an
   event natively, and aggregated to a per-window count at query time.

We do *not* want to force these into the metrics layer because:

- Metrics have one fixed-width value per `(timestamp, metric)`. Events
  carry variable-width payloads.
- Metric ordering and density assumptions don't apply.
- The query shape — "show me events in this window" or "count them per
  hour" — is different from raw range scans on dense numeric streams.

---

## Mental model

A **database** has metrics *and* events. Both share retention, partition
mode, and WAL discipline. Both are inspectable offline via `nanocli`.

```text
sensors/
  catalog.json             — metric catalog
  events.json              — event catalog              (NEW)
  manifest.toml            — extended with [events]
  sensors.wal              — metric WAL (unchanged)
  sensors.events.wal       — event WAL                  (NEW, separate file)
  data-<partition>.dat     — metric raw pages
  metric-<partition>.dat   — optional query-optimized metric layout
  events-<partition>.dat   — event pages                (NEW)
```

The separation principle: **two independent storage layers, one shared
database namespace.** Disabling events on a DB leaves the metric storage
untouched. Disabling metrics is not a thing; metrics are core. Events are
opt-in per database via `[events].enabled`.

---

## Data model

### An event

```text
Event {
  name       string         — user-supplied identifier (e.g. "disc.write.slow")
  ts         int64          — Unix nanoseconds, monotonically non-decreasing per name
  value      none|i32|f32   — optional typed value, type fixed at first write
  payload    []byte         — optional opaque bytes (typically JSON)
}
```

Three things to highlight:

- **The value type is fixed at first write** for a given event name, and
  pinned in `events.json`. Same rule as metrics.
- **String values are not first-class.** Strings belong in the payload.
  This keeps the on-disk record fixed-width-when-present and avoids
  high-cardinality value-space hazards in future aggregate work.
- **Payload is opaque.** NanoTDB stores the bytes the caller sent. No
  parsing, no validation, no schema. If you sent valid JSON, you'll get
  valid JSON back; if you sent base64 protobuf, you'll get those bytes.

### EventID

`EventID` is a `uint16` in the range `1..1023`, independent of `MetricID`.
Same cap as metrics. Assigned automatically on first write, persisted in
the events catalog, never reused.

EventID and MetricID spaces are independent — no collision risk because
they live in separate catalogs and separate WALs/pages.

**The `1023` cap is a hard architectural constant, not a tuning knob.**
The page header's `event_id_bitmap` is sized for exactly 1023 IDs
(`ceil(1023/8) = 128` bytes), the EventID wire field is bounded to that
range, and several read-path optimizations assume it. Raising the cap
would require a coordinated change to the page format, WAL replay, and
inspect tooling. Mirrors `MaxMetricsPerDatabase` in
[catalog.go](../internal/engine/catalog.go) on the metric side.

The constant lives in code as `MaxEventsPerDatabase = 1023`. Attempting
to register a 1024th event name returns `ErrTooManyEvents`.

### Event catalog (`events.json`)

```json
{
  "events": [
    {"name": "disc.write.slow",      "id": 1, "value_type": "int32"},
    {"name": "temp.office.overheat", "id": 2, "value_type": "float32"},
    {"name": "heartbeat",            "id": 3, "value_type": "none"}
  ]
}
```

Catalog is human-readable, JSON, atomic-temp+rename writes, validated on
load. Mirrors [internal/engine/catalog.go](../internal/engine/catalog.go).

Validation rules on load (hard failures):

- duplicate name
- duplicate id
- `id = 0`
- invalid `value_type` (must be `none|int32|float32`)
- empty name

---

## Storage layout

### Files added per database

- `events.json` — event catalog
- `sensors.events.wal` — events WAL (one reusable file)
- `events-<partition>.dat` — durable event pages, one per partition window

### Partition cadence and retention

Events use the **same partition mode** the database is configured for
(`day|month|year|forever` from `[retention].partition`). One
`events-<partition>.dat` per partition window, sealed when the window
closes.

Events files **join the partition family** for retention. When
`retention_action = delete`, the events file is removed alongside the
`data-*.dat`, `metric-*.dat`, and `raw-*.dat` files for the same expired
partition. When `retention_action = archive`, all family members are
folded into the same tar bucket. `keep` leaves everything in place.

---

## WAL

### Why a separate WAL

Yesterday's design decision: events get their own `<db>.events.wal`
rather than sharing the metric WAL. Rationale:

- The existing WAL has no header / version / magic bytes on disk. A new
  record kind or stolen flag bit would be a forward-compatibility break
  (downgrade reads garbage), with no version field to detect it.
- WAL format bumps are already deferred in the project (per
  [CHANGELOG.md](../CHANGELOG.md) — per-record CRC32 is the standing
  example).
- A separate file is the existing "one file per purpose" pattern
  (`catalog.json`, `manifest.toml`, `<db>.wal` — now `<db>.events.wal`).
- The metric WAL code stays mature and untouched; event WAL is new code
  in a new file. No risk to the durability story we already ship for
  metrics.
- Independent enable/disable, independent failure isolation, optionally
  independent fsync policy in future.

Costs are small: one extra file handle per DB, one extra fsync when both
WALs are written in the same batch.

### Events WAL record format

```text
[uvarint: record_len]
  EventID      uint16 LE
  TS           int64  LE
  Flags        uint8           bit 7 = newEvent (name + value_type follow)
                                bit 6 = newBaseline (8-byte baseline TS follows)
  [if Flags & newBaseline]
    BaselineTS int64 LE
  [if Flags & newEvent]
    NameLen    uint8           (≤ 255)
    Name       NameLen bytes
    ValueType  uint8           (0=none, 1=int32, 2=float32)
  [Value present iff catalog ValueType ≠ none]
    Value      4 bytes LE      (int32 or float32 per catalog ValueType)
  PayloadLen   uvarint
  Payload      PayloadLen bytes
```

**Hot-path size** (known event, has int32 value, no payload):

```text
length prefix:                1 byte
EventID:                      2
TS:                           8
Flags:                        1
Value:                        4
PayloadLen (varint 0):        1
                            ----
total:                       17 bytes
```

Comparable in spirit to the metric WAL's ~11-byte hot-path record.

**Notes on the format:**

- `Flags` is a fresh, dedicated byte on this new WAL file. We don't reuse
  the metric-WAL `CompactTL` byte definitions; this is a parallel format
  for a parallel file.
- `ValueType` is **only carried inline on `newEvent` records**, identical
  to how the metric WAL only carries it on `newMetric` records. Subsequent
  records resolve the value type by looking up the EventID in the events
  catalog. Mirrors [wal.go:611-623](../internal/engine/wal.go#L611-L623).
- `Value` is present **iff the catalog `value_type` for this event is not
  `none`**. A `none`-typed event records only `(EventID, TS, Payload)`.
- Optional TS-delta compression (mirroring the metric WAL's 3-byte delta +
  baseline scheme) can be added in a later WAL revision if event volumes
  justify it. Initial implementation uses the full 8-byte TS for
  simplicity; events are sparse so the savings are limited anyway.

### Replay invariant

Events WAL replay must reconstruct any in-memory event-catalog entry
whose name+type was introduced by a `newEvent` record. This is the
recovery story for "crashed after WAL append, before catalog write."

If a non-`newEvent` record references an EventID not in the (in-memory or
on-disk) catalog at replay time, that is a **hard error** — same rule as
the metric WAL's catalog-required policy ([CHANGELOG.md](../CHANGELOG.md)
"WAL integrity" fix from `1.4` Unreleased).

### Crash-tail policy

The only legitimate crash-tail signal is the **outer uvarint length
prefix** being short or pointing past the end of the file. By the time
a record's payload bytes reach the decoder, the writer has declared a
length that the file already satisfies — so any decoder-level error
(truncated inner field, invalid value-type byte, reserved-flag bit set,
trailing bytes, catalog/inline type mismatch) means the writer and
reader disagree about layout. Those are corruption and surface as hard
errors.

Concretely: `RecordsWithCatalog` breaks the scan silently only when the
outer length prefix is incomplete or extends past EOF. Every error
returned by the per-record decoder propagates up unchanged.

---

## Catalog persistence — mirroring the metric pattern

Critical correctness rule, taken straight from the metric catalog:
**the events catalog must be persisted before the events WAL is reset.**

In-memory flow when a new event is registered:

1. `AddEvent("sensors", "disc.write.slow", ts, value, payload)` arrives.
2. Engine looks up `disc.write.slow` in the in-memory event catalog.
3. If missing, assigns the next `EventID`, inserts the in-memory entry,
   sets `dirty = true`. *No fsync to `events.json` yet.*
4. WAL append: record carries the `newEvent` flag bit and the inline
   `(name, value_type)`. *This is the durable ledger between in-memory
   assignment and catalog rewrite.*
5. In-memory page accumulates the event.

The events catalog is then persisted at the same **four checkpoint
sites** the metric catalog uses today (see
[engine.go:586,660,2389,2475](../internal/engine/engine.go#L586) for the
metric-catalog calls — events will sit alongside each one):

| Checkpoint                              | Why                                                                 |
|-----------------------------------------|---------------------------------------------------------------------|
| `Engine.Close()`                        | Clean shutdown                                                      |
| Engine-wide page flush + WAL reset prep | Catalog must be durable before any WAL reset                        |
| Per-DB WAL-reset-eligibility path       | Same rule, narrower scope                                           |
| Per-DB flush/reset path                 | Same rule, narrower scope                                           |

At each site, immediately before any `db.eventsWAL.Reset()`:

```go
if db.eventCatalog != nil && db.eventCatalog.IsDirty() {
    if err := db.eventCatalog.WriteCatalog(); err != nil {
        return fmt.Errorf("write events catalog for database %q: %w", name, err)
    }
}
```

Same temp+fsync+rename+dir-fsync discipline as the metric catalog —
inherited automatically by cloning the catalog implementation.

---

## Page format

### In-memory event page

One open page per `(database, partition)` pair. Accumulates events in
write order. Flushes when **any** of these thresholds are crossed:

- `events.page.max_records`
- `events.page.max_bytes` (total compressed-payload estimate)
- `events.page.max_age` (wall-clock age of the page)
- `events.max_in_memory_bytes` (spike-protection ceiling; see below)

### `events-<partition>.dat` frame layout

```text
Frame:
  FrameHeader (variable, length-prefixed):
    start_ts            int64 LE
    end_ts              int64 LE
    record_count        uint32 LE
    event_id_bitmap     128 bytes        — 1 bit per EventID in 1..1023
    compressed_len      uint32 LE
  Payload:
    compressed block    S2 or zstd (codec per [events] config)
  Trailer:
    payload_crc32       uint32 LE
```

Decoded payload is a flat sequence of records using the same shape as the
WAL record body, **minus** the inline new-event metadata (since catalog
is authoritative for sealed pages):

```text
PageRecord:
  EventID    uint16 LE
  TS         int64  LE
  Value      0 or 4 bytes (per catalog value_type)
  PayloadLen uvarint
  Payload    PayloadLen bytes
```

**Per-page event-id bitmap.** The 128-byte bitmap in the header lets
name-filtered queries skip whole pages without decompressing. For 1023
possible IDs that's `ceil(1023/8) = 128` bytes — fixed cost per page.

When the query is `name = disc.*` and `disc.*` resolves to `EventID ∈
{1,7,12}`, the scanner ANDs that set against the page bitmap before
considering the page. Most pages on a sparse events stream will be
skippable without reading their compressed payload.

### Sealing and retention

`events-<partition>.dat` seals when its partition window closes (same
trigger as `data-<partition>.dat`). Once sealed it's immutable.

No `metric-*.dat`-style query-optimized rewrite layer for events in v1.
The page format is already per-event-id-indexable via the header bitmap,
and event volumes are expected to be low. If we ever see workloads that
need it, we can add an `events-optimized-<partition>.dat` layer later
behind the same preference-and-fallback rule that metric files use.

---

## Spike safety

The "we don't expect many but it can happen" failure mode is the
dangerous one. Two knobs, one hard cap, plus a back-pressure rule:

```toml
[events]
enabled = true
max_payload_bytes = 4096      # per-event payload size ceiling
max_in_memory_bytes = 1048576 # force-flush threshold for the open page
```

The 1023-event-name cap per database is a hard architectural constant
(`MaxEventsPerDatabase`), not a config knob — see [EventID](#eventid).

Behavior under flood:

1. **Catalog cap** — once 1023 distinct event names are registered,
   attempts to register a *new* event name are rejected with
   `ErrTooManyEvents`. Existing events still ingest normally.
2. **Payload cap** — payloads larger than `max_payload_bytes` are
   rejected at ingress with a 400-class error. Same rule on the engine
   API. Protects against runaway emitters (think: someone dumping a
   stack trace into every event).
3. **In-memory ceiling** — if the open events page exceeds
   `max_in_memory_bytes`, the engine **force-flushes** the page before
   accepting more. This back-pressures the flood through normal
   page-flush mechanics rather than dropping, OOMing, or accepting
   silently.
4. **Stats counters** — emitted to the internal stats DB:
   `internal/nanotdb/<db>/events/wal_appends`,
   `events/payload_rejected_oversized`,
   `events/page_force_flushes`. An operator can *see* the flood.

Deliberately **not** included by default:

- No rate limiter. "Store safely" means "don't crash, don't lose data,
  don't fill the disk." It does not mean throttle.
- No payload-content validation. We are storage, not a schema validator.

---

## Ingest API

### `POST /api/v1/events`

**JSON form** (preferred for non-trivial payloads):

```http
POST /api/v1/events
Content-Type: application/json

[
  {"db":"sensors","name":"disc.write.slow","value":542,"payload":{"path":"/tmp"}},
  {"db":"sensors","name":"temp.office.overheat","value":31.2},
  {"db":"sensors","name":"heartbeat"}
]
```

Field rules:

- `db` and `name` required.
- `ts` optional; defaults to server-side `time.Now()` Unix ns.
- `value` optional; type pinned at first write per event name.
- `payload` optional. Encoded into bytes as `json.Marshal(payload)` for
  the JSON form. Capped by `max_payload_bytes` after encoding.

**Line-protocol form** (for shell-friendly emitters):

```text
sensors/disc.write.slow 542
sensors/disc.write.slow 542 1717238400000000000
sensors/disc.write.slow 542 1717238400000000000 {"path":"/tmp"}
sensors/heartbeat
```

Parser rules:

- First token: `db/name`.
- Second token (optional): the numeric value, if the catalog
  `value_type` is `int32` or `float32`. Forbidden if `value_type = none`.
- Third token (optional): timestamp in Unix ns.
- Everything after the third token (optional): raw payload, taken
  verbatim from the start of the payload byte to the end of the line.

To keep the parser unambiguous: if a payload is present, a timestamp must
be present. If you want "now" with a payload, supply `0` as the ts and
the engine substitutes the current time on accept.

### Engine method (for embedders)

```go
e.AddEvent(
    db string,
    name string,
    ts int64,            // 0 = use time.Now()
    value any,           // nil, int32, or float32; type-checked against catalog
    payload []byte,      // nil for no payload
) error
```

Errors:

- `ErrTooManyEvents` — registered event count has hit the `MaxEventsPerDatabase` cap (1023).
- `ErrEventTypeMismatch` — value type doesn't match catalog.
- `ErrEventPayloadTooLarge` — payload exceeds `max_payload_bytes`.
- `ErrEventTimestampTooOld` — ts < last accepted ts for this event name
  (mirrors the per-metric monotonic rule).

---

## Query API

### `GET /api/v1/events`

Range query with optional name filter and pagination.

```http
GET /api/v1/events
    ?db=sensors
    &name=disc.*           (optional; wildcard pattern; default = all events)
    &start=2026-06-01T00:00:00Z
    &end=2026-06-02T00:00:00Z
    &limit=100              (optional; default 100, hard cap 1000)
    &cursor=<opaque>        (optional; from previous response)
```

Response:

```json
{
  "status": "success",
  "data": {
    "resultType": "events",
    "result": [
      {
        "name": "disc.write.slow",
        "ts":   1717238412000000000,
        "value": 542,
        "payload": {"path": "/tmp"}
      },
      {
        "name": "disc.write.slow",
        "ts":   1717238531000000000,
        "value": 870
      }
    ],
    "next_cursor": "..."
  }
}
```

Response shape rules:

- `value` field omitted when the event's `value_type = none`.
- `payload` field omitted when no payload was stored.
- `payload` returned as parsed JSON if it parses cleanly; as
  `{"raw_base64": "..."}` otherwise. (Keeps the response valid JSON
  even when payloads happen to be non-JSON bytes.)
- `next_cursor` present when more results exist; absent on the final page.

### `GET /api/v1/events/aggregate` *(Phase 2)*

Time-bucketed count of matching events. Response shape mirrors the
metric `query_range` aggregate response so the dashboard can chart it
with the existing line-chart widget.

```http
GET /api/v1/events/aggregate
    ?db=sensors
    &name=disc.*
    &start=2026-06-01T00:00:00Z
    &end=2026-06-02T00:00:00Z
    &window=1h
    &aggregate=count            (count only in v1; see Aggregate semantics below)
```

### `GET /api/v1/events/catalog`

List registered event names + IDs + value types.

```http
GET /api/v1/events/catalog?db=sensors
```

Response shape mirrors `GET /api/v1/metrics?db=...&details=true`.

---

## nanocli

Mirrors the metric-side CLI surface.

```text
nanocli inspect events          --root <dir> --db <name> [--verbose] [--json]
nanocli inspect events-wal      --root <dir> --db <name> [--verbose] [--json]
nanocli inspect events-catalog  --root <dir> --db <name> [--json]

nanocli events                  --root <dir> --db <name>
                                [--name <pattern>] [--start <t|d>] [--end <t>]
                                [--limit N]
                                [--aggregate count --window <duration>]
                                [--format table|json]
```

- `inspect events` — per-file/per-page table: bytes, record_count,
  event_id_set, start/duration. Verbose mode validates frame payloads.
- `inspect events-wal` — per-file size, decode stats, tail diagnostics in
  verbose mode. Mirrors `inspect wal`.
- `inspect events-catalog` — list of registered events with id and type.
- `events` — range query or aggregate count, table/json output. Pagination
  is local — the CLI auto-pages until the requested range is exhausted
  or `--limit` is hit.

---

## Dashboard integration

### Numeric-valued events as line-chart series

Numeric (`int32`/`float32`) events plot naturally as a discrete series
where each event is one point. No new widget type needed — extend the
existing `line_chart` series shape:

```json
{
  "type": "line_chart",
  "title": "Slow-write latency events",
  "series": [
    {"label": "ms", "event": "disc.write.slow"}
  ]
}
```

The `event` field, when present, makes the series event-backed:
y-coordinate is the event's typed value, x-coordinate is its timestamp.
Defaults to scatter-point style (not connected lines) because events are
sparse occurrences, not a continuous signal.

For event counts, use the aggregate-event-backed form:

```json
{
  "label": "Slow writes / h",
  "event": "disc.write.slow",
  "aggregate": "count",
  "window": "1h"
}
```

### Event overlays on metric charts *(Phase 3)*

Optional `event_overlays` field on `line_chart` widgets renders vertical
markers at each event timestamp. Hover surfaces the name, ts, value,
and payload.

```json
{
  "type": "line_chart",
  "title": "CPU temp with overheat events",
  "series": [{"label": "CPU", "metric": "temp.cpu"}],
  "event_overlays": [
    {"name": "temp.office.overheat", "color": "#c00"}
  ]
}
```

### Event-log widget *(Phase 3)*

New widget type `event_log` for a tabular live feed:

```json
{
  "type": "event_log",
  "title": "Recent events",
  "lookback": "24h",
  "name_pattern": "*",
  "limit": 50
}
```

---

## Aggregate semantics

For Phase 2, only `count` is universal:

| Aggregate              | `none` value | `int32` value | `float32` value |
|------------------------|--------------|---------------|-----------------|
| `count`                | yes          | yes           | yes             |
| `min` / `max`          | rejected     | yes           | yes             |
| `avg` / `sum`          | rejected     | yes           | yes             |
| `median` / `p50/95/99` | rejected     | yes           | yes             |

For value-typed events, the numeric aggregate set matches the metric
aggregate set, so the query and dashboard surfaces are uniform.

For `none`-typed events, only `count` is meaningful. Any other aggregate
returns a clean error rather than silently zero-filling.

---

## Future hooks (designed for, not built now)

A handler registry attached to event IDs:

```go
e.RegisterEventHandler("disc.write.slow", func(ev Event) {
    // re-emit as a metric, forward to a webhook, write a log, whatever.
})
```

Properties:

- Handlers fire **after** WAL append + in-memory page enqueue + catalog
  resolution.
- Asynchronous, off a per-DB bounded channel. Handler failure can never
  block or fail ingest.
- Drop-and-count on channel full; surfaced via
  `internal/nanotdb/<db>/events/handler_drops`.
- Handler set is in-memory only; not persisted in `events.json`.
  Re-register on engine open.

This unlocks (without committing to any of them now):

- **Threshold → event** — a tiny built-in that watches a metric and
  emits an event when crossed.
- **Event → metric promotion** — a handler that increments a counter
  metric for each event of a given name. Lets numeric-event ingest also
  participate in dense metric workflows.
- **Event → external webhook** — for alerting integrations.

A *different* future path: rollups against an events stream produce a
per-window count series, persisted as a real `int32` metric in a
destination DB. That's the "promote event counts to metrics" use case
without a handler — just a `[rollups]` entry whose source is the events
layer. Worth keeping the manifest schema open to that even if v1 doesn't
ship the implementation.

---

## Phased delivery

Each phase is independently shippable and useful.

**Phase 1 — Storage + ingest + basic query.** Catalog, events WAL with
replay, `events-*.dat` page format with id-set header, `POST/GET
/api/v1/events`, `GET /api/v1/events/catalog`, `nanocli inspect events`,
`nanocli inspect events-wal`, `nanocli inspect events-catalog`, retention
joins events files to the partition family. *Phase 1 alone is "I can log
events and look at them later."*

**Phase 2 — Aggregate counts.** `GET /api/v1/events/aggregate`,
`nanocli events --aggregate count`, numeric-valued events as line-chart
series in dashboards.

**Phase 3 — Dashboard richness.** `event_overlays` field on `line_chart`,
new `event_log` widget type, editor support, validation.

**Phase 4 — Handler registry.** `RegisterEventHandler`, async dispatch,
the first built-in handler (threshold→event), drop-counter telemetry.

---

## Configuration

### `engine.toml` defaults

Mirrors the existing `[manifest_defaults]` pattern.

```toml
[manifest_defaults.events]
enabled = false                # NEW: opt-in per DB
max_payload_bytes = 4096
max_in_memory_bytes = 1048576

[manifest_defaults.events.page]
max_records = 1000
max_bytes = 65536
max_age = "1h"

[manifest_defaults.events.wal]
enabled = true
max_segment_size = 16777216    # 16 MiB — smaller than metric WAL
# fsync_policy inherits from [wal].fsync_policy unless set
```

The 1023 cap on registered event names per database is a hard
architectural constant (`MaxEventsPerDatabase`), not a config knob.

### `manifest.toml` per-database

Same shape, no `manifest_defaults` prefix. Applies on database creation;
existing databases are not retroactively rewritten when defaults change
(consistent with the existing metric-side rule).

---

## Defaults at a glance

| Knob                                | Default | Rationale                                       |
|-------------------------------------|---------|-------------------------------------------------|
| `[events].enabled`                  | `false` | Opt-in per DB; no surprise files on upgrade.    |
| `[events].max_payload_bytes`        | `4096`  | Bounds runaway emitters; large enough for typical JSON. |
| `[events].max_in_memory_bytes`      | `1 MiB` | Spike back-pressure ceiling.                    |
| `[events.page].max_age`             | `1h`    | Longer than metrics — events are sparse.        |
| `[events.wal].max_segment_size`     | `16 MiB`| Smaller than metric WAL — events are sparse.    |
| `[events.wal].fsync_policy`         | inherit | Inherits from `[wal].fsync_policy`.             |
| Event ordering rule                 | monotonic per name | Mirrors per-metric rule. Equal ts allowed. |
| Event WAL value-type carry          | new-event records only | Mirrors metric WAL new-metric records. |
| Value types supported               | `none, int32, float32` | Strings live in payload.            |

Architectural constants (not knobs):

| Constant                | Value | Rationale                                                 |
|-------------------------|-------|-----------------------------------------------------------|
| `MaxEventsPerDatabase`  | 1023  | Page header bitmap and EventID wire field are sized for this range. Mirrors `MaxMetricsPerDatabase`. |

---

## Crash-safety contract

These properties must hold:

1. **Catalog before WAL reset.** Events catalog is fsync'd to disk before
   any events WAL is allowed to reset. Same rule as metric catalog.
2. **Replay reconstructs in-memory catalog.** Any `newEvent` record in
   the WAL re-creates its in-memory catalog entry on replay.
3. **Unknown EventID without new-event flag is a hard error.** Replay
   does not silently coerce, does not default a value type, does not
   skip the record.
4. **Value type mismatch between WAL and catalog is a hard error.** On
   `newEvent` records, the inline `ValueType` must equal the catalog
   entry if one already exists; if not, the WAL record is taken as the
   source of truth and the catalog is updated.
5. **Per-event-name monotonicity.** Replay rejects an event record whose
   ts is below the last accepted ts for that name, same as metrics. The
   rule is enforced at the catalog level (via `EventCatalog.LastTS`),
   not at the page level — different event names may interleave with
   reordered timestamps within an in-memory page, and the page's
   `start_ts`/`end_ts` track min/max ts (not first/last record's ts).
   This is intentional: events are sparse and often arrive from multiple
   threads, and a page-wide monotonic rule would force expensive
   flush-and-retry rotations on every interleaving.

Phase 1 test plan extends `scripts/first_test_chaos.py` (or adds an
`events_chaos.py`) to interleave events with metrics, kill -9 at random
points, restart, and assert all five properties above.

---

## What's not in v1

For clarity, the things this design explicitly does not include in
Phase 1–3:

- String-valued events (use the payload).
- Per-event-name retention overrides (use a separate DB if you need it).
- Payload schema validation (we are storage, not a schema service).
- Query-optimized events-file rewrite layer (no events analogue of
  `metric-*.dat`; not needed at expected volumes).
- Per-record CRC32 on the events WAL. Deferred for the same reason it's
  deferred on the metric WAL — needs a format-version bump that the
  events WAL doesn't have a slot for yet. Page-frame CRC32 is still
  enforced on `events-*.dat` payloads.
- Rate limiting at ingress. Spike behavior is bounded by payload caps,
  in-memory force-flush, and stats visibility.
- The handler registry (Phase 4, designed-for-only in this doc).

---

## File touch list for Phase 1

For the implementation review:

**New files**

- `internal/engine/events.go` — `Event`, `EventID`, errors.
- `internal/engine/events_catalog.go` — `EventCatalog`, `WriteCatalog`,
  `WriteCatalogTo`, load+validation.
- `internal/engine/events_wal.go` — record format, append, replay.
- `internal/engine/events_page.go` — in-memory accumulator + flush rules.
- `internal/engine/events_file.go` — `events-<partition>.dat` writer and
  reader, frame header with event-id bitmap, payload CRC.

**Modified files**

- `internal/engine/engine.go` — `AddEvent`, `QueryEvents`, four
  catalog-checkpoint sites grow a sibling `eventCatalog.WriteCatalog()`
  call, `Engine.Close()` flushes both WALs, partition seal also seals
  the events page, retention scans include `events-*.dat`.
- `internal/engine/config*.go` — `EngineConfigEvents`,
  `EngineConfigEventsPage`, `EngineConfigEventsWAL`,
  `manifest_defaults.events.*` mapping.
- `internal/web/...` (or `internal/server/...`) — `POST /api/v1/events`,
  `GET /api/v1/events`, `GET /api/v1/events/catalog`.
- `cmd/nanocli/...` — `inspect events`, `inspect events-wal`,
  `inspect events-catalog`, `events` query command.
- `docs/CONFIGURATION.md` — `[events]` and `[events.wal]` sections.
- `docs/HTTP_API.md` — new endpoints.
- `docs/NANOCLI.md` — new inspect + query commands.
- `docs/ARCHITECTURE.md` — on-disk layout block.
- `docs/GLOSSARY.md` — `Event`, `EventID`, `Event Catalog`, `Events WAL`,
  `Events Page File`.
- `docs/LAWS.md` — extend LAW 0/1/2 to cover events.
- `docs/CONCEPTS.md` — friendly walkthrough section after metrics.
- `CHANGELOG.md` — Unreleased / Added.

---

## See also

- [CONCEPTS.md](CONCEPTS.md) — how metrics and pages and the WAL fit
  together; events follow the same model.
- [ARCHITECTURE.md](ARCHITECTURE.md) — storage and query walkthrough.
- [RECOVERY.md](RECOVERY.md) — WAL behavior and durability tuning; the
  same fsync policy and durability profile apply to the events WAL.
- [CONFIGURATION.md](CONFIGURATION.md) — `engine.toml` and
  `manifest.toml` reference.
- [GLOSSARY.md](GLOSSARY.md) — canonical terms (will gain event entries
  in Phase 1).
- Metric catalog reference points used during this design:
  [catalog.go](../internal/engine/catalog.go),
  [wal.go](../internal/engine/wal.go),
  [engine.go](../internal/engine/engine.go).
