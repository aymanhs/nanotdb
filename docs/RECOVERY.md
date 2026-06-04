# Recovery & durability

NanoTDB's strongest operational property for edge systems is that *recent
data is recoverable after restart* without an external service. The WAL plus
the durable `.dat` files give you a clear story for both clean shutdown and
crash recovery.

This page explains the model and the knobs. For a friendlier introduction
to how the WAL, partition files, and pages fit together, see
[CONCEPTS.md](CONCEPTS.md).

---

## The model in one diagram

```text
ingest ──► WAL append ──► in-memory page ──► (page full or aged)
                                         └──► compressed frame in data-<partition>.dat
                                              WAL reset (replay no longer needed)
```

- The **WAL** protects samples that exist only in memory.
- Once a page is **flushed** to a `.dat` file and no open page still depends
  on that WAL content, the WAL can be reset.
- Sealed `.dat` frames are immutable.

That gives three behaviors:

1. **Clean shutdown.** NanoTDB flushes open pages before exit so recent samples
   move into durable data files. WAL is reset.
2. **Restart after a crash or power loss.** WAL is replayed into the in-memory
   open page, reconstructing the state that was in memory at the moment of the
   crash.
3. **Steady state.** WAL never grows unboundedly — it's a single reusable file
   per database, reset whenever the data behind it is durable.

For the on-disk shape of the WAL and `.dat` files, see
[ARCHITECTURE.md](ARCHITECTURE.md).

---

## WAL fsync policy

`wal.fsync_policy` in `engine.toml` controls how aggressively the WAL is
synced to disk.

| Value     | Behavior                                       | When to use                                                                 |
|-----------|------------------------------------------------|-----------------------------------------------------------------------------|
| `segment` | fsync on WAL reset (after a page flush)        | Reasonable default for many local telemetry setups. Lower SD-card wear.     |
| `always`  | fsync on every append                          | Stronger per-append durability. The conservative choice when power loss is plausible. |

`wal.max_segment_size` controls how large the WAL is allowed to grow before
reset after a page flush. The default is 64 MiB, which is more than enough
for typical edge workloads.

---

## Durability profiles

`durability.profile` controls page-file and catalog fsync behavior.

| Profile      | Page file fsync | Catalog fsync | Notes                                |
|--------------|-----------------|---------------|--------------------------------------|
| `strict`     | yes             | yes           | Conservative end. Default.           |
| `balanced`   | yes             | no            | Reasonable middle ground.            |
| `throughput` | no              | no            | Lowest write overhead, most crash risk. |

---

## Page flush behavior

Per-database `[page]` settings in `<db>/manifest.toml` decide how long data
stays WAL-backed:

- `page.max_records` — sample count limit per in-memory page.
- `page.max_bytes`   — byte limit per in-memory page.
- `page.max_age`     — wall-clock age limit; long-idle pages still flush.

Smaller limits move data into durable `.dat` files faster, at the cost of
more frequent flush overhead. Larger limits buffer more samples in memory
(and in the WAL) before flushing.

For sparse, low-rate telemetry (a Pi pushing one sample every 10 seconds for
a handful of metrics), a larger `page.max_age` keeps the number of tiny
flushed pages down. The default rollup destination manifests already do
this.

---

## Choosing a posture

Conservative end (strongest local recovery):

```toml
[wal]
fsync_policy = "always"

[durability]
profile = "strict"
```

Throughput end (lower write overhead, more crash risk):

```toml
[wal]
fsync_policy = "segment"

[durability]
profile = "throughput"
```

A reasonable middle for SD-backed Pi telemetry where you want to be SD-friendly
but still recover cleanly:

```toml
[wal]
fsync_policy = "segment"

[durability]
profile = "balanced"
```

---

## Backfill considerations

If you're bulk-importing old data, set the per-database `wal.skip_before`
window in the manifest to skip WAL for samples older than the threshold.
This avoids huge WAL churn during the import — the historical data goes
straight into `.dat` pages, and only the live tail is WAL-protected.

---

## Why this matters on SD cards

SD cards do not love rewrite-heavy access patterns. NanoTDB's `.dat` files
are append-only, partitioned (so retention is partition-file deletion, not
in-place rewrites), and compressed. The WAL is a single reusable file per
database that's reset whenever the data behind it is durable, so it doesn't
grow unboundedly. Together, this keeps the SD-write profile much closer to
"large append-only writes" than "lots of small rewrites".

The `drip` collector even ships an `sd_write_probe` measurement
(`disk.sd_write_probe_ms`) so you can *watch* the latency of your actual SD
card over time. See [DRIP.md](DRIP.md).
