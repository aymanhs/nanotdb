# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- New `--timestamp-unit ns|us|ms|s` flag on `nanocli query` and `nanocli import parts`, plus the equivalent `timestamp_unit` query parameter on the web API range endpoints; default is `ns`. Bare numeric timestamps are no longer auto-bucketed by magnitude.
- `engine.ParseDuration` accepts `d` (days) and `w` (weeks) on top of the stdlib units. Used everywhere durations are configured — manifest fields (`grace`, `wal.skip_before`, `page.max_age`, `rollups.*`), the web `window` parameter, and `nanocli` `--start` / `--end` / `--window`.
- Strict database-name allowlist (`[A-Za-z0-9_.-]`, 1–64 bytes, cannot start with `.` or `-`) enforced at every ingress — line protocol, `AddSample`, HTTP `?db=` parameters, rollup backfill, and offline import/export.
- HTTP server now sets `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, and `MaxHeaderBytes` in addition to `ReadHeaderTimeout`.
- Manifest field upper bounds rejected at load time: `max_active_days ≤ 1000`, `retention_days ≤ 36500`, `page.max_records ≤ 65535`, `page.max_bytes ≤ 64 MiB`, `metrics.time_cache_slots ≤ 1M`.

### Changed
- Manifests now require `retention.retention_action` to be set explicitly (`keep|delete|archive`). Pre-1.4 manifests without this field will fail to open until the operator chooses a value — this prevents a silent switch to `keep` (no pruning) on upgrade.
- `nanocli query --end` defaults to `MaxInt64` (unbounded) again, restoring 1.3-era behaviour for future-dated samples (clock skew, backfills, replication lag).
- Engine API responses no longer leak absolute filesystem paths; data/metric/WAL file paths are returned relative to the engine root, and dashboard `backup_path` is returned as basename only.
- `Engine.Close()` now flushes/closes every database even when one of them errors, returning a joined error rather than stopping at the first failure.
- Metric names are now capped at 255 bytes; `/` remains forbidden (1.4 behaviour).

### Fixed
- **Crash safety:** WAL replay now skips records whose timestamp is at or below the durable watermark of `data-*.dat`, eliminating a duplicate-frame hazard after a crash between data fsync and WAL truncate.
- **Replay robustness:** a WAL record whose timestamp would land before the current page's max is now handled by flushing the page and retrying, instead of aborting the entire replay and bricking the database.
- **Rollup durability:** destination database pages are now flushed before the rollup checkpoint is appended, closing a gap where a crash could leave a checkpoint claiming "interval done" with no data on disk.
- **Atomic writes:** manifest writes (`engine.toml`, `manifest.toml`, auto-created rollup destination manifest) now use tmp+fsync+rename+dir-fsync. Catalog, V1/V2 metric files, and rollup checkpoint compaction also now call `syncParentDir` after rename. Orphan `.tmp` files are removed on every rename-failure path.
- **Concurrency:** removed an ABBA deadlock hazard between the inspect/query path and the write path. Lock acquisition order is now documented on the `Engine` struct; the `writeLockHeld bool` flag-parameter was replaced with explicit `*Locked` method variants.
- **Compression bomb / OOM protection:** every on-disk frame allocation (page frames, V1 metric pages, V2 time-frames, V2 metric-frames) now rejects oversized `PayloadLen` / `DecodedLen` values before allocating the receive buffer.
- **WAL integrity:** WAL replay no longer silently defaults an unknown value-type byte to `int32` (which previously could misread float bits as int); resolution now requires the catalog and errors out if the metric id is missing.
- **Catalog integrity:** `catalog.json` is validated on load — rejects empty names, `id=0`, invalid `value_type`, duplicate names, and duplicate ids. Internal `idToName` map now keys on `MetricID` (uint16) instead of `int16`, eliminating a wrap hazard for metric IDs ≥ 32768.
- **V2 metric files:** fixed three sites where `uint32(TimeOffset + pointCount)` could silently wrap and return a truncated sample slice.
- **Sample-formatting helpers:** unified `engine.ValueTypeName`, `FormatSampleValue`, `FormatLPValue`, `ParseLPValue`. Online ingest (`parseLineProtocol`) and offline import/export now share the same definition of a valid LP sample.
- **Reader interface:** new `MetricFileReader` abstraction over V1/V2; every callsite that previously switched on the version byte (summary, frame walk, sample walk, single-metric and multi-metric collect, raw-vs-metric compare) now goes through one interface.
- **Query path:** `QueryRange` is now the n=1 special case of `QueryRangeMany`; the three-deep `os.IsNotExist` ladder is consolidated.
- Path traversal via `?db=../etc` on the engine UI is rejected (new database-name allowlist + `HasPrefix(rootDir)` guard).
- `Engine.ExportFile` now surfaces buffered-write/Sync failures from the output file instead of silently dropping them via `defer Close()`.
- LP import scanners (`engine.ImportFile`, `ImportOfflineLPToMetricParts`) raised from the 64 KiB default to a 1 MiB line cap; longer lines surface as errors rather than silently truncating.
- `Aggregator.Compute` interface dropped two unused `Timestamp` parameters; tests and call sites updated.

### Deferred
- HTTP authentication on the engine endpoints — tracked for a dedicated release.
- WAL per-record CRC32 — requires a WAL on-disk format-version bump; tracked separately.

## [1.4.0] - 2026-05-28

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

[Unreleased]: https://github.com/aymanhs/nanotdb/compare/v1.4.0...HEAD
[1.4.0]: https://github.com/aymanhs/nanotdb/compare/v1.3.0...v1.4.0
[1.3.0]: https://github.com/aymanhs/nanotdb/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/aymanhs/nanotdb/releases/tag/v1.2.0
[1.1.2]: https://github.com/aymanhs/nanotdb/releases/tag/v1.1.2
[1.1.1]: https://github.com/aymanhs/nanotdb/releases/tag/v1.1.1
[1.1.0]: https://github.com/aymanhs/nanotdb/releases/tag/v1.1.0
[1.0.1]: https://github.com/aymanhs/nanotdb/releases/tag/v1.0.1
[1.0.0]: https://github.com/aymanhs/nanotdb/releases/tag/v1.0.0
