# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- Dynamic aggregate range queries with shared aggregate execution in one scan, plus dashboard/editor/explore support for aggregate-backed charts and query-driven widgets.
- A first-class `aggregate_band` dashboard widget with shorthand config expansion, interval-as-window behavior, batched aggregate fetching, and simplified editor support for min/avg/max band charts.
- Built-in aggregate discovery and query surfaces, including `/api/v1/aggregates`, `nanocli query --aggregate ... --window ...`, and new aggregate options beyond the original min/max/sum/avg/count set: `median`, `p50`, `p95`, `p99`, `trimmed_avg`, and the `trimmed_average` alias.
- Engine inspector WAL and runtime surfaces for live file/state inspection, including WAL previews, recent flush history, process RSS, Go runtime memory stats, process age, and cross-database open-page visibility.
- `nanocli inspect catalog` for listing a database's registered metrics from `catalog.json` with name, id, and type in table or JSON form.
- Per-file engine inspector compaction actions to build query-optimized metric `v2` files from sealed raw partitions with status and size reporting.
- Per-database `retention.retention_action` manifest support with `keep|delete|archive`, partition-family cleanup for `data-`/`raw-`/`metric-` files, hardcoded tar archive buckets for expired sealed partitions, and cutoff-based expiry semantics so daily partitions roll into monthly archives one expired partition at a time instead of all at once at month boundaries.

### Changed
- Shared raw-partition metric-file preparation helpers were moved into a neutral engine file and renamed away from `v1`-specific terminology, clarifying that both legacy `v1` and default `v2` metric builds use the same partition scan/coalesce path.
- Dashboard, editor, explore, and engine web surfaces now use a cleaner tabbed navigation/header treatment, simpler refresh controls, and more consistent aggregate-window query/chart ergonomics.
- Query-range `step`/UI resolution selection now maps to aggregate buckets using the server default aggregate (`avg` by default), and dashboard line-chart aggregate series now use widget interval as their shared bucket window instead of per-series windows.
- Engine runtime inspection now reports process-wide state instead of one selected database, while the Files and WAL tabs split disk inspection from live WAL diagnostics.
- The `trimmed_avg` aggregate now trims one low/high outlier for small sample sets and uses a lighter 5% per-tail trim for larger windows instead of the previous fixed 10% rule.
- README, getting-started, and metric-file docs were refreshed around aggregate-backed queries/widgets, built-in inspector compaction actions, and the updated dashboard/editor/explore workflow.

### Fixed
- Default `v2` metric-file builds now tolerate sealed raw partitions whose per-metric samples appear out of timestamp order in raw page append order by normalizing each metric stream during build and verification.
- Query APIs and web consumers now treat `end` as optional, stabilize aggregate range stepping, and collapse duplicate aggregate-band backend requests into shared multi-aggregate fetches.
- `nanocli query` now accepts relative durations for both `--start` and `--end`, including day/week suffixes such as `1d` and `1w`, treats an omitted `--metric` as all metrics, defaults missing `--end` to `now` for range queries, and avoids repeated per-metric rescans by sharing one partition pass across multi-metric range queries.
- Rollup checkpoint logs now compact automatically once `rollup.checkpoints.log` grows past 100 KB, rewriting the file down to the latest checkpoint per job instead of growing indefinitely.
- Engine WAL UI now avoids redundant scanned/live preview sections, renders unset flush timestamps as `never`, and the CodeQL dismiss workflow YAML no longer fails parsing because of an unquoted colon-containing comment string.
- Trimmed-average aggregate coverage now locks in the small-sample minimum trim and the 50-point 5% per-tail behavior through focused engine tests.

## [1.3.0] - 2026-05-24

### Added
- Metric-file `v2` shared-time format with version-aware readers, a dedicated `CompareDataAndMetricPartitionV2` checker, and shared decoded time-frame caching for query reuse.
- Engine config support for `[metrics].time_cache_slots` plus internal cache metrics under `internal/nanotdb/metric_file/time_cache_*`.
- Version-aware metric inspection so `nanocli inspect metric` can summarize and fully validate both legacy `v1` and default `v2` metric files.
- Structured `slog` logging configuration via `[logging]` / `[[logging.logger]]`, plus file-only diagnostic logging controls for `nanocli` with `--log-file` and `--log-level`.
- Task-oriented onboarding docs with a copy/paste Hello World guide, a dedicated architecture page, a brief systemd service guide, glossary-linked terminology references, and expanded dashboard/editor/explore documentation.

### Changed
- Metric-file builds now default to `v2` for sealed-partition auto-builds and `nanocli build metric`; legacy `v1` builds remain available only through explicit comparison flows such as `nanocli build metric --format v1`.
- Metric-file verification and inspection paths are now format-aware, and metric-file docs/design docs were updated to reflect the shipped `v2` default workflow and cache configuration.
- The dashboard and Explore UI now use a more consistent refresh model, improved chart stability, wider metric selection, and more polished presentation.
- The main README now positions NanoTDB more clearly around edge and single-node use cases, WAL-backed recovery, SD-friendly storage, real Raspberry Pi footprint examples, and fit-versus-tradeoff guidance instead of leading with internals.

