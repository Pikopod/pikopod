# Releasing pikopod

Releases are tag-driven. The release workflow runs for tags matching `v*`; it
does not run for ordinary branch pushes or GitHub releases created by hand.

## Before tagging

Run the normal contribution checks from a clean checkout of the commit you
intend to release:

```bash
go test ./... -race -count=1
go build ./...
go vet ./...
test -z "$(gofmt -l .)"
git diff --check
```

Also check the following before creating the tag:

- The version is the next intended pre-v1 SemVer version and the commit is on
  `main`.
- `go.mod` is unchanged by an accidental local dependency update.
- `CHANGELOG.md`, release notes, and upgrade notes describe user-visible
  changes.
- The repository's `HOMEBREW_TAP_GITHUB_TOKEN` secret is present and has
  `contents: write` access to `pikopod/homebrew-tap`.
- The release commit is the one you want to publish; the workflow checks out
  the tag with full history and embeds the tag version, commit, and build date
  into the binary.

The repository is pre-v1. Use `v0.MINOR.PATCH` tags. Increment PATCH for a
backwards-compatible fix, MINOR for a new feature or other intentional
contract change, and do not imply v1-level compatibility before the project
declares v1. Exit codes are a public API; the drift-event schema is also a
public API, with structural changes tracked by `schema_version`. See
[`docs/exit-codes.md`](docs/exit-codes.md) for the current contracts.

## Create and push a release tag

From the release commit, create an annotated tag and push it:

```bash
VERSION=v0.1.2
git tag -a "$VERSION" -m "pikopod $VERSION"
git push origin "$VERSION"
```

The push starts `.github/workflows/release.yml`. Watch the workflow in the
Actions tab, or with:

```bash
gh run list --workflow release.yml --limit 5
gh run watch RUN_ID --exit-status
```

Replace `RUN_ID` with the run associated with the tag. A successful run creates
the GitHub release and its assets. The `provenance` job starts only after the
GoReleaser job has emitted the checksum subject.

## What the workflow publishes

The GoReleaser job performs these steps:

1. Checks out the tagged commit and installs the Go version declared by
   `go.mod`.
2. Installs `cosign` and Syft, then logs into GHCR with the workflow's
   `GITHUB_TOKEN`.
3. Builds static (`CGO_ENABLED=0`) binaries for Linux, macOS, and Windows on
   amd64 and arm64.
4. Packages each platform build as `.tar.gz`, or `.zip` for Windows.
5. Builds Debian and RPM packages.
6. Writes `SHA256SUMS` for the release artifacts.
7. Generates an SBOM for every archive.
8. Creates a keyless cosign signature and certificate for `SHA256SUMS`.
9. Publishes a Homebrew formula to `pikopod/homebrew-tap` under `Formula/`.
10. Publishes versioned and `latest` container images to GHCR.

The separate SLSA generator job uploads provenance for the checksum subjects.
The release is not complete until both the GoReleaser and provenance jobs have
succeeded.

## Secrets and permissions

The workflow uses the automatic `GITHUB_TOKEN` for assets in
`Pikopod/pikopod`, GHCR authentication, and release operations. It cannot push
to the separate `pikopod/homebrew-tap` repository.

`HOMEBREW_TAP_GITHUB_TOKEN` is therefore required. It must be a fine-grained
GitHub personal access token scoped to `pikopod/homebrew-tap` with
`Contents: Read and write`. Store the token only as the repository Actions
secret of the same name. Never put its value in this file, a shell history, a
commit, or a release note.

To rotate it, create a replacement token with the same narrow repository and
permission scope, update the repository secret, run a release that publishes
the formula, then revoke the old token. If the token is exposed, revoke it
immediately and inspect the tap repository's audit history before retrying.

The workflow also requests `id-token: write` so GitHub Actions can produce the
keyless cosign certificate and SLSA provenance. Do not replace those identity
permissions with a long-lived signing key unless the release design is
changed and reviewed separately.

## Verify a downloaded release

Download the archive, `SHA256SUMS`, `SHA256SUMS.sig`, and `SHA256SUMS.pem` from
the GitHub release. In the directory containing those files, verify the
checksum and then the keyless signature:

```bash
sha256sum -c SHA256SUMS --ignore-missing
```

On macOS without GNU coreutils, use:

```bash
shasum -a 256 -c SHA256SUMS
```

Verify that the checksum file was signed by this repository's tagged release
workflow. Confirm that the certificate identity refers to the capitalised
`Pikopod` organisation, this repository's release workflow, and a tag ref; the
canonical verification command is maintained in
[`docs/security.md`](docs/security.md#verifying-a-release).

## If the workflow fails

First inspect the failed job and the GitHub release page. Do not immediately
move a tag: a release may already have uploaded some assets, and consumers may
have downloaded them.

### Failure before a release exists

If the run failed before creating a GitHub release and no assets were published:

1. Fix the failure on a new commit, or confirm that the tagged commit itself
   is correct and only the workflow failed.
2. Delete the failed tag locally and remotely.
3. Recreate the same tag on the exact commit being released.
4. Push it once and watch the new run from start to finish.

```bash
VERSION=v0.1.2
git tag -d "$VERSION"
git push origin ":refs/tags/$VERSION"
git tag -a "$VERSION" -m "pikopod $VERSION"
git push origin "$VERSION"
```

Deleting and recreating a tag is appropriate only for a failed, unpublished
release. Never silently move a tag after a successful release; publish a new
patch version instead.

### A release or assets already exist

Stop and inspect before deleting anything. If a release was created, determine
whether any asset was downloaded and whether the failure is limited to the
provenance job or Homebrew publication. A maintainer should decide whether to
rerun the failed job, repair the tap, or replace the release. Do not overwrite
published artifacts under an existing version.

If the GoReleaser job succeeded but provenance failed, the binaries and
checksums may be usable, but the release is not fully verified until provenance
has been repaired and checked. If the Homebrew push failed, rotate or repair
the tap credential as needed and rerun only after confirming that rerunning will
not create conflicting formula history.

## After a successful release

Confirm all of the following from the tag, Actions run, release page, GHCR, and
Homebrew tap:

- Both workflow jobs succeeded.
- The release assets match the tagged version and include checksums, SBOMs,
  and cosign signature material.
- The checksum and cosign commands above succeed for one downloaded archive.
- The Homebrew formula points at the new archive checksums.
- The GHCR versioned image exists, and `latest` points at the intended release.
- The release notes explain any exit-code or event-schema implications.
