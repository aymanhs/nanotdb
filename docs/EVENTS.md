# Events

NanoTDB has a second per-database storage layer for **events**: discrete,
named occurrences with an optional typed value and an optional opaque
payload.

Events are designed to live alongside metrics — same database, same
partition cadence, same retention, same operational properties — while
covering a different shape of data. Where a metric is dense, regular, and
single-valued, an event is sparse, irregular, and may carry context.

This document is the canonical reference for the events layer's storage
layout, on-disk byte shapes, catalog rules, crash-recovery invariants,
HTTP / engine APIs, and `nanocli` surface. Phases 1, 2, and 3 are shipped
(see the "Phased delivery" section). Phase 4 (handler registry) is
designed-for-only.

For the friendly intro to where events fit alongside metrics, see
[CONCEPTS.md](CONCEPTS.md).

NanoTDB and `drip` both use this events layer to emit their own
lifecycle, partition, and target-state events into the `internal`
database. See [INTERNAL_EVENTS.md](INTERNAL_EVENTS.md) for the
internal-events spec, group catalog, runtime toggle, and per-event
payload shapes.

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

Events get their own `<db>.events.wal` rather than sharing the metric WAL.
Rationale:

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
  TS           int64  LE       (full 8-byte timestamp; no delta encoding in v1)
  Flags        uint8           bit 7 = newEvent (name + value_type follow inline)
                                bits 0..6 reserved (must be zero in v1)
  [if Flags & newEvent]
    NameLen    uint8           (≤ 255)
    Name       NameLen bytes
    ValueType  uint8           (0=none, 1=int32, 2=float32)
  [Value present iff catalog ValueType ≠ none]
    Value      4 bytes LE      (int32 or float32 per catalog ValueType)
  PayloadLen   uvarint
  Payload      PayloadLen bytes
```

The constants are `walEventNewEvent = 0x80` and `walEventReservedBits = 0x7F`
in [events_wal.go](../internal/engine/events_wal.go). Any reserved-bit set in
the flag byte is a hard parse error at replay (forward-compat guard).

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
- TS-delta compression (mirroring the metric WAL's 3-byte delta + baseline
  scheme) is deliberately not in v1. Events are sparse, so the metric WAL's
  per-record savings would rarely outweigh the baseline overhead. A future
  WAL revision can revisit if real event volumes justify it.

### Replay invariant

Events WAL replay must reconstruct any in-memory event-catalog entry
whose name+type was introduced by a `newEvent` record. This is the
recovery story for "crashed after WAL append, before catalog write."

If a non-`newEvent` record references an EventID not in the (in-memory or
on-disk) catalog at replay time, that is a **hard error**, surfaced as
the `ErrEventsWALUnknownEventID` sentinel — same discipline as the
metric WAL's catalog-required policy (the 1.4 fix "WAL replay now
fails cleanly on unknown or invalid metric type information instead of
guessing").

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

The events catalog is persisted at the same checkpoint sites as the
metric catalog. The shipped engine uses a `writeEventCatalogIfDirty(db)`
helper at every site where `db.catalog.WriteCatalog()` runs:

| Checkpoint                              | Where (engine.go)                                | Why                                                       |
|-----------------------------------------|--------------------------------------------------|-----------------------------------------------------------|
| `Engine.Close()`                        | catalog write block in the close path             | Clean shutdown                                            |
| Engine-wide flush + WAL reset prep      | `flushDatabasesLocked`                           | Catalog must be durable before any WAL reset              |
| Per-DB WAL-reset-eligibility path       | `maybeResetWAL` / `maybeResetEventsWAL`           | Same rule, narrower scope                                 |
| Per-DB partition flush path             | `writePageWithOptions` / events page-flush path  | Same rule, narrower scope                                 |

The helper has the same temp+fsync+rename+dir-fsync discipline as
`Catalog.WriteCatalog()`:

```go
if db.eventCatalog != nil && db.eventCatalog.IsDirty() {
    if err := db.eventCatalog.WriteCatalog(); err != nil {
        return fmt.Errorf("write events catalog for database %q: %w", name, err)
    }
}
```

This is the property `scripts/events_chaos.py` asserts after every
graceful checkpoint shutdown: the `<db>.events.wal` must be empty
and `events.json` must be non-empty before the engine returns from
`Close()`. A regression here is what surfaced and was fixed during the
chaos-test introduction.

---

## Page format

### In-memory event page

One open page per `(database, partition)` pair. Accumulates events in
write order. Flushes when **any** of these thresholds are crossed:

- `events.page.max_records`
- `events.page.max_bytes` (rough uncompressed in-memory byte estimate:
  `len(records) × ~15 + sum(payload_bytes)`)
- `events.page.max_age` (wall-clock age of the page)
- `events.max_in_memory_bytes` (spike-protection ceiling; see below)

### `events-<partition>.dat` frame layout

A frame is exactly **152 bytes of fixed header** followed by the
S2-compressed payload and a 4-byte CRC32 trailer. The header is NOT
length-prefixed — its size is a compile-time constant
(`EventsFrameHeaderBytes` in
[events_page.go](../internal/engine/events_page.go)).

```text
Frame:
  FrameHeader (fixed 152 bytes):
    start_ts            int64 LE    (8 bytes)
    end_ts              int64 LE    (8 bytes)
    record_count        uint32 LE   (4 bytes)
    event_id_bitmap     128 bytes   (1 bit per EventID slot in 0..1023;
                                     bit 0 is always 0 since EventID 0 is reserved)
    compressed_len      uint32 LE   (4 bytes)
  Payload:
    compressed block    S2-compressed (codec is hardcoded to S2 in v1)
  Trailer:
    payload_crc32       uint32 LE   (4 bytes)
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
4. **Visibility.** Spike conditions surface through the existing engine
   inspection routes (engine-view, `nanocli inspect events`,
   `nanocli inspect events-wal`) rather than dedicated stats counters in
   v1. A future change is tracked in [TODO.md](TODO.md) to emit
   per-DB events counters into the internal stats database
   (`internal/nanotdb/<db>/events/*`) so the flood is observable from
   the metrics surface too.

