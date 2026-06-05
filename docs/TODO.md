# Things to do
- ~~think about adding events or annotations, use those to put events for things like sd read > 500ms.~~ events shipped across Phases 1–3; see [EVENTS.md](EVENTS.md). `drip` emits `disk.sd_write_probe.slow` events when the probe crosses its configured threshold.
- **Events Phase 4 — handler registry.** Designed in [EVENTS.md](EVENTS.md), not built. See "Phase 4 backlog" section below for the concrete scope.
- **Events Phase 2 — numeric aggregates over event values.** `/api/v1/events/aggregate` ships count-only today. The aggregate-semantics table in [EVENTS.md](EVENTS.md) lists the planned avg/min/max/sum/median/p50/p95/p99 work for typed events; not built.
- **Events query — cursor pagination.** `GET /api/v1/events` currently caps at `limit` (default 100, hard cap 1000) with no cursor. EVENTS.md was edited to stop promising it, but adding real pagination is worth doing if anyone hits the cap.
- **Events stats counters.** Emit per-DB events counters into the internal stats database — at minimum `internal/nanotdb/<db>/events/wal_appends`, `events/payload_rejected_oversized`, `events/page_force_flushes` — so an operator can see ingest volume and spike-protection hits from the metrics surface, the same way metric WAL stats are exposed today. The engine inspection routes already cover the "what's on disk" view; this would close the "what's been arriving" gap.
- add baseurl so the app is friendly to reverse-proxy
- consider having this as main webserver, support basic templats for pages
- separate web-ui from db port
- ~~add median and trimmed average aggregate and use them for sd latency~~ added few aggregates
- ~~analyze existing sd latency data to see good numbers for trimming,  5%, 1%?~~
- ~~start with ignoring anything more than 250ms for a start as an outlier ~~

# Phase 4 backlog — Events handler registry

Status: **designed, not built**. Canonical spec in [EVENTS.md](EVENTS.md)
"Future hooks" section. This block is the short version so the TODO is
self-navigable.

## What it is

A per-engine registry that lets caller code react to events as they're
written, without affecting ingest safety:

```go
e.RegisterEventHandler("disc.write.slow", func(ev Event) {
    // re-emit as a metric, forward to a webhook, write a log, whatever.
})
```

## Hard contract (the reason it's a separate phase)

- Handlers fire **after** WAL append + in-memory page enqueue + catalog
  resolution. The event is durable and queryable before any handler
  runs.
- Asynchronous, off a per-DB bounded channel. Handler failure cannot
  block or fail ingest.
- Drop-and-count on channel full; surfaced via a new stats counter
  `internal/nanotdb/<db>/events/handler_drops`.
- Handler set is in-memory only — re-registered on every engine open.
  Handlers are application code, not data; persisting them via
  `events.json` would be the wrong layer.

## What this unlocks (designed, none built)

- **Threshold → event** built-in: watches a metric value via the
  engine's write path and emits an event when a configured threshold
  is crossed. (Opposite direction of `drip`'s collector-side probe.)
- **Event → metric promotion**: handler increments a counter metric per
  event, so downstream metric-aware tooling can see event volumes
  without learning the events API.
- **Event → webhook**: forward to Slack / PagerDuty / etc. Handler
  owns retries.

## What this is explicitly *not*

- No file-based handler config — Go code only.
- No retry / dead-letter queue at the registry level. Drop-and-count
  is the failure mode; handlers that must not drop own their retries.
- No ordering guarantee across handlers for the same event.
- No sync-mode escape hatch. Sync handlers would re-couple ingest
  latency to handler latency.

## Rough implementation footprint (estimate)

- `internal/engine/events_engine.go` — `RegisterEventHandler`,
  per-DB dispatch channel allocated lazily, a goroutine per DB
  draining the channel.
- `Engine.AddEvent` — non-blocking send into the dispatch channel
  with a `default:` drop branch + stat counter bump.
- One built-in handler shipped with the feature (likely
  threshold→event since `drip` already covers the inverse).
- Tests: handler fires once per event, drops counted on full channel,
  slow handler doesn't block ingest, handler panic doesn't propagate.
- Doc updates: flip Phase 4 marker in [EVENTS.md](EVENTS.md) from
  DESIGNED to SHIPPED; mention the registry in CONCEPTS / LAWS.

Estimate: ~200 lines engine + ~200 lines tests + ~100 lines of the
built-in + doc edits. Comparable in size to the event_overlays work.

## Decisions to lock in before building

- Dispatch channel size per DB. Probably `~64`; large enough to
  absorb a normal burst, small enough that an actually-broken handler
  is visible in `handler_drops` within seconds.
- Handler registration ergonomics: name-exact only, name-glob, or
  both? EVENTS.md shows name-exact in the example. Glob is cheap to
  add but doubles the lookup paths.
- Whether to expose a tiny "list registered handlers" inspect/HTTP
  surface for debugging, or keep the registry purely in-process.


# NanoTDB TODO (Design Follow-ups)

These items are intentionally deferred from the current design lock.

## Deferred Safety Details

- Specify crash-tail detection and truncation rules for variable-length `.dat` page frames.
- Define exact framing/checksum strategy for on-disk page frames.
- Add explicit on-disk format versioning for `.dat` frames/files so future layout changes can be detected and migrated safely.
- Define whether `.dat` files should end with a trailer record containing file-level metadata such as partition, min/max time, metric summaries, frame index, and compatibility/version markers.
- Prefer a dual-file strategy over in-place `.dat` evolution: keep `data-<partition>.dat` as the current interleaved ingest file, and introduce a separate optimized day-file name for versioned/trailer-backed read-optimized storage.
- Settled lifecycle rule for dual-file storage: `metric-<partition>.dat` exists only for fully sealed partitions, and a successful rewrite removes `data-<partition>.dat` so query readers never arbitrate between both formats for the same partition.

## Deferred API/Behavior Decisions

- Define multi-metric insert semantics under crash (partial success behavior).
- Revisit acknowledgment semantics once stronger durability (WAL or fsync policy changes) is introduced.
- Decide how rollup checkpoint logs should compact or rewrite once append-only checkpoint files become large.
- Define when sealed ingest files should be converted into the optimized day-file format, including whether close-time rewrite should also backfill older sealed partitions opportunistically.

## Open Questions

- Q5: fsync guarantees on clean shutdown versus buffered-loss model.
- Q6: Should raw `.dat` layout stay multi-metric interleaved, or should a future compaction/rewrite mode support per-metric day pages with trailer-based lookup/index metadata?

## Planned Documentation

- Add a developer-focused binary format document covering:
  - Variable-length page frame header/layout
  - Compression block payload rules
  - WAL record layout and replay rules
  - Startup replay metrics semantics
  - Versioning and compatibility rules
  - File trailer/index format and lookup semantics if trailers are introduced
  - Tradeoffs between interleaved multi-metric pages and per-metric rewritten pages
  - Dual-file ingest/optimized partition lifecycle, naming, and reader selection rules
