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
- Each database has its own daily raw `.dat` files and source metric catalog
- No data or catalog state is shared across databases
- Crash recovery is performed independently per database

Purpose:
- Failure isolation
- Simplified recovery
- Clear retention and lifecycle boundaries

---

## LAW 1 - UTC Day Partitioning

Persisted raw data is partitioned by UTC calendar day.

Formally:
- For a sample with timestamp `T`, target file is `data-UTC(T).dat` where `UTC(T)` is formatted as `YYYY-MM-DD`
- Retention boundaries are day-file boundaries, not per-record tombstones

Purpose:
- Deterministic storage placement
- Simple retention through folder deletion

---

## LAW 2 - Ordered Inserts per Metric

Samples for a given metric must be time-ordered.

Formally:
- For a metric `M`, incoming samples must have timestamp `>=` the last accepted timestamp for `M`
- Samples with timestamp `<` last accepted timestamp are rejected
- Equal timestamps are valid and preserve append order

Purpose:
- Eliminates reordering complexity
- Simplifies write and read logic

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
- Recovery reconstructs state from source metric catalog, daily raw `.dat` files, and WAL
- Startup WAL replay reconstructs unflushed in-memory page state
- Recovery scans `.dat` sequentially and discards/truncates invalid trailing tail

Purpose:
- Deterministic restart behavior
- No hidden persistent state

---

## LAW 7 - No Separate Index File

NanoTDB never uses a separate index file. Queries must operate directly on data files.

Formally:
- Full correctness is derived from `.dat` frames and catalog only
- No separate index file is ever created, read, or maintained
- No in-file footer index or auxiliary index structures are used

Purpose:
- Minimal write-path complexity
- Fewer crash-consistency surfaces
- Permanent architectural simplicity

---

## LAW 8 - Retention Is Filesystem-Scoped Deletion

Retention is enforced by deleting old UTC day files.

Formally:
- Retention operations remove old `data-YYYY-MM-DD.dat` files; they do not rewrite surviving `.dat` content

Purpose:
- Operational simplicity
- Preserves append-only semantics

---

## LAW 9 - MetricID Mapping Is Persistent and Monotonic

Metric identity is API-string based and storage-id based.

Formally:
- Each database maintains a persistent mapping from metric identifier string to source metric id in range `1..1023`
- New metric strings are assigned a new source metric id within `1..1023`
- If no source metric id remains available, metric creation is rejected
- Assigned source metric ids are never deleted and never reused

Purpose:
- Stable on-disk identity
- Deterministic recovery and query behavior

---

## LAW 10 - Acknowledgment Does Not Imply Immediate Stable-Storage Durability

In v0, acknowledged writes may be lost after crash.

Formally:
- An acknowledgment means accepted by the write path
- WAL replay provides crash recovery for unflushed pages, but durability still depends on WAL/file-system flush behavior

Purpose:
- Makes durability contract explicit
- Avoids accidental over-promising before WAL/durability features are implemented

---

## LAW 11 - WAL Replay Is Mandatory When WAL Data Exists

WAL behavior is not part of v0 correctness.

Formally:
- If WAL contains valid records, startup must replay them
- WAL may only be reset after associated in-memory page data is flushed and no open pages remain

Purpose:
- Preserve future extensibility
- Keep v0 semantics unambiguous


---

## META-LAW - Crashes Are Legal at Any Instruction

The system may crash at any instruction boundary.

If a crash can violate any law above, the design is incorrect.

---

These laws define the foundation of NanoTDB.
Every feature must preserve them.