Deliberately **not** included by default:

- No rate limiter. "Store safely" means "don't crash, don't lose data,
  don't fill the disk." It does not mean throttle.
- No payload-content validation. We are storage, not a schema validator.

---

## Ingest API

### `POST /api/v1/events`

Body is a JSON array (or a single object) of event records. The endpoint
accepts `Content-Type: application/json` (or any `application/json;...`
variant), and also accepts requests with no `Content-Type` header at all
for convenience. Any other content type is rejected. There is no
line-protocol form for events ingest — the LP wire format wouldn't
unambiguously carry the payload field, so we use JSON instead.

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
- `ts` optional; missing/null → engine substitutes `time.Now()` Unix ns.
- `value` optional. Missing/null → none-typed event. JSON integer → `int32`
  (whole numbers within int32 range). JSON number with a fractional or
  exponent part → `float32`. JSON strings, booleans, arrays, and objects
  are rejected — strings belong in the payload.
- `payload` optional. Encoded as `json.Marshal(payload)` and handed to
  the engine as opaque bytes. Capped by `max_payload_bytes` after
  encoding.

The endpoint returns a `vmResponse` with `data.imported` set to the count
of accepted events. The batch is all-or-nothing: any per-event error
returns a 4xx and aborts the import without partially applying any
earlier records.

A request whose target database does not have `[events].enabled = true`
returns HTTP 409 with `ErrEventsDisabled`.

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

- `ErrEventsDisabled` — target database does not have `[events].enabled = true`.
- `ErrTooManyEvents` — registered event count has hit the `MaxEventsPerDatabase` cap (1023).
- `ErrEventTypeMismatch` — value type doesn't match catalog.
- `ErrEventPayloadTooLarge` — payload exceeds `max_payload_bytes`.
- `ErrEventNameEmpty` / `ErrEventNameTooLong` — name fails the
  `1..MaxEventNameLen` byte range check.
- Per-event-name monotonic-ts violation surfaces as an inline error
  (`stale event rejected for <db>/<name>: ts=… < last=…`), mirroring the
  metric-side stale-sample rejection. Not a sentinel — the message
  carries the diagnostic values directly.

---

## Query API

### `GET /api/v1/events`

Range query with optional name filter. Cursor-based pagination is not
implemented in v1 — the response is bounded by `limit` (default 100,
hard cap 1000). When you hit the cap, narrow the window or filter by
name; bumping the cap is a future change.

