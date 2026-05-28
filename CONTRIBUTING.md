# Contributing

## Branch Model

NanoTDB uses a simple release-branch workflow.

Branches:
- `main`: next development line for the next planned release.
- `release/X.Y`: stable maintenance branch for a released minor line.
- `feature/...`: short-lived work branches that merge into `main`.

Tags:
- `vX.Y.Z` tags are immutable release points.
- Examples: `v1.2.0`, `v2.0.0-rc1`, `v2.0.0`.

## Current Release Policy

Current release lines:
- `release/1.3`: safe maintenance line for the `1.3.x` series.
- `main`: active integration branch for the next planned minor release.

Branch intent:
- `release/1.3` accepts only low-risk fixes, docs fixes, packaging fixes, and critical regressions.
- `main` is the active integration branch for the next release until feature scope is frozen.
- When the next minor release is frozen, cut a `release/X.Y` branch from `main` and allow only stabilization work there.
- New feature work should go to `main` or a `feature/...` branch and then be included in the release branch when that branch is refreshed or cut from `main`.

## Rules

- Do not develop new features directly on a maintenance branch.
- Do not merge `main` wholesale into an older release branch.
- Fixes for older supported releases must be cherry-picked.
- Ship releases from `release/X.Y` branches, not from a local feature branch.
- Cut a release branch before final stabilization if the upcoming release is large or risky.

## Release Flow

### Patch release from a stable line

Example: `1.3.1`

1. Branch or work on `release/1.3`.
2. Apply only the targeted fix set.
3. Validate the release branch.
4. Tag the release: `v1.3.1`.
5. Cherry-pick important fixes forward into `main` and any newer release branch.

### Major or minor release

Example: `1.4.0`

1. Merge finished feature branches into `main`.
2. Run integration and regression testing primarily on `main` while feature work is still settling.
3. When scope is frozen, cut or fast-forward `release/1.4` from `main`.
4. Allow only bug fixes, docs, packaging, and release preparation on that branch.
5. Optionally tag one or more release candidates such as `v1.4.0-rc1`.
6. Tag the final release from `release/1.4` as `v1.4.0`.
7. Keep the release branch for `1.4.x` follow-up fixes if needed.

## Cherry-Pick Policy

Use cherry-picks for fixes that must land on multiple supported branches.

Typical direction:
- old stable fix -> cherry-pick forward into newer lines
- never merge a newer unstable branch back into an older stable line

## Operator Safety

To keep a release line safe:
- production deployments should come from a release branch or release tag
- do not deploy from a long-lived feature branch
- treat tags as shipped history and release branches as supported code lines

## Notes For This Repo

For the current release cycle:
- `v1.3.0` is the latest shipped minor release anchor
- `release/1.3` is the safe line for any `1.3.x` follow-up fixes
- `main` is the active merge and regression line for the next minor release
- cut `release/1.4` from `main` when feature scope is frozen and move to stabilization there
