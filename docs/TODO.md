# Things to do
- ~~add median and trimmed average aggregate and use them for sd latency~~ added few aggregates
- think about adding events or annotations, use those to put events for things like sd read > 500ms.
- ~~analyze existing sd latency data to see good numbers for trimming,  5%, 1%?~~
- ~~start with ignoring anything more than 250ms for a start as an outlier ~~


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