```http
GET /api/v1/events
    ?db=sensors
    &name=disc.*           (optional; exact match OR wildcard pattern)
    &start=2026-06-01T00:00:00Z
    &end=2026-06-02T00:00:00Z
    &limit=100              (optional; default 100, hard cap 1000)
    &timestamp_unit=ns      (optional; ns | us | ms | s; default ns)
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
        "id":   1,
        "ts":   1717238412000000000,
        "value_type": "int32",
        "int32": 542,
        "payload": {"path": "/tmp"}
      },
      {
        "name": "disc.write.slow",
        "id":   1,
        "ts":   1717238531000000000,
        "value_type": "int32",
        "int32": 870
      }
    ]
  }
}
```

Response shape rules:

- The typed-value field is named for its type (`int32` or `float32`) so
  consumers don't have to redo the polymorphism client-side. It's
  omitted when `value_type = none`.
- `value_type` is always present and is one of `none`, `int32`, `float32`.
- `payload` field omitted when no payload was stored.
- `payload` returned as parsed JSON if the stored bytes parse cleanly;
  otherwise as `{"raw_base64": "..."}` so the wrapping response stays
  valid JSON regardless of what bytes the producer stored.

### `GET /api/v1/events/aggregate`

Time-bucketed **count** of matching events. v1 supports `count` only —
the endpoint has no `aggregate` parameter; the count semantic is
hardcoded. (Numeric aggregates over event values are listed in the
"Aggregate semantics" table below as future work.)

```http
GET /api/v1/events/aggregate
    ?db=sensors
    &name=disc.*               (optional; exact match OR wildcard pattern)
    &start=2026-06-01T00:00:00Z
    &end=2026-06-02T00:00:00Z   (optional; defaults to now)
    &window=1h                  (required; bucket size)
    &timestamp_unit=ns          (optional; ns | us | ms | s; default ns)
```

Response:

```json
{
  "status": "success",
  "data": {
    "resultType": "events_aggregate",
    "db": "sensors",
    "window": "1h0m0s",
    "result": [
      {"ts": 1717238400000000000, "count": 12},
      {"ts": 1717242000000000000, "count": 7}
    ]
  }
}
```

Bucket TS is the **bucket start** (`floor(event_ts / window) * window`).
Buckets with zero matching events are omitted from the result — clients
that want a dense series should fill gaps client-side. This is a
deliberate v1 simplification; the metric `query_range` aggregate
response shape (`result: [{metric: {...}, values: [[ts,val],...]}]`) is
not used here.

`name` supports both exact match and shell-style wildcards (`*`, `?`,
`[abc]`). Without a `name` parameter, every event in the time window
contributes to the count.

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

Three event integrations are shipped on the dashboard side:

1. **`event_log` widget** — tabular live feed of recent events.
2. **Event-backed line-chart series** — each numeric event becomes one
   scatter point on a `line_chart` widget.
3. **`event_overlays` on metric line-charts** — vertical markers at
   event timestamps drawn on top of metric series.

### `event_log` widget

A tabular live feed of recent events, filtered by name pattern. Each
series under the widget is one log channel:

```json
{
  "type": "event_log",
  "title": "Slow Disk Writes",
  "refresh_sec": 10,
  "lookback": "6h",
  "series": [
    {
      "db": "metrics",
      "event_name_pattern": "disk.sd_write_probe.slow",
      "event_limit": 10
    }
  ]
}
```

Field rules (enforced by `validateDashboardConfig` in
[internal/web/dashboard_config.go](../internal/web/dashboard_config.go)):

- `type` must be `event_log`.
- Widget-level `lookback` is required and must be a valid duration
  (parsed via `engine.ParseDuration`).
- Each series **must** define `event_name_pattern` (an exact name OR a
  wildcard pattern with `*`, `?`, `[abc]`).
- Each series **may not** mix `event_name_pattern` with the metric-side
  fields (`query`, `metric`, `measurement`, `field`).
- `event_limit` caps the row count returned per series (default 100,
  hard cap 1000 — same as `GET /api/v1/events`).
- `db` overrides `default_db` for this series.

### Event-backed line-chart series

A `line_chart` series whose `event_name_pattern` is set is event-backed
rather than metric-backed. Each int32/float32 event in the time window
becomes one scatter point at `(event.ts, event.value)`. `none`-typed
events have no value to plot and are silently skipped.