### Fixed
- Auto-built sealed metric files now validate through the default version-aware checker instead of assuming the legacy trailer format.
- `nanocli inspect metric` no longer assumes `v1` page walkers when reading the default `v2` metric-file format.
- WAL reset now flushes eligible non-current open day pages during ingest, fixing a case where a stale pre-midnight page could block WAL truncation after midnight and let the active WAL grow far larger than expected.
- `nanocli inspect wal` now reports sane WAL start/duration timestamps for live current-era WAL files, avoiding misleading `1970-01-01` ranges in inspection output.

## [1.2.0] - 2026-05-19

### Added
- Structured `slog`-based logging support for NanoTDB via `[logging]` / `[[logging.logger]]` configuration and shared engine logging infrastructure.
- File-based diagnostic logging flags for `nanocli` through `--log-file` and `--log-level`.
- New documentation pages for architecture, Hello World onboarding, and running NanoTDB as a service.

### Changed
- Startup, configuration, and getting-started docs were refreshed, including a broader README rewrite and bundled project artwork.
- Default engine configuration and design docs were updated to document the new logging controls.

### Fixed
- WAL reset and data-file close handling now flush eligible non-current open day pages before truncation, preventing a stale pre-midnight page from blocking WAL cleanup and allowing WAL files to grow far beyond expectations.
- Added test coverage around CLI logging flag parsing, engine logging setup, and WAL durability behavior.

## [1.1.2] - 2026-05-18

### Changed
- GitHub release publishing now builds and uploads `drip` binaries only for Linux targets, matching the collector's Linux-oriented implementation.
- Getting-started install docs now describe `drip` release assets as Linux-only and keep macOS/Windows release lists to `nanotdb` and `nanocli`.

## [1.1.1] - 2026-05-18

### Added
- Release workflow support for publishing `drip` binaries was added.

### Changed
- Getting-started install docs were updated to mention prebuilt `drip` binaries.

## [1.1.0] - 2026-05-18

### Added
- Code cleanup, addressing linter warnings.
- `drip`: Raspberry Pi/edge metrics collector with CPU, memory, disk, IO, network, loadavg, one-wire, and SD write probe collectors.
- Selector-based rollup config with `source_pattern`, per-DB rollup defaults, and wildcard exclusion lists.
- Engine-owned rollup backfill workflow with `nanocli rollup` and `POST /api/v1/rollup/backfill` entry points.
- `nanocli inspect dat --verbose` per-page table output with page size/compression stats.
- `nanocli inspect wal --verbose` aligned table output with optional tail diagnostics.

### Changed
- Rollup config docs and default engine template now document wildcard job expansion, defaults, and exclusion semantics.
- Rollup destination manifests now persist resolved rollup defaults so reopened chained rollup DBs keep the same partition and page settings used during rollup writes.
- Rollup backfill now persists rebuilt destination data and catalog state to disk before returning so offline tools can immediately inspect/export rebuilt DBs.
- Same-destination rollup jobs are executed period-by-period in grouped order to reduce tiny overlapping frames during backfill and chained rollups.
- `nanocli inspect db --verbose` now uses aligned DAT/WAL detail tables with `start` + `duration` summaries.

## [1.0.1] - 2026-05-17

### Added
- GitHub Actions release workflow to build and publish multi-platform binaries.
- Raspberry Pi release targets for old and new models:
  - `linux-armv6-rpi0-rpi1`
  - `linux-armv7-rpi3-rpi4`
  - `linux-arm64`
- Beginner-focused getting started guide with install/build/push/query examples.
- REST discovery endpoints:
  - `GET /api/v1/databases`
  - `GET /api/v1/metrics?db=<name>`

### Changed
- README quick start now includes direct links to prebuilt release binaries.
- Added API discovery quick reference to README.
- Added discovery endpoint curl + JSON examples in getting-started docs.
- Updated getting-started Python HTTP examples to match current import success status (`200`).

## [1.0.0] - 2026-05-17

### Added
- Initial public release baseline.

---

## Release Notes

- End-user binary downloads: GitHub Releases page
  https://github.com/aymanhs/nanotdb/releases
- Detailed project history: this file (`CHANGELOG.md`)

[Unreleased]: https://github.com/aymanhs/nanotdb/compare/v1.3.0...HEAD
[1.3.0]: https://github.com/aymanhs/nanotdb/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/aymanhs/nanotdb/releases/tag/v1.2.0
[1.1.2]: https://github.com/aymanhs/nanotdb/releases/tag/v1.1.2
[1.1.1]: https://github.com/aymanhs/nanotdb/releases/tag/v1.1.1
[1.1.0]: https://github.com/aymanhs/nanotdb/releases/tag/v1.1.0
[1.0.1]: https://github.com/aymanhs/nanotdb/releases/tag/v1.0.1
[1.0.0]: https://github.com/aymanhs/nanotdb/releases/tag/v1.0.0
