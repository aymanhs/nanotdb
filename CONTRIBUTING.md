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
- `release/1.2`: safe maintenance line for the `1.2.x` series.
- `release/2.0.0`: stabilization line for the upcoming `2.0.0` release once scope is frozen.

Branch intent:
- `release/1.2` accepts only low-risk fixes, docs fixes, packaging fixes, and critical regressions.
- `main` is the active integration branch for the next release until feature scope is frozen.
- `release/2.0.0` is not the day-to-day merge target; it is where `2.0.0` is stabilized before release.
- New feature work should go to `main` or a `feature/...` branch and then be included in the release branch when that branch is refreshed or cut from `main`.

## Rules

- Do not develop new features directly on a maintenance branch.
- Do not merge `main` wholesale into an older release branch.
- Fixes for older supported releases must be cherry-picked.
- Ship releases from `release/X.Y` branches, not from a local feature branch.
- Cut a release branch before final stabilization if the upcoming release is large or risky.

## Release Flow

### Patch release from a stable line

Example: `1.2.1`

1. Branch or work on `release/1.2`.
2. Apply only the targeted fix set.
3. Validate the release branch.
4. Tag the release: `v1.2.1`.
5. Cherry-pick important fixes forward into `main` and any newer release branch.

### Major or minor release

Example: `2.0.0`

1. Merge finished feature branches into `main`.
2. Run integration and regression testing primarily on `main` while feature work is still settling.
3. When scope is frozen, cut or fast-forward `release/2.0.0` from `main`.
4. Allow only bug fixes, docs, packaging, and release preparation on that branch.
5. Optionally tag one or more release candidates such as `v2.0.0-rc1`.
6. Tag the final release from `release/2.0.0` as `v2.0.0`.
7. Keep the release branch for `2.0.x` follow-up fixes if needed.

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

For the current transition:
- `v1.2.0` remains the immutable anchor for the pre-metrics release line
- `release/1.2` is the safe line for any `1.2.x` follow-up fixes
- `main` is the active merge and regression line for metrics, dashboard, and related `2.0` integration work
- `release/2.0.0` is kept ready for final freeze, release-candidate work, and `2.0.x` stabilization