```json
{
  "type": "line_chart",
  "title": "Slow Disk Write Latencies",
  "lookback": "6h",
  "interval": "30s",
  "series": [
    {
      "label": "ms",
      "db": "metrics",
      "event_name_pattern": "disk.sd_write_probe.slow",
      "event_limit": 1000
    }
  ]
}
```

Field rules for event-backed line-chart series:

- `event_name_pattern` activates event-backing. Cannot mix with
  `query`/`metric`/`measurement`/`field` — the validator rejects mixed
  shapes.
- `aggregate` and `window` are **not** supported on event-backed series.
  Use the `GET /api/v1/events/aggregate` endpoint via a separate
  visualization if you need event counts. (Per-event-value numeric
  aggregates are designed but not built — see the "Aggregate semantics"
  table.)
- `transform` (factor / offset / unit / decimals / format) is honored
  the same way as on metric series: the raw int32/float32 value is run
  through the transform before plotting.
- Each event-backed series renders as scatter points (no connecting
  line) because events are sparse occurrences, not a continuous signal.
- A single `line_chart` widget may freely mix metric-backed and
  event-backed series.

### Event overlays on metric charts

`event_overlays` is an optional widget-level array on `line_chart`
widgets. Each entry renders one layer of vertical dashed markers at
event timestamps over the chart. Multiple overlays compose; the
chart's y-scale is determined entirely by the underlying metric series.

```json
{
  "type": "line_chart",
  "title": "CPU temp with overheat events",
  "lookback": "6h",
  "interval": "1m",
  "series": [{"label": "CPU", "metric": "temp.cpu"}],
  "event_overlays": [
    {
      "event_name_pattern": "temp.office.overheat*",
      "color": "#c00",
      "label": "Overheat",
      "event_limit": 200
    },
    {
      "event_name_pattern": "deploy.completed",
      "color": "blue"
    }
  ]
}
```

Field rules for `event_overlays` (validated at save time):

- Only allowed on `line_chart` widgets — `event_log`, `aggregate_band`,
  `numbers`, `number` reject the field.
- Each overlay **must** define a non-empty `event_name_pattern`.
- `color` is optional. Accepts CSS hex (`#rgb`, `#rrggbb`, `#rrggbbaa`,
  with or without alpha) or a short ASCII named color (`red`,
  `crimson`, etc.). When omitted, a stable palette color is picked
  deterministically from the layer's label/pattern.
- `event_limit` caps the marker count per layer (default 200, hard cap
  1000). The cap mostly matters on busy event streams; a render with
  thousands of markers becomes visually unusable anyway.
- Duplicate effective labels (the resolved `label` or, falling back,
  the `event_name_pattern`) are rejected at save time — they would be
  indistinguishable in the chart legend.
- A widget that produced no metric/event series points renders nothing,
  even if overlays have markers. Overlays decorate a chart; without a
  chart there's nothing to overlay on.

---

## Aggregate semantics

**v1 implements `count` only.** The aggregate endpoint has no
`aggregate` parameter; the count semantic is hardcoded. The table below
documents the *planned* aggregate matrix once value-aware aggregates
ship — concretely, the design intent is for numeric aggregates to apply
to typed events the same way they apply to metrics. None of the
non-`count` rows are queryable today.

| Aggregate              | `none` value | `int32` value | `float32` value | Status   |
|------------------------|--------------|---------------|-----------------|----------|
| `count`                | yes          | yes           | yes             | shipped  |
| `min` / `max`          | rejected     | yes           | yes             | planned  |
| `avg` / `sum`          | rejected     | yes           | yes             | planned  |
| `median` / `p50/95/99` | rejected     | yes           | yes             | planned  |

When the numeric aggregates ship, the contract is:

- For value-typed events, the numeric aggregate set matches the metric
  aggregate set, so the query and dashboard surfaces stay uniform.
- For `none`-typed events, only `count` will be meaningful — any other
  aggregate is expected to return a clean error rather than silently
  zero-filling.

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

**Phase 1 — Storage + ingest + basic query. SHIPPED.** Catalog, events
WAL with replay, `events-*.dat` page format with id-set header,
`POST /api/v1/events`, `GET /api/v1/events`, `GET /api/v1/events/catalog`,
`nanocli inspect events`, `nanocli inspect events-wal`,
`nanocli inspect events-catalog`, `nanocli events` range query, retention
joins events files to the partition family, `drip` opt-in event emission
for the SD-write-probe threshold. Crash-safety exercised by
[scripts/events_chaos.py](../scripts/events_chaos.py).

