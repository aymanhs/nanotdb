# Configuration

NanoTDB has two layers of configuration:

- `engine.toml` — engine-wide settings (one per data root)
- `<db>/manifest.toml` — per-database settings (retention, partitioning, WAL,
  page limits, rollups)

`engine.toml` is created automatically when you run `nanotdb --init`. Per-database
manifests are created when a database is first written to, using the defaults
in `engine.toml`'s `[manifest_defaults]`.

---

## `engine.toml`

### Top-level engine settings

| Key                    | Default               | Effect                                                  |
|------------------------|-----------------------|---------------------------------------------------------|
| `engine.listen`        | `:8428`               | HTTP server address                                     |
| `wal.max_segment_size` | `67108864` (64 MiB)   | WAL size before reset after a page flush                |
| `wal.fsync_policy`     | `segment`             | `segment` = fsync on WAL reset; `always` = fsync every append |
| `durability.profile`   | `strict`              | `strict` / `balanced` / `throughput`                    |
| `stats.enabled`        | `true`                | Emit engine self-metrics to the `internal` database     |
| `stats.interval`       | `30s`                 | How often stats are flushed                             |

### Durability profiles

| Profile      | Page file fsync | Catalog fsync |
|--------------|-----------------|---------------|
| `strict`     | yes             | yes           |
| `balanced`   | yes             | no            |
| `throughput` | no              | no            |

See [RECOVERY.md](RECOVERY.md) for the full tuning discussion.

### `[logging]`

Engine and server logging are configured under `[logging]` with one or more
`[[logging.logger]]` entries.

```toml
[logging]

[[logging.logger]]
output = "console"
level = "info"

[[logging.logger]]
output = "/var/log/nanotdb/debug.log"
level = "debug"
```

Rules:

- `output = "console"` writes to stderr.
- Any other `output` is treated as a file path and opened in append/create mode.
- `level` can be `info`, `debug`, or `trace`.
- Multiple logger entries are allowed — keep operator-facing console logs sparse
  while writing more detail to a file.

Level guidance:

- `info`: startup, shutdown, database open/replay, backfill begin/end.
- `debug`: page flushes, WAL resets, rollup trigger boundaries, file lifecycle.
- `trace`: per-sample ingest flow, new-metric creation, stale/out-of-order
  rejection, HTTP request summaries.

### `[metrics]`

Controls optional query-optimized metric files. Full reference in
[METRIC_FILES.md](METRIC_FILES.md).

```toml
[metrics]
enabled = false
compression = "zstd_fastest"
time_cache_slots = 256
raw_ingest_action = "keep"
```

### `[web]`

Dashboard, editor, Explore, and engine view settings. Full reference in
[DASHBOARD.md](DASHBOARD.md#web-config).

```toml
[web]
enabled = true
base_path = "/dashboard"
explore_path = "/explore"
engine_path = "/engine"
title = "NanoTDB Dashboard"
refresh_seconds = 10
dashboard_config = "dashboard.json"
web_root = "ui"
api_base_url = ""
```

### `[manifest_defaults]`

Defaults applied when a new per-database manifest is created. Anything you can
set in `<db>/manifest.toml` can also be defaulted here.

---

## `<db>/manifest.toml`

Per-database settings. The most common sections are `[retention]`, `[wal]`,
`[page]`, and `[rollups]`.

### `[retention]`

```toml
[retention]
partition = "day"
retention_action = "keep"
```

Partition options:

- `partition = "day"` (default): `data-YYYY-MM-DD.dat`
- `partition = "month"`:          `data-YYYY-MM.dat`
- `partition = "year"`:           `data-YYYY.dat`
- `partition = "forever"`:        `data-forever.dat`

Retention action:

- `keep` (default): leave expired sealed partition files in place.
- `delete`: remove expired partition families.
- `archive`: append expired partition families into tar buckets, then remove
  originals.

Retention acts on the whole expired partition *family*, not only raw data
pages. If these files exist for the same expired partition, they are processed
together:

- `data-<partition>.dat`
- `raw-<partition>.dat`
- `metric-<partition>.dat`
- `events-<partition>.dat`

Archive bucket names follow the partition mode:

- `partition = "day"`     → `archive-YYYY-MM.tar`
- `partition = "month"`   → `archive-YYYY.tar`
- `partition = "year"`    → `archive-forever.tar`
- `partition = "forever"` → not supported with `retention_action = "archive"`

### `[wal]`

- `enabled` — enable or disable WAL for this database.
- `skip_before` — skip WAL for older backfill samples (useful for bulk import).

### `[page]`

- `max_records` — how many samples an in-memory page holds before flushing.
- `max_bytes`   — byte limit for the same.
- `max_age`     — wall-clock age limit; long-idle pages still flush.

These control how quickly recent data leaves the WAL-protected window and
lands in durable `.dat` files.

### `[rollups]`

Downsampling job definitions. See [ROLLUPS.md](ROLLUPS.md) for the full
reference.

### `[events]`

Per-database events storage is opt-in and has independent page/WAL tuning.

- `enabled` — enable events for this DB.
- `max_payload_bytes` — per-event payload cap.
- `max_in_memory_bytes` — spike-protection ceiling that force-flushes the open events page.

Nested page knobs:

- `[events.page].max_records`
- `[events.page].max_bytes`
- `[events.page].max_age`

Nested events WAL knobs:

- `[events.wal].max_segment_size`
- `[events.wal].fsync_policy` (`segment` or `always`)

---

## Choosing a durability posture

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

See [RECOVERY.md](RECOVERY.md) for the reasoning, especially on SD-backed
edge systems.
