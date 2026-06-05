# NanoTDB - Laws (Invariants)

This document defines the invariants of NanoTDB.

An invariant is a rule that must always hold:
- before and after any operation
- during normal execution
- and even if the process crashes at any instruction

If a change violates any law below, the design is incorrect.

All laws apply per database unless explicitly stated otherwise.

---

## LAW 0 - Database Isolation

Each NanoTDB database is an independent failure and storage domain.

Formally:
- Each database has its own partitioned raw `.dat` files and source metric catalog
- When events are enabled, each database also has its own `events.json`, `<db>.events.wal`, and partitioned `events-<partition>.dat` files
- No data or catalog state — metric or event — is shared across databases
- Crash recovery is performed independently per database
- The metric and events layers within one database are independent of each other: disabling events leaves the metric storage untouched, and corruption of one events file does not propagate to metric storage (or vice versa)

Purpose:
- Failure isolation
- Simplified recovery
- Clear retention and lifecycle boundaries

---

## LAW 1 - Deterministic UTC Partitioning

Persisted raw data is partitioned deterministically by configured UTC partition granularity.

Formally:
- For a sample or event with timestamp `T`, target file is determined only by `(partition mode, UTC(T))`
- Supported partition modes are `day|month|year|forever`
- Both metric (`data-*.dat`) and event (`events-*.dat`) storage use the same per-database partition mode; they never split
- Retention boundaries are partition-file boundaries, not per-record tombstones; the events file for an expired partition is treated as part of the same partition family as the matching `data-`/`raw-`/`metric-` files

Purpose:
- Deterministic storage placement
- Simple retention through folder deletion

---

## LAW 2 - Ordered Inserts per Stream

Samples for a given metric — and occurrences of a given event name — must be time-ordered within their stream.

Formally:
- For a metric `M`, incoming samples must have timestamp `>=` the last accepted timestamp for `M`
- For an event name `E`, incoming occurrences must have timestamp `>=` the last accepted timestamp for `E`
- Out-of-range timestamps are rejected for both layers
- Equal timestamps are valid and preserve append order

Cross-stream order is not constrained:
- Different metrics in the same database may interleave freely (the page-wide monotonic rule applies per partition, not across metrics)
- Different event names in the same partition page may interleave with reordered timestamps; the per-name rule is enforced at the events catalog (`EventCatalog.LastTS`) rather than at the page level

Purpose:
- Eliminates reordering complexity within a stream
- Simplifies write and read logic
- Permits the events layer to accept arrival-order multi-name batches without expensive page rotations

---

## LAW 3 - Append-Only Immutable Page Frames

Once a page frame is written, it is never modified in place.

Formally:
- Daily `.dat` files are append-only streams
- Existing frame bytes are immutable after durable write

Purpose:
- Simple crash semantics
- SD-card friendly I/O

---

## LAW 4 - Self-Describing Variable-Length Frames

Each frame must be decodable and skippable without external index state.

Formally:
- Every frame includes enough metadata to determine boundaries and time range
- Every frame includes integrity information sufficient for corruption/crash-tail detection
- Query filtering must be possible from header metadata without decompressing payload

Purpose:
- Enables no-index correctness model
- Supports safe sequential scan and recovery

---

## LAW 5 - Independent Compression Blocks

Frame payload compression must be independent per frame.

Formally:
- A frame payload may not depend on bytes from previous or next frames for decompression

Purpose:
- Localized recovery
- Predictable read amplification

---

## LAW 6 - Recovery from Durable Files

After a crash, in-memory state is discarded and rebuilt from durable files.

Formally:
- Metric recovery reconstructs state from the metric catalog, `data-*.dat` files, and the metric WAL
- Events recovery (when the layer is enabled) reconstructs state from `events.json`, `events-*.dat` files, and the events WAL; the two recoveries run side-by-side and do not share state
- Startup WAL replay reconstructs unflushed in-memory page state for both layers independently
- Recovery scans `.dat` and `events-*.dat` files sequentially and discards/truncates invalid trailing tail
- A newEvent record in the events WAL re-creates its in-memory catalog entry on replay — `events.json` need not exist beforehand

