# Internal Events

NanoTDB and `drip` emit **internal events** about their own lifecycle —
engine startup, partition seal/delete, retention sweeps, drip target
disconnects, threshold crossings, and so on. These ride the existing
events layer documented in [EVENTS.md](EVENTS.md): same on-disk shape,
same WAL, same catalog rules, same query surface. The difference is
*who* emits them — the engine itself, not user code — and *where* they
land: the `internal` database that already collects engine stats
metrics.

This document is the canonical reference for the internal-events
surface: the event catalog, the group-toggle config schema, the
self-recursion guard, the async emit pipeline, and the discovery
endpoint. It is a sibling to [EVENTS.md](EVENTS.md), not a replacement
— everything about how events are stored and queried lives there.

Status: **DESIGNED, not built.** This is the spec we'll implement against.

---

## Why internal events

Three properties the existing surfaces don't cover:

1. **Discrete things deserve discrete records.** "Partition `2026-06`
   was sealed" is a point-in-time fact with context (bytes written,
   record count, file family). Modelling it as a metric is awkward; it
   isn't a continuous signal.
2. **Forensics.** When you read back through an incident, "the dirty
   shutdown happened at 14:02:17, WAL replay recovered 8412 records,
   retention deleted partition `2026-05` at 14:03:01" is exactly the
   trail you want — and exactly what the events layer is shaped for.
3. **Reuse of the storage layer we already shipped.** Catalog,
   partition cadence, retention, WAL, query APIs, dashboard widgets,
   nanocli inspection — all already exist. Internal events are an
   *application* of the events layer, not a new layer.

