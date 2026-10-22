---
type: Guide
title: "Toolchain pin bumps"
description: "The weekly job that proposes newer releases of the image's release-fetched tools, the script behind it, which pins stay manual, and what the bot needs."
status: stable
tags: [contributing, packaging, ci, dependencies, supply-chain]
---

# Toolchain pin bumps

Every Monday the `pin-bump` workflow (`.github/workflows/pin-bump.yml`) checks each tool
`packaging/Containerfile` downloads from a GitHub release, and opens one pull request per
tool that has a newer stable release. Each tool runs as its own job, so a broken upstream
release fails that tool's job and not the others. CI runs on it like on any other pull request, and it
lands only when someone merges it.

## What a bump changes

`packaging/pinbump` does the work. For every tool the Containerfile fetches through its
checked download, it:

1. picks the newest release by version, skipping drafts, prereleases and any tag that is
   not a plain `x.y.z`
2. downloads the release's checksum file over HTTPS and takes the amd64 and arm64 sha256
   from it, failing when the digest GitHub records for an asset or for the checksum file
   itself disagrees. For age, whose releases carry no checksum file, it takes the digests
   GitHub records for the assets
3. rewrites the `ARG <TOOL>_VERSION` line, the tool's two lines in
   `packaging/tool-digests.txt`, its field in `packaging/MANIFEST.json`, and its row in the
   [bill of materials](../sbom/bill-of-materials.md)

It exits non-zero and leaves the files as they were when those four files disagree before
or after the rewrite, when a release lacks an asset for either architecture, or when a
checksum is missing or malformed. A run with every pin current changes nothing.

The pull request is titled `build(image): bump <tool> to <version>` and comes from the
branch `bot/bump-<tool>`. Its body names the old and new version, links the release, and
says where the digests came from.

CI on a bump builds the amd64 image (`make image`), which checks the new amd64 digest, and
runs the static packaging tests. The arm64 digest is first downloaded and checked by
`publish.yml` after the merge.

## Running it locally

```sh
go run ./packaging/pinbump -dry-run       # print the diff, write nothing
go run ./packaging/pinbump                # apply every pending bump
go run ./packaging/pinbump -tool restic   # apply one tool's bump
go run ./packaging/pinbump -plan          # list pending bumps as JSON
```

With `GITHUB_TOKEN` set, the API reads are authenticated and the anonymous rate limit does
not apply. The tests in `packaging/pinbump` run offline against fixtures, and one of them
fails `make check` when the committed pins disagree.

## Automated and manual pins

| Pin | Digest source |
|---|---|
| tofu | the release's `tofu_<version>_SHA256SUMS` |
| restic | the release's `SHA256SUMS` |
| sops | the release's `sops-v<version>.checksums.txt` |
| nono | the release's `SHA256SUMS.txt` |
| litestream | the release's `checksums.txt` |
| age | the digest GitHub records for each asset, as the release publishes no checksum file |

These stay manual:

- **Python, ansible-core and ansible-runner.** A move means regenerating
  `packaging/toolchain-constraints.txt` with `uv pip compile`, and an ansible-core release
  can change what controllers and managed hosts need. The opt-in image test and CI's race
  job also name the ansible-core version. That is a review, not a substitution.
- **uv.** `UV_IMAGE` is pinned by registry index digest, not by a release checksum file,
  and the uv version decides which Python builds `uv python install` can fetch.
- **The Go and Fedora base images.** `GO_IMAGE` and `FEDORA_IMAGE` follow a release line
  (`golang:1.26`, `fedora-minimal:44`), and moving the line is a decision. git, zstd and
  iputils come from the Fedora release.
- **The test tools in `ci.yml`.** The race job installs restic, sops and age at versions
  written in the workflow. Pushing a change to a workflow file takes the app's `workflows`
  permission, which the job does not request.

## What the bot needs

A pull request opened with the default `GITHUB_TOKEN` starts no workflow, so CI would not
run on it. The job instead mints a short-lived installation token from the GitHub App
behind the `RELEASE_APP_ID` and `RELEASE_APP_PRIVATE_KEY` repository secrets, the app
`release.yml` also uses. The token is requested with exactly two permissions, and the app
needs both on this repository:

- read and write access to contents, to push the `bot/bump-*` branches
- read and write access to pull requests, to open and edit them

The release lookups use the run's own read-only `GITHUB_TOKEN`, and the app token is only
in the environment of the step that pushes and opens pull requests.

## Branches and pull requests

- A `bot/bump-<tool>` branch that proposes the current newest version and still merges
  cleanly into `main` is left as it is, so its CI does not re-run every week. When a newer
  version appears or the branch conflicts with `main`, the job rebuilds it from `main` and
  force-pushes it with a lease, which updates the open pull request in place.
- A branch carrying a commit from anyone but the bot is left alone, with a warning in the
  run.
- A version whose pull request was closed without merging is not proposed again. A later
  version is. Deleting a `bot/bump-*` branch by hand closes its pull request, which counts
  as declining that version.
- Only the app's own pull requests from this repository count. A fork's pull request from a
  branch of the same name is ignored.
- If `main` has changed a workflow file since the branch was made, GitHub can refuse the
  rebuilt push, since the token has no `workflows` permission. That tool's job then fails.
  Updating the branch from the pull request's page brings it up to date, and from then on
  the job leaves that branch to whoever updated it.
- The job never merges, approves or closes a pull request.

## Turning it off

`gh workflow disable pin-bump` stops the schedule, as does disabling it in the repository's
Actions tab, and `gh workflow enable pin-bump` brings it back. Deleting
`.github/workflows/pin-bump.yml` removes it. `gh workflow run pin-bump` runs it on demand
against `main`.
