# M6 — CI and release publishing

GitHub Actions for Lambdary: test on push/PR, and on a semver tag build the
binaries and publish a GitHub release whose body is the CHANGELOG section for
that version — following the org's established conventions.

## Org conventions (verified in open-southeners/rcomp and laravel-apiable)

- Changelog → release notes via `release-flow/keep-a-changelog-action@v3`,
  `command: query`, `version: <semver>` → output `release-notes`.
- Release creation via `softprops/action-gh-release@v2` with
  `body: ${{ steps.changelog.outputs.release-notes }}`,
  `prerelease: ${{ contains(github.ref_name, '-') }}`,
  `make_latest: ${{ !contains(github.ref_name, '-') }}`.
- Tag triggers: `"v[0-9]+.[0-9]+.[0-9]+"` and `"v[0-9]+.[0-9]+.[0-9]+-*"`.
- `permissions: contents: write` on the release workflow; top-of-file comment
  block explaining trigger/steps/idempotency; NEVER interpolate `${{ }}`
  directly into `run:` scripts — pass via `env:` (injection hygiene).
- CI: separate small jobs (fmt, vet, test), `on: push/pull_request` to main.

## Facts

- Version injection: `-ldflags "-X github.com/open-southeners/lambdary/internal/cli.version=<ver>"`.
- Pure Go, `CGO_ENABLED=0` cross-compiles fine; process backend uses POSIX
  syscalls — Windows is out of scope by design (WSL uses linux binaries).
- Targets: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64.
- ubuntu-latest runners have Docker + node + python3 + git → the full test
  suite (including Docker-gated and process-backend e2e) can actually run.
- CHANGELOG.md currently has only `[Unreleased]` — a release REQUIRES a
  `[X.Y.Z]` section; the workflow must fail fast and clearly when missing.
- Validate workflows locally with
  `go run github.com/rhysd/actionlint/cmd/actionlint@latest`.

## Units

### Unit A — `.github/workflows/ci.yml`  *(parallel with B)*

Jobs (small, parallel, rcomp-style):
- `fmt`: `gofmt -l .` must output nothing (fail with the file list shown).
- `vet`: `go vet ./...`.
- `test`: ubuntu-latest, `actions/setup-go@v5` (go-version from go.mod,
  cache enabled), `go test ./... -count=1` — full suite; Docker/node/python3
  are present on the runner so integration + e2e run for real. Generous
  `timeout-minutes` (~20). A second `test-short` macos-latest job runs
  `go test ./... -short -count=1` for darwin coverage without Docker.
- Triggers: push to main + pull_request to main. Concurrency group cancels
  superseded runs on the same ref.
- Top-of-file comment block per org style.

### Unit B — `.github/workflows/release.yml` + RELEASING.md  *(parallel with A)*

release.yml, tag-triggered (org tag patterns), `permissions: contents:
write`, two jobs:
1. `release`: checkout; derive `VERSION=${TAG#v}`; **changelog guard** —
   `release-flow/keep-a-changelog-action@v3` `command: query` with that
   version (a missing section fails here, before anything is built; add an
   explicit friendly `::error::` step-summary hint referencing RELEASING.md
   on failure via `if: failure()`); create the GitHub release with
   softprops/action-gh-release@v2 (body = release-notes, prerelease/
   make_latest per org convention). Outputs: version + release id/upload
   info for job 2.
2. `binaries`: needs release; single ubuntu-latest job (pure-Go cross
   compile — no OS matrix needed): setup-go, then for each target pair
   (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64):
   `CGO_ENABLED=0 GOOS=… GOARCH=… go build -trimpath -ldflags "-s -w -X
   github.com/open-southeners/lambdary/internal/cli.version=${VERSION}" -o
   dist/lambdary ./cmd/lambdary`, then tar.gz as
   `lambdary_${VERSION}_${GOOS}_${GOARCH}.tar.gz` (binary + README.md +
   CHANGELOG.md), plus a `checksums.txt` (sha256 of every archive).
   Upload all assets to the release from job 1 (softprops action with
   `files:` + the existing tag, or gh CLI upload — pick the org-consistent
   softprops path). Version-vs-tag sanity: also assert
   `dist` binary `--version` output contains ${VERSION} for the host-arch
   build (linux/amd64 runs natively on the runner).
- RELEASING.md: the release process — move `[Unreleased]` entries to a new
  `[X.Y.Z] - YYYY-MM-DD` section (Keep a Changelog), commit, `git tag
  vX.Y.Z`, `git push origin vX.Y.Z`; what the workflow does; how to retry a
  failed release (delete release+tag or re-run); note binaries cover
  linux/darwin (Windows via WSL uses linux).
- CHANGELOG entry under [Unreleased]/Added: prebuilt `lambdary` binaries for
  Linux and macOS (amd64/arm64) attached to GitHub releases, with release
  notes generated from this changelog.

## Verification (both units)

`go run github.com/rhysd/actionlint/cmd/actionlint@latest` clean on the new
workflow files; YAML parse sanity; no `${{ }}` inside `run:` blocks (grep).
Real tag runs happen on GitHub — document that limitation honestly in the
final report.

## Still deferred

RIE release embedding (CURRENT_ISSUES) — this pipeline is the prerequisite;
a follow-up can add the per-target RIE build + go:embed wiring. Homebrew tap
(org has homebrew-tap) — natural follow-up once releases exist.
