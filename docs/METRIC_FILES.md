# Metric Files

NanoTDB can build optional query-optimized `metric-<partition>.dat` files alongside the normal raw `data-<partition>.dat` ingest files.

Raw files preserve ingest order and are the write-path source of truth. Metric files rewrite one partition into a query-optimized layout so range queries can scan one metric at a time instead of walking interleaved ingest pages.

Current status:
- `v2` shared-time metric files are the default format for auto-builds and `nanocli build metric`
- `v1` remains available only as an explicit comparison format via `nanocli build metric --format v1`
- readers, verification, and `nanocli inspect metric` are version-aware and support both `v1` and `v2`

## What They Are For

Metric files are useful when:
- your queries are mostly per-metric range scans
- your raw ingest pages contain many interleaved metrics
- you want better compression on metric-local payloads
- you want to test size vs query-latency tradeoffs on your real dataset

Metric files are optional. If none exist, NanoTDB reads raw `data-*.dat` files.

## Config

Metric-file behavior is controlled from `[metrics]` in `engine.toml`:

```toml
[metrics]
enabled = false
compression = "zstd_fastest"
time_cache_slots = 256
raw_ingest_action = "keep"
```

Meaning:
- `enabled = false`: sealed partitions are not auto-converted; build metric files manually with `nanocli build metric` if you want them
- `enabled = true`: when a partition is sealed out of the active ingest window, NanoTDB attempts to build `metric-*.dat` for it automatically
- `compression`: metric-file block codec for new builds
- `time_cache_slots`: how many decoded shared time frames the process keeps in memory for `v2` reads
- `raw_ingest_action`: what happens to the source raw partition after a successful build

Query behavior is independent of `enabled`: when `metric-*.dat` exists for a
partition, `QueryRange` prefers it and falls back to raw files when the metric
file is absent. `nanocli query --metric-files off` remains the explicit override
when you want to benchmark or inspect raw-only query behavior.

Supported codecs:
- `s2`: fastest baseline encoder, usually larger files
- `s2_better`: slower S2 mode, somewhat smaller files
- `zstd_fastest`: usually the best default balance for NanoTDB metric payloads
- `zstd_default`: slowest build, usually the smallest built-in metric files

Supported raw ingest actions:
- `keep`: leave `data-<partition>.dat` in place
- `rename`: rename it to `raw-<partition>.dat`
- `delete`: remove it after successful metric-file build

Internal metrics expose the shared `v2` time-frame cache under:
- `internal/nanotdb/metric_file/time_cache_entries`
- `internal/nanotdb/metric_file/time_cache_bytes`
- `internal/nanotdb/metric_file/time_cache_max_entries`
- `internal/nanotdb/metric_file/time_cache_hits`
- `internal/nanotdb/metric_file/time_cache_misses`
- `internal/nanotdb/metric_file/time_cache_evictions`

## Build Metric Files

If `enabled = true`, newly sealed partitions are auto-built best-effort during
ingest. Existing older partitions are not retroactively rebuilt just because the
flag changed; use `nanocli build metric` to backfill metric files for partitions
that already exist on disk.

The builder discovers both `data-<partition>.dat` and `raw-<partition>.dat`
source files. That matters when `raw_ingest_action = "rename"`, because later
rebuild or verification runs still work against the renamed raw source.

Build all discovered partitions for one database:

```bash
./nanocli build metric --root ~/nanotdb-data --db sensors --verify
```

The default build format is `v2`. Use `--format v1` only when you want an explicit comparison build against the legacy layout.

Build one specific partition:

```bash
./nanocli build metric --root ~/nanotdb-data --db sensors --part 2026-05-03 --verify
```

Build the legacy `v1` format explicitly for comparison:

```bash
./nanocli build metric --root ~/nanotdb-data --db sensors --part 2026-05-03 --format v1 --verify
```

Override codec or raw-file handling for one run without editing config:

```bash
./nanocli build metric \
  --root ~/nanotdb-data \
  --db sensors \
  --codec zstd_default \
  --raw-ingest-action keep \
  --verify
```

`--verify` runs the raw-vs-metric correctness comparison after each build. For the default path this is version-aware; for `--format v1` it uses the legacy v1 checker.

## Query Routing And Tradeoffs

Metric files change query layout, not the logical result stream.

- `QueryRange` prefers `metric-*.dat` whenever it exists for a partition
- if a metric file is missing, queries fall back to the raw `data-*.dat` or `raw-*.dat` source
- `nanocli query --metric-files off` forces raw-only reads for diagnostics or benchmarks
- `nanocli query --metric-files on` forces metric-file reads whenever available

`v2` reduces one of `v1`'s main costs by storing shared time frames once per
file instead of repeating one timestamp vector per metric frame. `v1` is still
useful for controlled comparisons, but on interleaved real-world workloads it
can be materially larger than the source raw partitions.

## Inspect Metric Files

Inspect metric-file summaries:

```bash
./nanocli inspect metric --root ~/nanotdb-data --db sensors
```

Summary output is version-aware. For `v2` files it also reports the number of shared `time_frames` in each file.

Inspect with per-frame detail:

```bash
./nanocli inspect metric --root ~/nanotdb-data --db sensors --verbose
```

Verbose mode validates full frame payloads for both `v1` and `v2` files.

## Benchmark On Your Own Data

Use [scripts/benchmark_metric_files.sh](/home/ayman/code/nanotdb/scripts/benchmark_metric_files.sh) with a prebuilt `nanocli` binary. This does not require Go.

Example:

```bash
./scripts/benchmark_metric_files.sh \
  --nanocli ./nanocli \
  --root ~/nanotdb-data \
  --db sensors \
  --metric 'cpu.*' \
  --repeats 7
```

What the script does:
1. measures a raw-query baseline with `nanocli query --metric-files off`
2. copies your root data directory into a temporary work directory for each codec
3. builds metric files with `nanocli build metric --codec <name> --raw-ingest-action keep --verify`
4. measures metric-backed query time with `nanocli query --metric-files on`
5. prints one table showing size and performance tradeoffs per codec

The output columns are intended to answer two questions quickly:
- how much smaller or larger are the metric files than the raw partitions?
- how much query latency do I save for the metrics I actually query?

Typical interpretation:
- lower `metric_bytes` is better for disk
- lower `build_ms` is better for rebuild speed
- lower `metric_query_avg_ms` is better for reads
- higher `metric_speedup` means metric files are helping more

If your workload is query-heavy and rebuilds are rare, `zstd_default` may win. If you rebuild often and want a safer default, `zstd_fastest` is usually the first codec to test.

## Related Tools

- [scripts/regenerate_metric_files.sh](/home/ayman/code/nanotdb/scripts/regenerate_metric_files.sh): rebuild all partitions for one database with `nanocli`
- [docs/DESIGN.md](/home/ayman/code/nanotdb/docs/DESIGN.md): on-disk metric file format and config mapping