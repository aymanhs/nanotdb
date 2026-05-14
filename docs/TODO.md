# NanoTDB TODO (Design Follow-ups)

These items are intentionally deferred from the current design lock.

## Deferred Safety Details

- Specify crash-tail detection and truncation rules for variable-length `.dat` page frames.
- Define exact framing/checksum strategy for on-disk page frames.

## Deferred API/Behavior Decisions

- Define multi-metric insert semantics under crash (partial success behavior).
- Revisit acknowledgment semantics once stronger durability (WAL or fsync policy changes) is introduced.
- Revisit rollup architecture after v1; current design intentionally excludes rollup storage/query semantics.
- If rollups are added later, define rollup job scheduling, catalog schema, retention, derivation levels, and query behavior from scratch.

## Open Questions

- Q5: fsync guarantees on clean shutdown versus buffered-loss model.

## Planned Documentation

- Add a developer-focused binary format document covering:
  - Variable-length page frame header/layout
  - Compression block payload rules
  - WAL record layout and replay rules
  - Startup replay metrics semantics
  - Versioning and compatibility rules
