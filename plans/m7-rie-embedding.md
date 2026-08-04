# M7 — Embed the RIE in release binaries

Closes the "RIE binary not embedded in releases" deferral: release builds of
`lambdary` carry the AWS Runtime Interface Emulator (pinned v1.35) inside the
binary, so the process backend needs no `git`/`go` on end-user machines.
Dev builds (`go install`, plain `go build`) keep today's behavior.

## Design

- **Build-tag-gated `go:embed`.** `go:embed` needs the file present at
  compile time, which dev builds won't have:
  - `internal/rie/embedded_stub.go` (no build tag... inverse tag
    `!embedrie`): `var embeddedRIE []byte` empty.
  - `internal/rie/embedded.go` (`//go:build embedrie`):
    `//go:embed embedded/aws-lambda-rie` into `embeddedRIE`.
  - `internal/rie/embedded/` is gitignored (build artifact) with a
    `.gitkeep`-style README explaining what lands there in CI. NOTE: go:embed
    fails on a missing file — acceptable because the path only compiles
    under the `embedrie` tag, which only CI sets after placing the file.
- **Acquisition chain becomes:** env override → cache → **embedded extract**
  → build from source. Embedded extract writes `embeddedRIE` to the standard
  cache path (0755, atomic temp+rename) and returns it; subsequent runs hit
  the cache step. Chain order preserves all existing behavior when
  `embeddedRIE` is empty.
- **Release workflow:** in the `binaries` job, before the target loop:
  shallow-clone the RIE repo at the pinned tag once. Inside the loop, per
  target: `CGO_ENABLED=0 GOOS/GOARCH GOTOOLCHAIN=auto go build` the RIE
  `./cmd/aws-lambda-rie` into `internal/rie/embedded/aws-lambda-rie`, then
  build `lambdary` with `-tags embedrie` (plus existing flags). Sanity: the
  native linux/amd64 `lambdary --version` check stays; additionally assert
  each embedded RIE file is non-empty before the lambdary build, and clean
  `internal/rie/embedded/` between targets so a stale binary can never leak
  into the wrong target.
- **Version coupling:** the embedded binary is always `rie.Version`; the
  clone uses that constant's value — keep the workflow's pinned tag in ONE
  place (read it from the Go source or define it in the workflow with a
  comment pointing at `internal/rie/rie.go`; prefer extracting via
  `go run`/`grep` from source so bumping `rie.Version` is the single source
  of truth).

## Unit (single implementer)

- Files: `internal/rie/embedded.go`, `internal/rie/embedded_stub.go`,
  `internal/rie/rie.go` (chain insertion + extraction func),
  `internal/rie/rie_test.go` (extend), `.gitignore`
  (`internal/rie/embedded/`), `.github/workflows/release.yml` (binaries
  job), `RELEASING.md` (mention embedded RIE + what changes for users),
  `CHANGELOG.md` (Added entry), `DESIGN.md` decisions-log RIE line +
  CURRENT_ISSUES already updated by the orchestrator (verify no stale
  reference remains).
- Tests: extraction path is testable without the build tag — `embeddedRIE`
  is a package var, so a test can set it (t.Cleanup restore), call Resolve
  with a temp `LAMBDARY_HOME`, and assert: file lands in cache with exec
  bits, content matches, second call hits cache, and the runner is never
  invoked (no git/go probing when embedded satisfies the chain). Also assert
  precedence: env override still wins over embedded; cache still wins over
  embedded.
- Workflow verification: actionlint clean; local dry-run of the new loop
  semantics (clone RIE, cross-build for two targets incl. one non-host,
  place file, `go build -tags embedrie ./...` succeeds and the resulting
  binary's rie package sees a non-empty embedded payload — verifiable via a
  tiny `go test -tags embedrie` run asserting `len(embeddedRIE) > 0`).