The internal-events surface complements the engine **stats metrics**
that already write to `internal/nanotdb/*` (see
[engine.go:269-270](../internal/engine/engine.go#L269-L270) for the
constants and `maybeFlushStats` for the writer). Stats are continuous
counters sampled on `StatsInterval`. Events are discrete things that
happened. Both live in the same `internal` database.

---

## Mental model

```text
internal/
  catalog.json          — engine stats metrics: nanotdb/* (existing)
  events.json           — internal events catalog          (NEW use of existing file)
  manifest.toml         — [events].enabled = true required (NEW)
  internal.wal          — stats metrics WAL (existing)
  internal.events.wal   — internal events WAL              (NEW use of existing file)
  data-*.dat            — stats metric pages (existing)
  events-*.dat          — internal event pages             (NEW use of existing file)
```

No new files, no new on-disk formats, no new partition machinery. The
`internal` database participates in the same retention rules as any
other db; old internal events age out exactly like old stats samples.

The shipping `internal` db is created lazily on first stats flush
([engine.go:2384](../internal/engine/engine.go#L2384)). Enabling
internal events makes the engine register the db up-front at startup,
with `[events].enabled = true` in its manifest.

---

## Naming convention

All internal events use a dotted name `<group>.<event>`:

- `nanotdb.partition.sealed`
- `nanotdb.wal.replayed`
- `drip.target.disconnected`
- `drip.collector.failed`

Two things to note:

- **Prefix is not reserved.** Users can register events called
  `nanotdb.whatever` if they want to. We trust callers to be mature
  enough not to squat the prefix; if it becomes a real problem we'll
  add a reservation later.
- **Group is the unit of toggling.** The portion of the name before
  the last useful boundary — see the "Group taxonomy" tables below —
  is what `[internal_events.groups]` references.

Severity is conveyed by the event name, not a payload field. We follow
the same convention the metric layer uses: `disk.write.slow` and
`disk.write.error` carry their severity in the name. No
`severity: "warn"` field, no `level: "info"` payload key.

---

## Value vs payload

Most internal events have a headline number worth charting — bytes
flushed, ms elapsed, records replayed. That goes in the typed
**value**. Rich context — db name, file family, partition key, error
text — goes in the JSON **payload**.

Concretely:

- `nanotdb.partition.sealed` — value: `int32` record_count;
  payload: `{db, file, partition_key, bytes, window_seconds}`
- `nanotdb.wal.fsync.slow` — value: `float32` ms;
  payload: `{db, file}`
- `nanotdb.engine.shutdown.clean` — value: `int32` ms_to_drain;
  payload: `{db_count, version}`
- `drip.target.reconnected` — value: `int32` outage_ms;
  payload: `{url}`

`none`-typed events are reserved for "nothing meaningful to chart" —
e.g. `nanotdb.engine.started` carries only context (no numeric
headline), or `drip.lifecycle.config.reloaded` likewise.

---

## Group taxonomy

Groups are the on/off toggle. Each event belongs to exactly one group.
Defaults below are what we ship in `engine.toml` /
`drip.toml`; every group can be overridden per install.

### nanotdb groups

| Group | Default | Events | Typical volume |
|---|---|---|---|
| `nanotdb.lifecycle` | on | `engine.started`, `engine.shutdown.clean`, `engine.shutdown.dirty`, `engine.flush.completed` | handful per day |
| `nanotdb.db` | on | `db.created`, `db.deleted`, `db.opened`, `db.events.enabled`, `db.events.disabled` | rare |
| `nanotdb.partition` | on | `partition.sealed`, `partition.deleted`, `partition.archived`, `partition.optimized` | one per partition window per db |
| `nanotdb.partition.slow` | on | `partition.flush.slow` (only above threshold) | sporadic |
| `nanotdb.wal` | **off** | `wal.replayed`, `wal.segment.rotated`, `wal.reset`, `wal.tail_truncated` | chatty under load |
| `nanotdb.wal.fsync` | **off** | `wal.fsync.slow`, `wal.fsync.error` | flood-prone on bad disks |
| `nanotdb.catalog` | on | `catalog.metric.added`, `catalog.event.added`, `catalog.full`, `catalog.write.failed` | rare |
| `nanotdb.ingest.reject` | **off** | `ingest.rejected.stale` (batched), `ingest.rejected.payload_too_large` | flood-prone |
| `nanotdb.ingest.spike` | on | `ingest.spike.force_flush` | rare and useful |
| `nanotdb.disk` | on | `disk.low`, `disk.write.error` | rare |
| `nanotdb.rollup` | on | `rollup.window.emitted`, `rollup.catchup.started`, `rollup.catchup.completed` | per-window cadence |
| `nanotdb.http` | on | `http.listener.started`, `http.listener.stopped` | rare |
| `nanotdb.mqtt` | on | `mqtt.connected`, `mqtt.disconnected`, `mqtt.subscription.dropped` | sporadic |
| `nanotdb.auth` | on | `auth.failure` (batched per minute) | security-relevant |
| `nanotdb.retention` | on | `retention.sweep.started`, `retention.sweep.completed` | per-sweep |

### drip groups

| Group | Default | Events |
|---|---|---|
| `drip.lifecycle` | on | `drip.started`, `drip.stopped.clean`, `drip.config.reloaded` |
| `drip.target` | on | `drip.target.disconnected`, `drip.target.reconnected` |
| `drip.buffer` | on | `drip.buffer.flush.failed`, `drip.buffer.high_water`, `drip.buffer.dropped` |
| `drip.collector` | on | `drip.collector.started`, `drip.collector.failed` (rate-limited), `drip.collector.recovered` |
| `drip.host` | **off** | `drip.host.boot`, `drip.host.disk.low`, `drip.host.temp.crossed` |
| `drip.threshold` | on | per-collector threshold crossings (e.g. `drip.threshold.disk.sd_write_probe.slow`) |

The on-by-default groups are roughly one-record-per-real-event, the
kind of thing you'd regret not having in front of you during an
incident. The off-by-default groups are either flood-prone or
duplicate of stats metrics we already write — opt in when you need
them.

---

## Event catalog (per-event reference)

Full catalog of the events the engine emits, grouped by feature area.
Each row lists the value type and the payload keys. The catalog is
returned at runtime by `GET /api/v1/internal-events/catalog` (see
below), populated from a registry in code so the doc and the wire stay
in sync.

### `nanotdb.lifecycle`

| Event | Value | Payload |
|---|---|---|
| `nanotdb.engine.started` | none | `{version, git_sha, root_dir, db_count, ms_to_ready}` |
| `nanotdb.engine.shutdown.clean` | int32 ms | `{db_count}` |
| `nanotdb.engine.shutdown.dirty` | none | `{detected_at_startup, prev_wal_bytes}` — emitted by the *next* startup when previous shutdown left WAL non-empty |
| `nanotdb.engine.flush.completed` | int32 bytes_written | `{dbs_flushed, ms}` |

### `nanotdb.db`

| Event | Value | Payload |
|---|---|---|
| `nanotdb.db.created` | none | `{db, partition_mode, retention_action}` |
| `nanotdb.db.deleted` | none | `{db}` |
| `nanotdb.db.opened` | int32 ms | `{db, metric_count, event_count, partition_count}` |
| `nanotdb.db.events.enabled` | none | `{db}` |
| `nanotdb.db.events.disabled` | none | `{db}` |

### `nanotdb.partition` / `nanotdb.partition.slow`

| Event | Value | Payload |
|---|---|---|
| `nanotdb.partition.sealed` | int32 record_count | `{db, file ("data"\|"events"\|"metric"), partition_key, bytes}` |
| `nanotdb.partition.deleted` | int32 bytes_freed | `{db, partition_key, files_removed, retention_reason}` |
| `nanotdb.partition.archived` | int32 bytes | `{db, partition_key, tar_path}` |
| `nanotdb.partition.optimized` | int32 bytes_saved (signed) | `{db, partition_key, file, source_bytes, dest_bytes}` — value is `source_bytes - dest_bytes`; negative means the optimized file is larger |
| `nanotdb.partition.flush.slow` | float32 ms | `{db, file, partition_key}` |

### `nanotdb.wal` / `nanotdb.wal.fsync`

| Event | Value | Payload |
|---|---|---|
| `nanotdb.wal.replayed` | int32 records_replayed | `{db, file ("wal"\|"events.wal"), bytes_scanned}` |
| `nanotdb.wal.tail_truncated` | int32 bytes | `{db, file, reason}` |
| `nanotdb.wal.segment.rotated` | int32 segment_size_bytes | `{db, file}` |
| `nanotdb.wal.reset` | int32 bytes_reclaimed | `{db, file}` |
| `nanotdb.wal.fsync.slow` | float32 ms | `{db, file}` |
| `nanotdb.wal.fsync.error` | none | `{db, file, err}` |

### `nanotdb.catalog`

| Event | Value | Payload |
|---|---|---|
| `nanotdb.catalog.metric.added` | int32 metric_id | `{db, name, value_type}` |
| `nanotdb.catalog.event.added` | int32 event_id | `{db, name, value_type}` |
| `nanotdb.catalog.full` | int32 cap | `{db, kind ("metrics"\|"events")}` |
| `nanotdb.catalog.write.failed` | none | `{db, file, err}` |

### `nanotdb.ingest.reject` / `nanotdb.ingest.spike`

| Event | Value | Payload |
|---|---|---|
| `nanotdb.ingest.rejected.stale` | int32 count_this_window | `{db, top_offenders: [{name, count}, ...]}` — batched, one per minute |
| `nanotdb.ingest.rejected.payload_too_large` | int32 bytes | `{db, name}` |
| `nanotdb.ingest.spike.force_flush` | int32 page_bytes | `{db, layer ("metric"\|"event")}` |

### `nanotdb.disk`

| Event | Value | Payload |
|---|---|---|
| `nanotdb.disk.low` | int32 bytes_free | `{mount, threshold_bytes}` |
| `nanotdb.disk.write.error` | none | `{file, err}` |

### `nanotdb.rollup`

| Event | Value | Payload |
|---|---|---|
| `nanotdb.rollup.window.emitted` | int32 records_written | `{src_metric, dst_metric, window}` |
| `nanotdb.rollup.catchup.started` | int32 windows_pending | `{src_metric, dst_metric}` |
| `nanotdb.rollup.catchup.completed` | int32 windows_caught_up | `{src_metric, dst_metric, ms}` |

### `nanotdb.http` / `nanotdb.mqtt` / `nanotdb.auth`

| Event | Value | Payload |
|---|---|---|
| `nanotdb.http.listener.started` | none | `{addr}` |
| `nanotdb.http.listener.stopped` | none | `{addr, reason}` |
| `nanotdb.mqtt.connected` | none | `{broker}` |
| `nanotdb.mqtt.disconnected` | none | `{broker, reason}` |
| `nanotdb.mqtt.subscription.dropped` | int32 count | `{broker, topic}` |
| `nanotdb.auth.failure` | int32 count_this_window | `{addr, route, top_users: [...]}` — batched, one per minute |

### `nanotdb.retention`

| Event | Value | Payload |
|---|---|---|
| `nanotdb.retention.sweep.started` | int32 candidate_partitions | `{}` |
| `nanotdb.retention.sweep.completed` | int32 partitions_actioned | `{deleted, archived, kept, ms}` |

### `drip.lifecycle` / `drip.target` / `drip.buffer` / `drip.collector` / `drip.host` / `drip.threshold`

| Event | Value | Payload |
|---|---|---|
| `drip.started` | int32 collector_count | `{version, target_db, target_url}` |
| `drip.stopped.clean` | int32 ms_to_drain | `{}` |
| `drip.config.reloaded` | none | `{}` |
| `drip.target.disconnected` | none | `{url, err}` |
| `drip.target.reconnected` | int32 outage_ms | `{url}` |
| `drip.buffer.flush.failed` | int32 queued_samples | `{err}` |
| `drip.buffer.high_water` | int32 queued_samples | `{capacity}` |
| `drip.buffer.dropped` | int32 count_this_window | `{}` — batched per minute |
| `drip.collector.started` | none | `{name, interval_seconds}` |
| `drip.collector.failed` | int32 consecutive_failures | `{name, err}` — rate-limited, emits only on N-in-a-row (default 3) |
| `drip.collector.recovered` | int32 outage_ms | `{name}` |
| `drip.host.boot` | none | `{kernel, uptime_seconds}` |
| `drip.host.disk.low` | int32 bytes_free | `{mount}` |
| `drip.host.temp.crossed` | float32 celsius | `{sensor, threshold_celsius}` |
| `drip.threshold.<metric>.slow` | int32 or float32 (the observed value) | `{collector, metric, threshold, observed}` — one event-name per threshold instance; the existing `disk.sd_write_probe.slow` becomes `drip.threshold.disk.sd_write_probe.slow` under this group |

---

## Configuration

### `engine.toml`

```toml
[internal_events]
enabled = true                       # master switch
db = "internal"                      # destination db; explicit knob for self-hosters
queue_depth = 4096                   # bounded async channel
drop_metric = "nanotdb/internal_events_dropped"
                                     # counter for queue-full drops, into existing stats

[internal_events.groups]
"nanotdb.lifecycle"     = "on"
"nanotdb.db"            = "on"
"nanotdb.partition"     = "on"
"nanotdb.partition.slow"= "on"
"nanotdb.wal"           = "off"
"nanotdb.wal.fsync"     = "off"
"nanotdb.catalog"       = "on"
"nanotdb.ingest.reject" = "off"
"nanotdb.ingest.spike"  = "on"
"nanotdb.disk"          = "on"
"nanotdb.rollup"        = "on"
"nanotdb.http"          = "on"
"nanotdb.mqtt"          = "on"
"nanotdb.auth"          = "on"
"nanotdb.retention"     = "on"
```

### `drip.toml`

```toml
[internal_events]
enabled    = true
target_db  = "internal"              # which db to emit into via the ingest HTTP endpoint
queue_depth = 1024

[internal_events.groups]
"drip.lifecycle"  = "on"
"drip.target"     = "on"
"drip.buffer"     = "on"
"drip.collector"  = "on"
"drip.host"       = "off"
"drip.threshold"  = "on"
```

### Rules

- **Master switch off ⇒ silent.** No catalog growth, no WAL writes,
  no goroutine, no allocations on emit sites.
- **Group key not listed ⇒ per-group default applies** (the "Default"
  column in the tables above).
- **Unknown group key is a hard load error.** A typo like
  `nantodb.parttition` doesn't silently disable nothing.
- **Master switch on but target db doesn't have `[events].enabled = true`
  ⇒ the engine flips it on at startup** for the destination db.
  Internal events are a use of the events layer; we don't make the
  operator enable it twice.
- Both knobs (`engine.toml` and `drip.toml`) are independent. The
  nanotdb engine can be configured to emit while drip stays silent, or
  vice versa.

---

## Self-recursion guard

The internal-events emitter writes to the events layer of the
`internal` db. Without a guard, several paths would re-enter
themselves and either deadlock, produce infinite event streams, or
both.

The rule, mirrored from the existing stats writer
([engine.go:2308](../internal/engine/engine.go#L2308) and
[engine.go:2366](../internal/engine/engine.go#L2366)):

> **No `nanotdb.*` event is ever emitted for the `internal` database.**

One guard, no per-event tagging. Concretely:

- Page flushes of `internal`'s own events file don't fire
  `partition.flush.completed`.
- WAL writes on `internal` don't fire any `wal.*` event.
- Catalog growth on `internal` does **not** fire
  `catalog.event.added` — otherwise the first internal event would
  recursively register itself.
- Retention sweep over `internal`'s own partitions does fire
  `retention.sweep.started/completed` (that's an engine-wide sweep,
  not a per-db event), but does not fire per-`internal`-partition
  `partition.deleted`.

Drip events are unaffected by the guard — they're emitted via the
HTTP ingest API, not from inside the engine, and they don't touch the
engine's write path.

---

## Async emit pipeline

The emitter must never block or fail an engine operation. Same
durability story as stats: at-most-once, drop-and-count on overflow.

1. **Call site** invokes `Engine.emitInternalEvent(group, name, value, payload)`.
2. If `[internal_events].enabled = false` or the group is disabled, the
   call returns immediately. The fast path is a single map lookup
   against a pre-computed `map[group]bool` built at config load — no
   string parsing, no payload allocation. **Encouraged pattern:** wrap
   payload construction in `if e.internalEventsActive(group) { ... }`
   so the payload isn't allocated when the group is off.
3. Otherwise, build the record and non-blocking send on a bounded
   channel of depth `queue_depth`. On channel full, atomically bump
   the `nanotdb/internal_events_dropped` stats counter and drop the
   record. **No fallback to a synchronous write** — that would invert
   the "never block ingest" invariant under exactly the load you most
   want events for.
4. A background goroutine drains the channel and calls
   `e.AddEvent("internal", name, ts, value, payloadJSON)`
   synchronously. Errors from `AddEvent` (e.g. `ErrTooManyEvents` if
   the `internal` db hit the 1023-name cap, or a value-type mismatch
   from a registry bug) are logged once per event name and the record
   is dropped. The drain goroutine never exits because of a per-record
   error.
5. The drain goroutine joins on `Engine.Close()`. Pending records are
   flushed before close returns. If close is forced (signal,
   timeout), unflushed records are lost — and the *next* boot's
   `nanotdb.engine.shutdown.dirty` event records that fact.

### Drip's pipeline

Drip emits via the same HTTP `POST /api/v1/events` endpoint it already
uses for `disk.sd_write_probe.slow`. Same bounded-channel drain shape,
same drop-counter pattern. The drop counter on the drip side surfaces
as a drip metric (`drip/internal_events_dropped`) into whatever
metrics target drip is already pointed at.

### Batching

A small number of events are flood-prone: `ingest.rejected.stale`,
`auth.failure`, `drip.buffer.dropped`. These are batched **inside the
emitter**, not at the call site. The emitter exposes:

```go
e.emitInternalEventBatched(group, name string, key string, payload map[string]any, every time.Duration)
```

Semantics:

- Bumps an in-memory counter keyed by `(name, key)`.
- A single background timer fires every `every` (default 1 minute) and
  emits one event per non-zero counter with `value = count_this_window`
  and payload merged from the most-recent call site.
- Counters reset to zero after emit.

This keeps the call sites simple and the volume bounded.

---

## Discovery: `GET /api/v1/internal-events/catalog`

Returns the registry of known internal events — names, groups, value
types, descriptions, current enable/disable state. Populated from a
table in code, **not** read from `events.json`, so the catalog is
authoritative even before any event has actually been emitted (and
therefore registered in the per-db events catalog).

```http
GET /api/v1/internal-events/catalog
```

Response:

```json
{
  "status": "success",
  "data": {
    "resultType": "internal_events_catalog",
    "master_enabled": true,
    "destination_db": "internal",
    "groups": [
      {
        "name": "nanotdb.partition",
        "enabled": true,
        "default": true,
        "events": [
          {
            "name": "nanotdb.partition.sealed",
            "value_type": "int32",
            "value_units": "record_count",
            "payload_keys": ["db", "file", "partition_key", "bytes"],
            "description": "A partition window closed and the file was sealed."
          },
          { "name": "nanotdb.partition.deleted", "...": "..." }
        ]
      },
      { "name": "nanotdb.wal", "enabled": false, "...": "..." }
    ]
  }
}
```

Used by:

- The dashboard editor — populates pickers for `event_log` widgets
  and `event_overlays` with known internal-event names, with
  descriptions in the hover tooltip.
- `nanocli internal-events` — see below.
- Operators sanity-checking config (which groups are on right now?).

---

## Runtime group toggle

Groups can be flipped on or off at runtime without restarting. Useful
during incident triage: turn on `nanotdb.wal` for ten minutes while
chasing a fsync latency spike, then turn it back off.

### `GET /api/v1/internal-events/groups`

Same shape as the `groups` section of the catalog response, in a
flatter form:

```json
{
  "status": "success",
  "data": {
    "resultType": "internal_events_groups",
    "master_enabled": true,
    "groups": [
      {"name": "nanotdb.lifecycle", "enabled": true,  "default": true,  "source": "engine.toml"},
      {"name": "nanotdb.wal",       "enabled": false, "default": false, "source": "default"},
      {"name": "nanotdb.partition", "enabled": true,  "default": true,  "source": "engine.toml"}
    ]
  }
}
```

`source` is one of `default | engine.toml | drip.toml | runtime` and
records why the group is in its current state.

### `POST /api/v1/internal-events/groups`

```http
POST /api/v1/internal-events/groups
Content-Type: application/json

{"nanotdb.wal": "on", "nanotdb.wal.fsync": "on"}
```

Each key must be a known group name; unknown keys are a 400.
Values must be `"on"` or `"off"`. The change applies immediately to
the in-memory toggle map and `source` flips to `"runtime"`.

**Runtime toggles do not persist.** A restart reverts the group to
its `engine.toml` / `drip.toml` value (or to its built-in default if
not set there). This is deliberate — runtime is for debug sessions;
persistent changes go in the config file. Each toggle change emits a
`nanotdb.lifecycle` event of its own:

| Event | Value | Payload |
|---|---|---|
| `nanotdb.internal_events.group.toggled` | none | `{group, from, to, source ("runtime"\|"config_reload")}` |

So an audit trail of "who turned `nanotdb.wal` on during that
incident" lives in the internal events stream itself.

### Drip side

Drip has the same pair of endpoints under the same paths on its own
HTTP surface (drip exposes a small admin HTTP listener; the runtime
toggle is the third route on it after `/healthz` and `/version`).
Drip-side toggles flip drip-side groups only.

---

## nanocli surface

```text
nanocli internal-events catalog --root <dir>              [--json]
nanocli internal-events groups  --root <dir>              [--json]
nanocli internal-events tail    --root <dir> [--group <g>] [--since <dur>]
nanocli internal-events set     --root <dir> <group> on|off
```

- `catalog` — mirror of the catalog endpoint above; readable table
  by default.
- `groups` — current enable/disable state per group, with the source
  of each value (`default | engine.toml | drip.toml | runtime`).
- `tail` — convenience wrapper over `nanocli events --db internal
  --name nanotdb.*` with optional group filter.
- `set` — POSTs to the runtime toggle endpoint. Same
  "doesn't persist across restart" semantics as the HTTP route.

---

## Interaction with stats metrics

The existing stats metrics at `internal/nanotdb/*` are unchanged.
Internal events live alongside them in the same db. The two surfaces
answer different questions:

| Question | Use |
|---|---|
| "How much WAL did we write per minute over the last hour?" | stats metric `nanotdb/wal_bytes` |
| "When was the last clean shutdown?" | event `nanotdb.engine.shutdown.clean` |
| "What's the current open-page byte count?" | stats metric `nanotdb/page_bytes` |
| "When did the engine force-flush a page under spike pressure?" | event `nanotdb.ingest.spike.force_flush` |

The convention to follow when adding new emit sites: if it's a
continuous level, it's a stats metric. If it's a discrete fact at a
point in time, it's an event. A few things will deserve both —
e.g. `wal_bytes` (continuous) and `wal.segment.rotated` (discrete).

---

## Dashboard integration

Internal events flow through the existing dashboard widgets
documented in [EVENTS.md](EVENTS.md#dashboard-integration) — same
`event_log`, event-backed `line_chart`, and `event_overlays` shapes.
The dashboard ships with a built-in "Engine Health" dashboard JSON
that pre-wires the most useful internal events against the existing
stats charts:

- `event_log` over `nanotdb.lifecycle*`, `nanotdb.disk*`,
  `nanotdb.catalog.full`, `nanotdb.partition.deleted` — the incident
  trail.
- `event_overlays` of `nanotdb.engine.shutdown.dirty` on the WAL-bytes
  stats chart.
- `event_overlays` of `nanotdb.partition.sealed` on the page-bytes
  stats chart.

The editor's pickers auto-suggest event names from
`GET /api/v1/internal-events/catalog` with the description as a
tooltip, so wiring up new dashboards doesn't require re-reading this
file.

---

## Error semantics

| Condition | Behavior |
|---|---|
| Master switch off | Emit sites no-op; no goroutine started. |
| Group disabled | Emit site no-op (single map lookup). |
| Queue full | Drop record, bump `nanotdb/internal_events_dropped`. |
| `AddEvent` returns error | Log once per event name, drop record. Drain goroutine continues. |
| Destination db doesn't exist at startup | Engine creates it with `[events].enabled = true`. |
| `internal` db hits the 1023 events cap | `nanotdb.catalog.full` cannot itself be emitted into the same db; logged instead. Operator action: prune the events catalog. (Unlikely — the spec above defines ~40 names.) |
| Config loaded with unknown group key | Hard error, engine refuses to start. |
| Self-emit from inside the `internal` write path | Suppressed by the recursion guard above. |

---

## Phased delivery

**Phase 1 — Core surface.** Master switch, group registry,
self-recursion guard, async emit pipeline with drop-counter, the
`nanotdb.lifecycle` / `nanotdb.partition` / `nanotdb.db` /
`nanotdb.retention` groups wired into existing engine call sites,
`GET /api/v1/internal-events/catalog`, `nanocli internal-events
catalog`/`groups`.

**Phase 2 — Remaining nanotdb groups.** `wal*`, `catalog`,
`ingest.*`, `disk`, `rollup`, `http`, `mqtt`, `auth`. Includes the
batched-emit helper for the per-minute aggregating events.

**Phase 3 — Drip groups.** All `drip.*` groups, drip-side config
section, drip-side drop counter. The existing
`disk.sd_write_probe.slow` event is renamed under the
`drip.threshold` group umbrella (back-compat note in
[CHANGELOG.md](../CHANGELOG.md)).

**Phase 4 — Dashboard integration polish.** Built-in Engine Health
dashboard JSON shipped with the binary. Editor pickers auto-suggest
from the catalog endpoint.

---

## A note on the `internal` db

The `internal` db is a *normal* nanotdb database. Same catalog rules,
same partition cadence, same retention, same WAL discipline, same
on-disk layout. There is nothing special about it from the storage
layer's point of view — it just happens to be the db the engine
writes its own stats metrics and internal events into.

Concretely:

- Its retention follows the engine default (the `[retention]`
  section of `engine.toml`). If you keep 90 days of metric data, you
  keep 90 days of `engine.started` events and `nanotdb/wal_bytes`
  samples too. Override per-db in `internal/manifest.toml` if you
  want a shorter window for engine telemetry.
- Its partition mode follows the engine default. Same caveat.
- It participates in retention sweeps. Old `internal` partitions are
  deleted/archived alongside everything else.
- It shows up in `GET /api/v1/databases`, in the engine UI's db
  picker, in `nanocli inspect`, and in dashboards. It is not hidden.

The only thing that makes the `internal` db special is the
[self-recursion guard](#self-recursion-guard) — engine code suppresses
internal-event emission *for writes to* the `internal` db so the
write path doesn't recursively emit events about itself. That's a
property of the engine emitter, not a property of the db.

Renaming the destination is a single `[internal_events].db` knob in
`engine.toml`. The guard follows the configured name — if you point
internal events at a db called `telemetry`, the guard suppresses
self-emission for `telemetry`.
