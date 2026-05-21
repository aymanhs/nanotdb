# Metric Files

NanoTDB can build optional query-optimized `metric-<partition>.dat` files alongside the normal raw `data-<partition>.dat` ingest files.

Raw files preserve ingest order and are the write-path source of truth. Metric files rewrite one partition into metric-local frames so range queries can scan one metric at a time instead of walking interleaved ingest pages.

## What They Are For

Metric files are useful when:
- your queries are mostly per-metric range scans
- your raw ingest pages contain many interleaved metrics
- you want better compression on metric-local payloads
- you want to test size vs query-latency tradeoffs on your real dataset

Metric files are optional. If you leave them disabled, NanoTDB continues to read only raw `data-*.dat` files.

## Config

Metric-file behavior is controlled from `[metrics]` in `engine.toml`:

```toml
[metrics]
enabled = false
compression = "zstd_fastest"
raw_ingest_action = "keep"
```

Meaning:
- `enabled = false`: `QueryRange` ignores `metric-*.dat` and reads raw files only
- `enabled = true`: `QueryRange` prefers `metric-*.dat` when present and falls back to raw files otherwise
- `compression`: metric-file block codec for new builds
- `raw_ingest_action`: what happens to the source raw partition after a successful build

Supported codecs:
- `s2`: fastest baseline encoder, usually larger files
- `s2_better`: slower S2 mode, somewhat smaller files
- `zstd_fastest`: usually the best default balance for NanoTDB metric payloads
- `zstd_default`: slowest build, usually the smallest built-in metric files

Supported raw ingest actions:
- `keep`: leave `data-<partition>.dat` in place
- `rename`: rename it to `raw-<partition>.dat`
- `delete`: remove it after successful metric-file build

## Build Metric Files

Build all discovered partitions for one database:

```bash
./nanocli metric build --root ~/nanotdb-data --db sensors --verify
```

Build one specific partition:

```bash
./nanocli metric build --root ~/nanotdb-data --db sensors --part 2026-05-03 --verify
```

Override codec or raw-file handling for one run without editing config:

```bash
./nanocli metric build \
  --root ~/nanotdb-data \
  --db sensors \
  --codec zstd_default \
  --raw-ingest-action keep \
  --verify
```

`--verify` runs the raw-vs-metric correctness comparison after each build.

## Inspect Metric Files

Inspect metric-file summaries:

```bash
./nanocli inspect metric --root ~/nanotdb-data --db sensors
```

Inspect with per-frame detail:

```bash
./nanocli inspect metric --root ~/nanotdb-data --db sensors --verbose
```

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
3. builds metric files with `nanocli metric build --codec <name> --raw-ingest-action keep --verify`
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