**Phase 2 — Aggregate counts. SHIPPED (count only).**
`GET /api/v1/events/aggregate` with hardcoded count semantic over a
`window` bucket; `nanocli events --aggregate count --window`. Value-aware
numeric aggregates (avg/min/max/sum/median/percentiles on
`int32`/`float32` events) are designed but not built — see the
"Aggregate semantics" table above.

**Phase 3 — Dashboard richness. SHIPPED.** The `event_log` widget type,
event-backed `line_chart` series (each numeric event becomes one scatter
point), widget-level `event_overlays` on metric line-charts (vertical
markers), editor pickers and toggle UI for both, and the engine view's
event-catalog / events-WAL / events-file inspection panels are all
live. See the Dashboard integration section above for the JSON shapes
and validation rules.

**Phase 4 — Handler registry. DESIGNED, not built.** `RegisterEventHandler`,
async dispatch, the first built-in handler (threshold→event),
drop-counter telemetry.

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
max_segment_size = 16777216    # 16 MiB — smaller than metric WAL
fsync_policy     = "segment"   # "segment" or "always"; no inheritance from [wal]
```

The events WAL has no separate `enabled` field — it is implicitly active
whenever `[events].enabled = true`. The fsync policy defaults to
`"segment"` and must be set explicitly to `"always"` if you want
per-append fsync; there is no automatic inheritance from the metric
`[wal].fsync_policy`.

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
| `[events.wal].fsync_policy`         | `segment` | `segment` or `always`. No inheritance from `[wal].fsync_policy` — set explicitly per layer. |
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

All five properties are asserted by
[scripts/events_chaos.py](../scripts/events_chaos.py): mixed-type events
(int32 / float32 / none), random batches, SIGKILL at jittered intervals,
restart, validate per-event-name monotonic ts, no phantom events, no
type drift, and a post-graceful-checkpoint assertion that the
`<db>.events.wal` is empty and `events.json` is non-empty (crash-safety
rule 1). A run of 50 iterations producing ~200 events lost zero events
and detected zero corruption.

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

## Implementation reference

For maintainers tracing the design to code, the shipped surface lives in:

- [internal/engine/events.go](../internal/engine/events.go) — `Event`, `EventID`, error sentinels, value-type byte codes.
- [internal/engine/events_catalog.go](../internal/engine/events_catalog.go) — `EventCatalog`, `WriteCatalog`, `LoadEventCatalog`, validation.
- [internal/engine/events_wal.go](../internal/engine/events_wal.go) — record format, `AppendEvent` / `AppendEventWithName`, `RecordsWithCatalog`, replay.
- [internal/engine/events_page.go](../internal/engine/events_page.go) — in-memory accumulator + frame encode/decode.
- [internal/engine/events_file.go](../internal/engine/events_file.go) — `events-<partition>.dat` writer/reader + frame walker.
- [internal/engine/events_engine.go](../internal/engine/events_engine.go) — `Engine.AddEvent`, `Engine.QueryEvents`, `Engine.ListEvents`, WAL replay, flush + reset wiring.
- [cmd/nanotdb/events_http.go](../cmd/nanotdb/events_http.go) — HTTP handlers for `/api/v1/events*`.
- [cmd/nanocli/events.go](../cmd/nanocli/events.go) and [cmd/nanocli/inspect_events.go](../cmd/nanocli/inspect_events.go) — CLI commands.

---

## See also

- [CONCEPTS.md](CONCEPTS.md) — how metrics and pages and the WAL fit
  together; events follow the same model.
- [ARCHITECTURE.md](ARCHITECTURE.md) — storage and query walkthrough.
- [RECOVERY.md](RECOVERY.md) — WAL behavior and durability tuning; the
  same fsync policy and durability profile apply to the events WAL.
- [CONFIGURATION.md](CONFIGURATION.md) — `engine.toml` and
  `manifest.toml` reference.
- [GLOSSARY.md](GLOSSARY.md) — canonical terms (including event entries).
- Metric catalog reference points used during this design:
  [catalog.go](../internal/engine/catalog.go),
  [wal.go](../internal/engine/wal.go),
  [engine.go](../internal/engine/engine.go).
