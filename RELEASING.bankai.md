# Releasing the Bankai fork

bankaihq/sortie is a long-lived fork of [sortie-ai/sortie](https://github.com/sortie-ai/sortie) that carries Bankai's own changes, such as the ClickUp tracker. This file covers the fork's releases. Upstream's [RELEASING.md](RELEASING.md) covers theirs; upstream's release workflow is disabled here.

## Branches and versions

- `main` mirrors upstream's `main` exactly. Nothing of ours is committed to it.
- `bankai` is the default branch. It is an upstream release tag plus our commits, and it takes each new upstream release as a merge. Its history is never rewritten.
- A fork version is `X.Y.Z-bankai.N`: upstream `vX.Y.Z` plus our changes. `N` starts at 1 for each new upstream base and goes up for every further fork release on the same base.
- Our changes are listed in [CHANGELOG.bankai.md](CHANGELOG.bankai.md). The fork's release files are [release-bankai.yml](.github/workflows/release-bankai.yml) and [.goreleaser.bankai.yaml](.goreleaser.bankai.yaml). Upstream's `CHANGELOG.md`, `release.yml` and `.goreleaser.yaml` are never edited here, so upstream merges don't conflict in them.

## Taking a new upstream release

Do this when upstream publishes a release (Watch → Custom → Releases on sortie-ai/sortie gets you a notification). `vX.Y.Z` is the new upstream tag.

```sh
git fetch upstream --tags
git push origin upstream/main:main      # keep the mirror current
git push origin vX.Y.Z                  # the release workflow checks the base tag exists here

git switch -c sync/vX.Y.Z origin/bankai
git merge vX.Y.Z
# resolve conflicts, then:
go build ./... && go test ./...
git push -u origin sync/vX.Y.Z
gh pr create --repo bankaihq/sortie --base bankai --title "Merge upstream vX.Y.Z"
```

Merge upstream's release tag, never its `main`. Commits between upstream releases are unreleased, and a `-bankai.N` version claims to contain exactly `vX.Y.Z`. The release workflow refuses a version whose base doesn't match the newest upstream tag on `bankai`.

When upstream changes an interface our code implements (for example `domain.TrackerAdapter`), the merge builds but the tests fail. Fix our code in the same PR.

## Cutting a release

1. On `bankai` (through a PR), move the entries under `## [Unreleased]` in `CHANGELOG.bankai.md` into a new section `## [X.Y.Z-bankai.N] - YYYY-MM-DD`. That section becomes the release notes. A release with no fork changes since the last one still needs a section; say which upstream release it takes.
2. Optionally do a dry run first, which builds everything but pushes no tag, release or image:
   ```sh
   gh workflow run release-bankai.yml --repo bankaihq/sortie --ref bankai -f version=X.Y.Z-bankai.N -f dry_run=true
   ```
3. Run it for real:
   ```sh
   gh workflow run release-bankai.yml --repo bankaihq/sortie --ref bankai -f version=X.Y.Z-bankai.N
   ```
4. Once the checks pass, the run waits for approval on the `release` environment. Approve it in the run's page on GitHub.

The workflow runs lint for three platforms, the unit tests with the race detector, the Gitea and GitLab integration tests (which bring their own containers), and the live ClickUp test. It then tags `vX.Y.Z-bankai.N` and publishes:

- A GitHub release with archives for linux/amd64, linux/arm64, darwin/arm64 and windows/amd64, `checksums.txt`, SBOMs and build-provenance attestations. Binaries are not code-signed.
- `ghcr.io/bankaihq/sortie:X.Y.Z-bankai.N` for linux/amd64 and linux/arm64. There is no `latest` tag.

## Installing and rolling out

Servers pin an exact version, and every pin moves together once a release is out.

```sh
V=1.32.0-bankai.1
gh release download "v$V" --repo bankaihq/sortie --pattern "sortie_${V}_linux_arm64.tar.gz" --pattern checksums.txt
sha256sum --check --ignore-missing checksums.txt
gh attestation verify "sortie_${V}_linux_arm64.tar.gz" --repo bankaihq/sortie
```

The repository is public, so plain `curl` from `https://github.com/bankaihq/sortie/releases/download/v$V/...` works as well. In a Dockerfile:

```dockerfile
COPY --from=ghcr.io/bankaihq/sortie:1.32.0-bankai.1 /usr/bin/sortie /usr/bin/sortie
```

On macOS a binary downloaded with a browser is quarantined; remove the flag with `xattr -d com.apple.quarantine sortie`. Windows SmartScreen asks once.

**Rollback** is pinning the previous version again. Never delete a release, a tag or an image: servers may still pin it.

**Hotfix**: fix it on `bankai`, add a section for `X.Y.Z-bankai.N+1`, and release.

## Local builds

```sh
make build VERSION=1.32.0-bankai.1-dev                         # ./sortie for this machine
GOOS=linux GOARCH=amd64 make build                             # another platform
BASE_VERSION=1.32.0 goreleaser release --snapshot --clean --config .goreleaser.bankai.yaml   # all archives in dist/, nothing published
```

## One-time setup

Already in place:

- The `release` environment, which requires an approval and accepts only the `bankai` branch.
- Upstream's other workflows (release, nightly, docs, price table, Dependabot sweeps, community notifications, PR assignment, protocol pin) are disabled in the fork's Actions settings. CI still runs on pull requests.

To do once:

- **ClickUp test list.** Create a "Sortie CI" list in Bankai's ClickUp, with at least one task in an active status (`to do` by default). Create a ClickUp user just for CI, give it access to that list only, and take its personal token (`pk_...`). Then set, under Settings → Secrets and variables → Actions:
  - secret `SORTIE_CLICKUP_API_KEY`: the CI user's token
  - variable `SORTIE_CLICKUP_LIST`: the list ID
  - variable `SORTIE_CLICKUP_WRITE_TASK`: the ID of a task the comment test may post on
  - optional variables `SORTIE_CLICKUP_ACTIVE_STATES` (comma separated) and `SORTIE_CLICKUP_TEAM_ID`
- **Image visibility.** After the first release, check that the `sortie` package under the bankaihq organization's Packages is public. GitHub can create a new package as private.
- **More approvers.** Add other people as required reviewers on the `release` environment when they join.
