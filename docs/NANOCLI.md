# nanocli

`nanocli` is the offline CLI for NanoTDB. It operates directly on a data
directory — the server can be stopped, running, or doesn't have to exist
yet (for import).

This is one of NanoTDB's main operational properties: you can inspect the
WAL, catalog, manifests, and `.dat` files at any time without a running
service.

---

## Command summary

```text
nanocli inspect db       --root <dir> --db <name> [--verbose] [--json]
nanocli inspect dat      --root <dir> --db <name> [--verbose] [--json]
nanocli inspect catalog  --root <dir> --db <name> [--json]
nanocli inspect wal      --root <dir> --db <name> [--verbose] [--json]
nanocli inspect metric   --root <dir> --db <name> [--verbose] [--json]

nanocli import           --root <dir> --in <file.lp>  [--json]
nanocli export           --root <dir> --db <name> [--out <file.lp>]

nanocli rollup           --root <dir> [--db <source-db>] [--json]

nanocli build metric     --root <dir> --db <name>
                         [--part <partition>] [--format <v2|v1>]
                         [--codec <name>] [--raw-ingest-action <keep|rename|delete>]
                         [--verify] [--json]

nanocli query            --root <dir> --db <name> [--metric <regex>]
                         [--start <time|duration>] [--end <time>]
                         [--aggregate <list> --window <duration>]
                         [--metric-files <config|on|off>] [--format table|json]
```

Verbose terminal output uses aligned tables. Human-readable mode shows `start`
plus `duration`; `--json` keeps raw timestamps for machine consumption.

---

## Logging

`nanocli` stays quiet by default and only writes diagnostics if you opt in
with `--log-file`.

- `--log-file <path>` enables file logging.
- `--log-level <info|debug|trace>` sets the level (defaults to `debug` when
  `--log-file` is given on its own).
- `--log-level` without `--log-file` is rejected.

```bash
./nanocli inspect wal --root ~/nanotdb-data --db sensors \
  --log-file /tmp/nanocli.log --log-level trace
```

---

## Inspect

```bash
./nanocli inspect db      --root ~/nanotdb-data --db sensors --verbose
./nanocli inspect dat     --root ~/nanotdb-data --db sensors --verbose
./nanocli inspect catalog --root ~/nanotdb-data --db sensors
./nanocli inspect wal     --root ~/nanotdb-data --db sensors --verbose
./nanocli inspect metric  --root ~/nanotdb-data --db sensors --verbose
```

- `inspect db` — database overview plus optional detailed DAT/WAL tables.
- `inspect dat` — per-file/per-page size and compression stats.
- `inspect catalog` — metric registry from `catalog.json`.
- `inspect wal` — per-file size/decode stats, with tail diagnostics in
  `--verbose`.
- `inspect metric` — query-optimized `metric-*.dat` files; see
  [METRIC_FILES.md](METRIC_FILES.md).

---

## Import / export

```bash
./nanocli import --root ~/nanotdb-data --in backup.lp
./nanocli export --root ~/nanotdb-data --db sensors --out backup.lp
```

`export` writes to stdout when `--out` is omitted.

### Timestamp formats

Import and exported line-protocol files use:

```text
YYYY-MM-DD HH:MM:SS.nnnnnnnnn   (UTC)
```

Import also accepts raw Unix nanoseconds.

---

## Query

```bash
./nanocli query --root ~/nanotdb-data --db sensors --metric 'living_room.*' --format table

./nanocli query --root ~/nanotdb-data --db sensors --start 2m --format table
```

`--start` / `--end` accept:

- RFC3339 (`2026-05-24T12:00:00Z`)
- `YYYY-MM-DD [HH[:MM[:SS[.nnnnnnnnn]]]]`
- Unix timestamps (seconds or nanoseconds)
- For `--start`, a duration like `2m` meaning `now-2m`

When `--metric` is omitted, all metrics in the database match.

### Aggregate queries

```bash
./nanocli query \
  --root ~/nanotdb-data \
  --db sensors \
  --metric '^temp\.out_dry$' \
  --start 2026-05-24T12:00:00Z \
  --end   2026-05-24T13:00:00Z \
  --aggregate min,max,sum,avg,count \
  --window 5m \
  --format table
```

Rules:

- Supported aggregates: `avg`, `count`, `max`, `median`, `min`, `p50`, `p95`,
  `p99`, `sum`, `trimmed_avg`, `trimmed_average`.
- `--aggregate` and `--window` are required together.
- `--start` is required; `--end` defaults to now.
- Must match exactly one metric after regex expansion.
- Rows are emitted at bucket-end timestamps; first and last bucket are
  clipped to the requested range.

### Force raw or metric-file routing

For diagnostics or benchmarking, override the metric-file routing:

```bash
./nanocli query --root ~/nanotdb-data --db sensors --metric 'cpu.*' --metric-files off --format json > /dev/null
./nanocli query --root ~/nanotdb-data --db sensors --metric 'cpu.*' --metric-files on  --format json > /dev/null
```

See [METRIC_FILES.md](METRIC_FILES.md) for the underlying mechanism.

---

## Build metric files

```bash
./nanocli build metric --root ~/nanotdb-data --db sensors --verify
./nanocli build metric --root ~/nanotdb-data --db sensors --part 2026-05-03 --verify
./nanocli build metric --root ~/nanotdb-data --db sensors --codec zstd_default --verify
./nanocli build metric --root ~/nanotdb-data --db sensors --part 2026-05-03 --format v1 --verify
```

The default builder writes the current `v2` metric-file format. Use
`--format v1` only when you want a direct comparison against the legacy
layout. Full reference in [METRIC_FILES.md](METRIC_FILES.md).

---

## Rollup backfill

```bash
./nanocli rollup --root ~/nanotdb-data            # all discovered sources
./nanocli rollup --root ~/nanotdb-data --db weather   # one source DB
```

For online backfill against a running server, use the HTTP API instead. See
[ROLLUPS.md](ROLLUPS.md).
