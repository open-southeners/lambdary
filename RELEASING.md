# Releasing

How to cut a Lambdary release. The `Release` workflow
(`.github/workflows/release.yml`) does the rest once you push a tag.

## 1. Prepare the changelog

Move the relevant entries out of `## [Unreleased]` in `CHANGELOG.md` into a
new dated section, keeping an empty `[Unreleased]` at the top for whatever
comes next ([Keep a Changelog](https://keepachangelog.com/en/1.1.0/)):

```markdown
## [Unreleased]

## [X.Y.Z] - YYYY-MM-DD

### Added

- ...
```

The release workflow reads this `[X.Y.Z]` section as the GitHub release
body, so it **must** exist for the version you're about to tag — the
workflow fails fast if it's missing.

## 2. Commit and tag

```sh
git add CHANGELOG.md
git commit -m "Release vX.Y.Z"
git tag vX.Y.Z
git push origin main vX.Y.Z
```

Tags must match `vX.Y.Z` or `vX.Y.Z-<pre-release>` (e.g. `v1.2.3`,
`v1.2.3-rc.1`) to trigger the workflow.

## 3. What the workflow does

Pushing the tag triggers two jobs:

1. **`release`** — derives the version from the tag, reads the matching
   `[X.Y.Z]` section from `CHANGELOG.md`, and creates a GitHub release with
   that section as its body (marked as a pre-release, and not `latest`, if
   the tag has a `-` suffix).
2. **`binaries`** — cross-compiles `lambdary` for `linux/amd64`,
   `linux/arm64`, `darwin/amd64`, and `darwin/arm64`; packages each as
   `lambdary_X.Y.Z_<os>_<arch>.tar.gz` (binary + `README.md` +
   `CHANGELOG.md`); generates `checksums.txt` (SHA-256 of each archive);
   and attaches all of it to the release from step 1. Each binary also
   embeds a per-target copy of AWS's Lambda Runtime Interface Emulator
   (built from the pinned tag in `internal/rie/rie.go`'s `Version`
   constant), so the process backend works on a fresh install with no
   `git`/`go` on the end user's machine — `go install` builds don't have
   that embedded copy and fall back to building the emulator from source
   on first use, same as before.

## Failure modes

**Missing changelog section.** The `release` job fails before creating
anything. Fix the changelog, then delete the tag and any release/draft it
may have created before re-tagging:

```sh
git tag -d vX.Y.Z
git push origin :refs/tags/vX.Y.Z
gh release delete vX.Y.Z --yes   # only if a release was actually created

# ...fix CHANGELOG.md, commit...

git tag vX.Y.Z
git push origin main vX.Y.Z
```

**Partial asset upload** (e.g. the `binaries` job fails or is interrupted
after the release already exists). Just re-run the failed job from the
Actions UI ("Re-run failed jobs") — no need to delete or re-tag anything.
`softprops/action-gh-release` matches the existing release by tag and
uploads into it rather than creating a duplicate, and any asset re-uploaded
under the same filename replaces the previous one, so a re-run is safe and
idempotent.

## Installing a release

Prebuilt binaries cover Linux and macOS (amd64 + arm64) — that's every
supported target; Windows users run the Linux binary under WSL, same as the
rest of the project. Download the archive for your platform and checksum
from the release page, or skip binaries entirely:

```sh
go install github.com/open-southeners/lambdary/cmd/lambdary@latest
```
