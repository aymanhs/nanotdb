# NanoTDB

<p align="center">
  <img src="docs/NanoTDB.png" alt="NanoTDB mascot" width="220">
</p>

**One binary. Local files. Built-in dashboard, editor, Explore, and offline CLI.**

NanoTDB is a single-binary time-series database with a browser dashboard, a
drag-and-edit dashboard *editor*, an ad-hoc Explore view, and an offline CLI —
all in one program. Drop it on a Raspberry Pi, edge box, appliance, or any
machine where standing up a TSDB plus a dashboard service plus a collector
stack is heavier than the problem you're solving.

You get metric ingest, range queries, rollups, retention, recovery, a
dashboard you can edit in the browser, and `nanocli` to inspect or export
data offline — without assembling anything else.

---

## What makes it different

Most "small" TSDBs hand you the storage and tell you to wire up your own UI,
collector, and operations. NanoTDB ships them together and keeps them honest:

- **Built-in dashboard AND editor.** Edit groups, widgets, and series in the
  browser. Validate, preview, save. No separate Grafana, no second service to
  run, no JSON file you have to edit by hand (though you can — it's just
  [`dashboard.json`](docs/DASHBOARD.md)).
- **Offline `nanocli`.** Inspect WAL, catalog, manifest, `.dat` files, export
  to line protocol, recompute rollups, run aggregate queries — all directly
  against the data directory, with the server stopped or running.
- **Files you can read.** Append-only `.dat` pages, a single reusable WAL per
  database, a JSON catalog, a TOML manifest. Retention is partition-file
  deletion. There is no opaque storage layer between you and your data.
- **Recoverable after crash or power loss.** Recent samples are WAL-protected
  and replayed on restart. Tunable from `segment` fsync to per-append `always`.
  Important on SD-backed edge boxes.
- **Built-in rollups.** Long-horizon downsampling lives in the same engine —
  define `[rollups]` in a manifest and you get min/max/avg/sum/count series
  in a destination database, with offline backfill and cascading rollups.
- **SD-friendly footprint.** Append-only, partitioned, S2-compressed. A real
  Raspberry Pi workload runs ~700k samples/day in under 1 MB on disk (see
  below).
- **Optional [`drip`](docs/DRIP.md) collector.** CPU, memory, disk, IO,
  network, load, one-wire temperature, and SD-write-probe metrics, ready to
  POST into NanoTDB.

---

## See it

<figure align="center">
  <img src="docs/nano-dashboard.png" alt="NanoTDB dashboard showing CPU and memory widgets">
  <figcaption><em>Mobile-friendly dashboard — live operational view from one local NanoTDB.</em></figcaption>
</figure>

<figure align="center">
  <img src="docs/dashboard-wide.png" alt="NanoTDB wide desktop dashboard layout" width="900">
  <figcaption><em>Wide desktop layout for denser placement.</em></figcaption>
</figure>

<figure align="center">
  <img src="docs/explore.png" alt="NanoTDB Explore view with metric picker and live chart" width="440">
  <figcaption><em>Explore — ad-hoc metric picker, last-value cards, live chart.</em></figcaption>
</figure>

<figure align="center">
  <img src="docs/dashboard-editor.png" alt="NanoTDB dashboard editor" width="440">
  <figcaption><em>In-browser editor — groups, widgets, series, preview, validate, save.</em></figcaption>
</figure>

---

## 60-second Hello World

Terminal 1:

```bash
mkdir -p ~/nanotdb-data
./nanotdb --init --config ~/nanotdb-data/engine.toml
./nanotdb --config ~/nanotdb-data/engine.toml
```

Terminal 2:

```bash
curl -X POST "http://localhost:8428/api/v1/import" \
  -d $'demo/room.temp 21.5\ndemo/room.humidity 48'

curl "http://localhost:8428/api/v1/query?query=demo/room.temp"

./nanocli inspect wal --root ~/nanotdb-data --db demo --verbose
```

Then open <http://localhost:8428/> for the dashboard, `/explore` for ad-hoc
charts, `/dashboard/edit` for the editor.

For the longer version see [docs/HELLO_WORLD.md](docs/HELLO_WORLD.md) or
[docs/GETTING_STARTED.md](docs/GETTING_STARTED.md).

---

## Real footprint on a Raspberry Pi

Actual live data from one Pi running NanoTDB + `drip` with a handful of
DS18B20 temperature sensors, ~12 consecutive days, 10-second cadence:

| Day        | Metrics | Points  | Metric file size |
|------------|--------:|--------:|-----------------:|
| 2026-05-22 |      83 | 693,091 |           757 KB |
| 2026-05-23 |      83 | 717,036 |           935 KB |
| 2026-05-24 |      83 | 722,348 |           996 KB |
| 2026-05-25 |      83 | 725,986 |         1,015 KB |
| 2026-05-26 |      83 | 716,709 |           982 KB |
| 2026-05-27 |      83 | 716,708 |           897 KB |
| 2026-05-28 |      91 | 773,136 |           968 KB |
| 2026-05-29 |      91 | 785,971 |           831 KB |
| 2026-05-30 |      91 | 784,799 |           982 KB |
| 2026-05-31 |      91 | 779,780 |           885 KB |
| 2026-06-01 |      91 | 784,533 |           925 KB |
| 2026-06-02 |      91 | 785,882 |           907 KB |

