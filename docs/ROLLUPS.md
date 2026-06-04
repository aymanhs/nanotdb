# Rollups

Rollups produce lower-resolution derived series from your raw metrics. They
run inside the same engine — no separate compaction service, no external job
scheduler.

The classic use case: keep raw 10-second samples for a short window, plus
hourly and daily summaries for the long horizon, all in local files.

---

## Model

- Rollups are defined in the **source** database's `manifest.toml`.
- Each job reads from one or more source metrics and writes aggregate series
  into a **destination** database (which can be the same DB or a different one).
- Destination DBs can themselves define rollups, producing cascades
  (e.g. `1h → 1d`).
- A checkpoint log in the source DB tracks rollup progress per `(job, metric)`.
- Auto-created destination DBs use rollup-tuned manifests: WAL disabled,
  `partition = "month"` for sub-daily rollups (or `"year"` for daily+), and a
  longer `page.max_age` to reduce tiny sparse pages.

---

## Minimal example

```toml
[rollups]
enabled = true
checkpoint_file = "rollup.checkpoints.log"
default_grace = "5m"
default_interval = "1h"
default_destination_db = "sensors_rollup_1h"
default_aggregates = ["min", "max", "avg", "count"]
global_exclude_patterns = ["nanotdb.*", "*.debug"]

[[rollups.jobs]]
id = "all_metrics_1h"
source_pattern = "*"
exclude_patterns = ["disk.sd_write_probe_ms", "net.*"]

[[rollups.jobs]]
id = "outside_temp_1h"
source_metric = "temp.out_dry"
interval = "1h"
aggregates = ["min", "max", "sum", "avg", "count"]
destination_db = "sensors_rollup_1h"
destination_metric_prefix = "temp.out_dry"
```

Output names follow `<destination_metric_prefix>.<agg>` — for example
`temp.out_dry.min`, `temp.out_dry.max`, etc.

---

## Field reference

| Field                                     | Scope | Required | Valid / Default                                         | Notes                                                              |
|-------------------------------------------|-------|----------|---------------------------------------------------------|--------------------------------------------------------------------|
| `rollups.enabled`                         | DB    | no       | `true|false` (default `false`)                          | Enables rollup processing for this DB as a source.                 |
| `rollups.checkpoint_file`                 | DB    | no       | string (default `rollup.checkpoints.log`)               | Checkpoint log path, relative to source DB directory.              |
| `rollups.default_grace`                   | DB    | no       | Go duration or empty                                    | Used when job `grace` is omitted.                                  |
| `rollups.default_interval`                | DB    | no       | Go duration or empty                                    | Used when job `interval` is omitted.                               |
| `rollups.default_destination_db`          | DB    | no       | string or empty                                         | Used when job `destination_db` is omitted.                         |
| `rollups.default_aggregates`              | DB    | no       | subset of `min|max|sum|avg|count`                       | Used when job `aggregates` is omitted.                             |
| `rollups.global_exclude_patterns`         | DB    | no       | wildcard list                                           | Excluded from selector-based jobs before expansion.                |
| `rollups.jobs[].id`                       | Job   | yes      | non-empty string                                        | Unique per source DB for checkpoint tracking.                      |
| `rollups.jobs[].source_metric`            | Job   | yes\*    | non-empty string                                        | Exact metric to read from source DB.                               |
| `rollups.jobs[].source_pattern`           | Job   | yes\*    | wildcard pattern supporting `*`                         | Selector job that expands to concrete metrics.                     |
| `rollups.jobs[].exclude_patterns`         | Job   | no       | wildcard list                                           | Per-job exclusions for selector-based jobs.                        |
| `rollups.jobs[].interval`                 | Job   | no       | valid Go duration (`>0`)                                | Rollup bucket size; may inherit `rollups.default_interval`.        |
| `rollups.jobs[].aggregates`               | Job   | no       | `min|max|sum|avg|count`                                 | Aggregate outputs; inherits `default_aggregates`, else all five.   |
| `rollups.jobs[].destination_db`           | Job   | no       | non-empty string                                        | Target DB; inherits `default_destination_db`.                      |
| `rollups.jobs[].destination_metric_prefix`| Job   | no       | string (default `source_metric`)                        | Output names are `<prefix>.<agg>`.                                 |
| `rollups.jobs[].grace`                    | Job   | no       | Go duration or empty                                    | Overrides `default_grace` for this job.                            |

\* Each job must set exactly one of `source_metric` or `source_pattern`.

Other notes:

- Checkpoints live in the source DB (default `rollup.checkpoints.log`).
- Selector-based jobs expand to deterministic checkpoint keys:
  `<job-id>::<metric-name>`.

---

## Backfill

You'll backfill when you add new rollups, change interval/aggregates, or want
to rebuild a destination DB from existing source data.

### Offline rebuild with `nanocli`

When the server is stopped:

```bash
./nanocli rollup --root ~/nanotdb-data
./nanocli rollup --root ~/nanotdb-data --db weather   # one source DB only
```

### Online rebuild through the HTTP API

When `nanotdb` is running, use the engine-owned endpoint rather than editing
files under a live server:

```bash
curl -X POST "http://localhost:8428/api/v1/rollup/backfill" \
  -H 'Content-Type: application/json' \
  -d '{"source_db":"weather"}'

# Or rebuild every discovered rollup source:
curl -X POST "http://localhost:8428/api/v1/rollup/backfill" \
  -H 'Content-Type: application/json' \
  -d '{}'
```

The endpoint clears rebuildable destination state, reruns the rollups in the
engine, and persists rebuilt destination `.dat` pages and `catalog.json`
before returning. Offline `nanocli inspect/export` can read the rebuilt DB
immediately.

---

## End-to-end verification

For deterministic round-trip checks (generate LP → import → rollups →
export → compare expected):

```bash
./scripts/rollup_full_cycle_check.sh
```

Optional arguments:

```bash
./scripts/rollup_full_cycle_check.sh \
  <root-dir> <duration-hours> <metrics> <cadence-seconds> <gap-metrics>
```

Defaults: `root-dir=test-data/full-cycle-check`, `duration-hours=30`,
`metrics=10`, `cadence-seconds=10`, `gap-metrics=2`.

Generated artifacts land in `<root-dir>/work`:

- `scenario_summary.json` — duration, rates, counts, per-metric stats
- `known_gaps.csv` — deterministic missing windows for `temp.gap_probeXX` metrics
- `SCENARIO.md` — quick human-readable summary
