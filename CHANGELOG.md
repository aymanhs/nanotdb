# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- (none yet)

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

[Unreleased]: https://github.com/aymanhs/nanotdb/compare/v1.0.1...HEAD
[1.0.1]: https://github.com/aymanhs/nanotdb/releases/tag/v1.0.1
[1.0.0]: https://github.com/aymanhs/nanotdb/releases/tag/v1.0.0
