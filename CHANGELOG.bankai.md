# Bankai fork changelog

Changes in bankaihq/sortie on top of the upstream release each version is based on. A version `X.Y.Z-bankai.N` contains upstream `vX.Y.Z` plus everything listed up to and including its entry; upstream's own changes are in [CHANGELOG.md](CHANGELOG.md).

The release workflow requires a heading `## [X.Y.Z-bankai.N] - YYYY-MM-DD` for the version it releases and uses that section as the release notes. See [RELEASING.bankai.md](RELEASING.bankai.md).

## [Unreleased]

### Added

- ClickUp tracker: set `tracker.kind: clickup` to work the tasks of one ClickUp list, with statuses as states, tags as labels, and task dependencies as blockers. `tracker.query_filter` narrows the list with ClickUp's list-tasks filters, and an optional `clickup.team_id` turns on custom task IDs.