Roughly **~1 MB/day per 700k–785k samples** across 83–91 metrics on this
real workload. That's under 1.3 bytes per point on disk after compression.
A typical Pi SD card holds *years* of this with room to spare — exactly
what the storage layout is tuned for.

---

## Best fit

| Good fit                                                          | Use something larger                                       |
|-------------------------------------------------------------------|------------------------------------------------------------|
| Single-binary observability on one machine                        | Distributed or horizontally scaled deployments             |
| Raspberry Pi, edge nodes, appliances, local app metrics           | Large fleets, high-cardinality multi-tenant workloads      |
| Hundreds of metrics you want local and inspectable                | Ecosystems where broad integrations matter more than simplicity |
| Built-in dashboard plus offline CLI workflow                      | Systems that need looser ordering guarantees               |

NanoTDB is **not** trying to be a distributed TSDB, a high-cardinality fleet
backend, or a system that accepts arbitrary out-of-order writes. It will tell
you that plainly — including in this README.

---

## Concepts in 60 seconds

A **database** is an isolated namespace (`prod`, `sensors`, `weather`) with
its own WAL, catalog, manifest, and partitioned `.dat` files. A **metric** is
one numeric time-ordered stream inside a database; type (`int32` or
`float32`) is fixed on first write. A **sample** is one `(timestamp, value)`
pair, written in line protocol:

```text
DB/metric.name value [timestamp_ns]
```

Examples:

```text
prod/room.temp 21.5 1715000000000000000
sensors/pressure.hpa 1013
weather/outdoor.humidity 48
```

For a friendly walkthrough — what a database is, how multiple metrics live
inside one DB, what happens when a partition seals, when `metric-*.dat`
files appear, and how to tune the WAL for resilience vs SD wear — see
[docs/CONCEPTS.md](docs/CONCEPTS.md). For the canonical reference, see
[docs/GLOSSARY.md](docs/GLOSSARY.md) and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

---

## Documentation

### Start here

- [docs/HELLO_WORLD.md](docs/HELLO_WORLD.md) — fastest copy/paste path.
- [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md) — installation, examples, walkthrough.
- [docs/RUN_AS_A_SERVICE.md](docs/RUN_AS_A_SERVICE.md) — systemd setup for Pi / Linux.

### Use the UI

- [docs/DASHBOARD.md](docs/DASHBOARD.md) — dashboard, editor, Explore, `dashboard.json`.
- [docs/DRIP.md](docs/DRIP.md) — the optional host metrics collector.

### Reference

- [docs/CONFIGURATION.md](docs/CONFIGURATION.md) — `engine.toml` and per-database `manifest.toml`.
- [docs/HTTP_API.md](docs/HTTP_API.md) — `/api/v1/*` endpoints, request/response shapes.
- [docs/NANOCLI.md](docs/NANOCLI.md) — offline CLI commands, timestamp formats, examples.
- [docs/ROLLUPS.md](docs/ROLLUPS.md) — downsampling jobs, cascades, backfill.
- [docs/METRIC_FILES.md](docs/METRIC_FILES.md) — optional query-optimized storage and benchmarks.
- [docs/RECOVERY.md](docs/RECOVERY.md) — WAL behavior, durability profiles, tuning.
- [docs/EMBEDDING.md](docs/EMBEDDING.md) — embedding the engine in a Go program.

### Concepts

- [docs/CONCEPTS.md](docs/CONCEPTS.md) — friendly walkthrough: databases, metrics, partitions, WAL, `data-*.dat` vs `metric-*.dat`, durability tuning.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — storage and query walkthrough.
- [docs/GLOSSARY.md](docs/GLOSSARY.md) — canonical terms.
- [docs/DESIGN.md](docs/DESIGN.md) — deeper design rationale.
- [docs/LAWS.md](docs/LAWS.md) — invariants the code upholds.

---

## Install

Prebuilt binaries are on [GitHub Releases](https://github.com/aymanhs/nanotdb/releases/latest):

- Raspberry Pi 0/1: `nanotdb-linux-armv6-rpi0-rpi1`, `nanocli-linux-armv6-rpi0-rpi1`
- Raspberry Pi 2/3/4 (32-bit): `nanotdb-linux-armv7-rpi3-rpi4`, `nanocli-linux-armv7-rpi3-rpi4`
- Raspberry Pi (64-bit): `nanotdb-linux-arm64`, `nanocli-linux-arm64`
- Linux x86_64, macOS Intel/Apple Silicon, Windows x64/ARM64 also available

Or build from source:

```bash
go build -o nanotdb ./cmd/nanotdb
go build -o nanocli ./cmd/nanocli
go build -o drip    ./cmd/drip   # optional collector
```

Full install matrix in [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md).

---

## Release history

- [Releases](https://github.com/aymanhs/nanotdb/releases) — published notes and downloads.
- [CHANGELOG.md](CHANGELOG.md) — detailed in-repo change log.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the branch model and release workflow.

## License

See [LICENSE](LICENSE).
