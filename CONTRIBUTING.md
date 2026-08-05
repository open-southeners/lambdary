# Contributing guidelines

Thank you very much for your contributions!

Remember that we are an open source organisation that will always accept any
external contribution and/or help (if it follows these guidelines).

## Table of Contents

- [How to Contribute](#how-to-contribute)
- [Development Workflow](#development-workflow)
- [Git Guidelines](#git-guidelines)
- [Release Process (internal team only)](#release-process-internal-team-only)

## How to Contribute

1. We must first see the idea you had behind, **if you found
   [already an issue](https://github.com/open-southeners/lambdary/issues) go
   ahead**, otherwise **communicate first** via
   [GitHub issue](https://github.com/open-southeners/lambdary/issues/new) or
   [Discord](https://discord.gg/tyMUxvMnvh).
2. Once approved the idea (and opened the issue),
   [fork this repository](https://github.com/open-southeners/lambdary/fork).
3. Read and make sure that the [Development Workflow](#development-workflow)
   is applied properly.
4. [Submit the branch as a Pull Request](https://help.github.com/en/github/collaborating-with-issues-and-pull-requests/creating-a-pull-request-from-a-fork)
   pointing to the `main` branch of this repository.
5. All done! Now wait until we review the changes of your Pull Request.

## Development Workflow

Lambdary is a plain Go project — no code generation:

```sh
go build ./...           # build everything, no binary output (compile check)
go test ./... -short     # fast suite (no Docker / no network)
go test ./...            # full suite — see below
```

To get a runnable `lambdary` binary for your own platform (what
`go build ./...` above doesn't produce, since `cmd/lambdary` is one package
among several under `./...`), use `scripts/build.sh`. It mirrors what the
release workflow does for each of its four cross-compiled targets
(`.github/workflows/release.yml`'s `binaries` job) but only for your current
`GOOS`/`GOARCH`, and skips packaging a tarball:

```sh
scripts/build.sh                        # dist/lambdary, with embedded RIE
LAMBDARY_SKIP_RIE_EMBED=1 scripts/build.sh   # skip the RIE build/embed step
```

The default run builds AWS's Runtime Interface Emulator from source and
embeds it into the binary (needs `git` + `go`, network, ~30s on first run;
cached at `internal/rie/embedded/aws-lambda-rie` and reused by subsequent
runs — delete it to rebuild for a different platform). Pass
`LAMBDARY_SKIP_RIE_EMBED=1` for a faster build that behaves like a plain
`go install`: the process backend falls back to building the emulator from
source on first use instead of finding it pre-embedded.

`DESIGN.md` documents the architecture and is the source of truth for how
the pieces fit together; `plans/` holds the per-milestone implementation
plans; `CURRENT_ISSUES.md` tracks known gaps and deferred work — check it
before opening an issue about a missing feature.

### Code style

Standard Go tooling only: code must be `gofmt`-clean and `go vet`-clean (CI
enforces both). Prefer the standard library — new dependencies need a good
reason.

### Testing

**All tests must pass** and we might consider asking for some more tests if
the contribution requires it.

The full suite includes integration tests that are skipped automatically
when their requirements are missing, so `go test ./...` adapts to your
machine:

- Container-backend and e2e tests need a running Docker daemon (they pull
  `public.ecr.aws/lambda/*` images on first run).
- Process-backend and hot-reload e2e tests need `node` and `python3` on your
  `PATH`, and build AWS's Runtime Interface Emulator from source on first
  run (needs `git` + `go`, network; cached under `~/.lambdary/bin`
  afterwards).
- `go test ./... -short` skips all of the above.

**Any additional test adding more coverage will be more than welcome!**

### Changelog

User-visible changes belong in `CHANGELOG.md` under `## [Unreleased]`,
following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/): one
bullet per change, written for the person using the release — not for
whoever wrote the diff.

## Git Guidelines

### Using branches

**We do not enforce this**, but it's recommended. Otherwise **make sure you
are contributing from your own forked** version of this repository.

We do not enforce any branch naming style, but please use something
descriptive of your changes.

### Descriptive commit messages

We do not enforce any rule (commitlint) or anything on this repository.

But being descriptive in the commit messages **is a must**.

## Release Process (internal team only)

This is only for us, you should not perform nor take care of any of this.

The `Release` workflow (`.github/workflows/release.yml`) does the heavy
lifting once a version tag is pushed.

### 1. Prepare the changelog

Move the relevant entries out of `## [Unreleased]` in `CHANGELOG.md` into a
new dated section, keeping an empty `[Unreleased]` at the top for whatever
comes next:

```markdown
## [Unreleased]

## [X.Y.Z] - YYYY-MM-DD

### Added

- ...
```

The release workflow reads this `[X.Y.Z]` section as the GitHub release
body, so it **must** exist for the version you're about to tag — the
workflow fails fast if it's missing.

### 2. Commit and tag

```sh
git add CHANGELOG.md
git commit -m "Release vX.Y.Z"
git tag vX.Y.Z
git push origin main vX.Y.Z
```

Tags must match `vX.Y.Z` or `vX.Y.Z-<pre-release>` (e.g. `v1.2.3`,
`v1.2.3-rc.1`) to trigger the workflow.

### 3. What the workflow does

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

### Failure modes

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

### Installing a release

Prebuilt binaries cover Linux and macOS (amd64 + arm64) — that's every
supported target; Windows users run the Linux binary under WSL, same as the
rest of the project. Download the archive for your platform and checksum
from the release page, or skip binaries entirely:

```sh
go install github.com/open-southeners/lambdary/cmd/lambdary@latest
```
