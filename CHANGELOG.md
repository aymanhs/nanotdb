# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- Structured `slog` logging configuration via `[logging]` / `[[logging.logger]]`, plus file-only diagnostic logging controls for `nanocli` with `--log-file` and `--log-level`.
- Task-oriented onboarding docs with a copy/paste Hello World guide, a dedicated architecture page, a brief systemd service guide, and glossary-linked terminology references.

### Changed
- The main README now positions NanoTDB more clearly around edge and single-node use cases, WAL-backed recovery, SD-friendly storage, and fit-versus-tradeoff guidance instead of leading with internals.

### Fixed
- WAL reset now flushes eligible non-current open day pages during ingest, fixing a case where a stale pre-midnight page could block WAL truncation after midnight and let the active WAL grow far larger than expected.
- `nanocli inspect wal` now reports sane WAL start/duration timestamps for live current-era WAL files, avoiding misleading `1970-01-01` ranges in inspection output.

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

[Unreleased]: https://github.com/aymanhs/nanotdb/compare/v1.1.2...HEAD
[1.1.2]: https://github.com/aymanhs/nanotdb/releases/tag/v1.1.2
[1.1.1]: https://github.com/aymanhs/nanotdb/releases/tag/v1.1.1
[1.1.0]: https://github.com/aymanhs/nanotdb/releases/tag/v1.1.0
[1.0.1]: https://github.com/aymanhs/nanotdb/releases/tag/v1.0.1
[1.0.0]: https://github.com/aymanhs/nanotdb/releases/tag/v1.0.0