Purpose:
- Deterministic restart behavior
- No hidden persistent state

---

## LAW 7 - No Separate Index File for Raw Ingest

NanoTDB never uses a separate external index file. Queries against raw
ingest data operate directly on the `data-<partition>.dat` frames.

Formally:
- Full correctness for raw ingest is derived from `data-*.dat` frames and the catalog only
- No separate external index file is ever created, read, or maintained
- The optional query-optimized `metric-<partition>.dat` files carry their
  own internal indexes (a header, time-frame and metric-frame indexes,
  and an EOF footer). These are part of the file itself and never
  separate sidecar artifacts, so the "no external index" property holds

Purpose:
- Minimal write-path complexity for the always-on ingest layout
- Fewer crash-consistency surfaces
- Permanent architectural simplicity

---

## LAW 8 - Retention Is Filesystem-Scoped Deletion

Retention is enforced by deleting (or archiving) old UTC partition files.

Formally:
- Retention operations remove old partition files; they do not rewrite surviving content
- A partition's family is treated atomically: `data-<partition>.dat`, `raw-<partition>.dat`, `metric-<partition>.dat`, and `events-<partition>.dat` for the same partition are processed together by the configured `retention_action`
- `keep` leaves everything in place; `delete` removes the family; `archive` folds the family into a tar bucket and removes the originals

Purpose:
- Operational simplicity
- Preserves append-only semantics
- Keeps metric and event data for the same time window on the same lifecycle

---

## LAW 9 - Identifier Mappings Are Persistent and Monotonic

Both metric identity and event identity are API-string based and storage-id based.

Formally:
- Each database maintains a persistent mapping from metric identifier string to `MetricID` in `1..65535`
- Each database maintains a persistent mapping from event identifier string to `EventID` in `1..1023`
- New identifier strings (metric or event) are assigned a new id within their respective range
- If no id remains available in either space, registration of a new identifier is rejected
- Assigned ids are never deleted and never reused
- The two id spaces are independent: a metric and an event may carry the same string name without collision

Purpose:
- Stable on-disk identity
- Deterministic recovery and query behavior

---

## LAW 10 - Acknowledgment Durability Is Governed by Configured Policy

The durability of an acknowledged write is determined by the configured
WAL fsync policy and durability profile, not by the acknowledgment itself.

Formally:
- An acknowledgment means accepted by the write path
- WAL replay provides crash recovery for unflushed pages, but per-append
  stable-storage durability requires `wal.fsync_policy = always`
- Page/catalog stable-storage durability depends on the active
  `durability.profile` (`strict|balanced|throughput`)

Purpose:
- Makes the durability contract explicit
- Documents the operator's tradeoff between throughput, SD wear, and
  per-append durability rather than hiding it

---

## LAW 11 - WAL Replay Is Mandatory When WAL Data Exists

WAL replay is part of the engine's correctness contract. Applies to both
the metric WAL (`<db>.wal`) and the events WAL (`<db>.events.wal`)
independently.

Formally:
- If the WAL contains valid records, startup must replay them
- WAL may only be reset after the associated catalog (metric or event) is durable AND the associated in-memory page is flushed AND no open pages still depend on the WAL
- Replay must reject records whose value type does not match the catalog, rather than silently coercing to a default type
- For the events WAL, a non-newEvent record referencing an unknown `EventID` is a hard error — no silent drop, no default type
- For the events WAL, a newEvent record whose inline `(name, value_type)` disagrees with an existing catalog entry is a hard error

Purpose:
- Deterministic and lossless recovery of WAL-protected samples and events
- Prevents type-coercion hazards (e.g. reading float bits as int)
- Asserts the catalog-before-WAL-reset invariant that
  [scripts/events_chaos.py](../scripts/events_chaos.py) verifies at every
  graceful checkpoint


---

## META-LAW - Crashes Are Legal at Any Instruction

The system may crash at any instruction boundary.

If a crash can violate any law above, the design is incorrect.

---

These laws define the foundation of NanoTDB.
Every feature must preserve them.
