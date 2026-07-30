# M0 — Skeleton + RIE darwin spike

Milestone M0 from `DESIGN.md`: Go CLI scaffold, function discovery, manifest
parsing, `lambdary list`, and the required spike proving the AWS Lambda
Runtime Interface Emulator (RIE) builds and runs on darwin/arm64 from pinned
upstream source.

Environment facts (verified): host is darwin/arm64 with go1.25.5 (upstream
RIE requires go 1.25.7 — rely on `GOTOOLCHAIN=auto`). Latest upstream RIE tag:
**v1.35** (the pin). Module path: `github.com/open-southeners/lambdary`
(GitHub org `open-southeners` confirmed).

## Conventions (all units)

- Go stdlib-first; allowed deps for M0: `spf13/cobra`, `gopkg.in/yaml.v3`.
- Package layout per DESIGN.md "Proposed repo layout".
- Table-driven tests, `testdata/` fixtures for filesystem cases.
- Errors wrapped with `%w`; user-facing messages name the remedy.
- Verify with `go build ./... && go vet ./... && go test ./...` from repo root.
- Report anything out of scope — do not fix it.

## Units

### Unit A — Module + CLI scaffold  *(sequential: first)*

- Files: `go.mod`, `go.sum`, `cmd/lambdary/main.go`,
  `internal/cli/root.go`, `internal/cli/list.go` (stub), `.gitignore`.
- `go mod init github.com/open-southeners/lambdary`; Go directive `1.25`.
- Cobra root command `lambdary` with `--version` (version var set via
  ldflags, default `dev`), persistent `--root` flag (default `.`) for the
  functions root folder, and a stub `list` subcommand that prints nothing yet
  (returns nil) so later units wire it.
- `main.go` stays thin: calls `cli.Execute()`, exits non-zero on error.
- Verify: `go build ./... && go vet ./...`; `go run ./cmd/lambdary --version`.

### Unit B — Manifest package  *(sequential: after A)*

- Files: `internal/manifest/manifest.go`, `internal/manifest/config.go`,
  `internal/manifest/manifest_test.go`, plus `testdata/` fixtures.
- Implement the `.lambda.yml` v0 spec exactly as written in DESIGN.md
  ("`.lambda.yml` spec (v0)" section): `name`, `runtime`, `handler`,
  `timeout` (seconds, int), `memory` (MB, int), `architectures` (list),
  `environment` (map), `url.path`, `url.payload`, and the `local` section
  (`backend` auto|container|process, `image`, `command`, `env_file`).
- `Load(path string) (*Manifest, error)` — parse + validate. Validation:
  unknown `local.backend` values are errors; `timeout`/`memory` must be > 0
  when set; `url.path` must start with `/` when set. Unknown YAML keys are
  errors (yaml.v3 `KnownFields(true)`) so typos surface loudly.
- `internal/manifest/config.go`: root-level `lambdary.yml` — `port` (default
  8000), `root` (functions root, default `.`), and a `defaults` block whose
  fields mirror the function manifest and are inherited by functions that
  don't set them. `LoadConfig(path)` + `(*Config).ApplyDefaults(*Manifest)`.
- Tests: happy path, defaults inheritance, each validation error, unknown
  keys.
- Verify: `go test ./internal/manifest/...`.

### Unit C — Discovery package  *(sequential: after B)*

- Files: `internal/discovery/discovery.go`,
  `internal/discovery/discovery_test.go`, `testdata/` fixture tree.
- `Discover(root string, cfg *manifest.Config) ([]Function, error)`.
  `Function` holds: `Name`, `Dir`, `Runtime`, `Handler`, `Route`,
  `Backend` (the manifest hint, default `auto`), `Manifest`
  (*manifest.Manifest, may carry more), and `Warnings []string`.
- A direct subdirectory of root is a function iff it contains `.lambda.yml`
  or a marker file. Detection table exactly per DESIGN.md: `package.json` →
  `nodejs22.x`; `composer.json` → `provided.al2023` (expects `bootstrap`,
  no framework-specific logic); `pyproject.toml`/`requirements.txt` →
  `python3.13`; `go.mod` → `provided.al2023`; `Gemfile` → `ruby3.3`;
  `*.csproj` → `dotnet8`; `Dockerfile` → container backend with that image.
  Manifest values always win over detection.
- Defaults: name = dir name; route = `/` + name (or manifest `url.path`).
- **Route collisions:** per DESIGN.md decisions log — never fail discovery.
  Mark all colliding functions (populate `Warnings` naming the competitors);
  callers decide presentation. Duplicate function names get the same
  treatment.
- Hidden dirs (leading `.`) and files at root level are skipped. A dir with
  `.lambda.yml` but no detectable runtime and no `runtime` key → include
  with a warning ("runtime unknown").
- Tests over a `testdata/` root covering: each marker, manifest override,
  collision, hidden dir skip, empty root (empty slice, no error).
- Verify: `go test ./internal/discovery/...`.

### Unit D — `lambdary list` + changelog  *(sequential: after C)*

- Files: `internal/cli/list.go` (replace stub), `CHANGELOG.md`,
  `internal/cli/list_test.go` (optional, if output formatting is factored
  into a testable func).
- `lambdary list` runs discovery against `--root` (loading `lambdary.yml`
  from the root if present) and prints an aligned table (`text/tabwriter`):
  `NAME  RUNTIME  BACKEND  ROUTE  DIR`. Warnings print after the table to
  stderr, one line each, prefixed `warning:`. Empty root prints a friendly
  "no functions found under <root>" to stderr, exit 0.
- `CHANGELOG.md`: Keep a Changelog format, `## [Unreleased]` with `### Added`
  entries for the CLI, discovery, manifest spec, and the RIE darwin support
  status (wording per the spike's outcome — coordinate with Unit E result,
  which will be provided in the prompt).
- Verify: full `go build ./... && go vet ./... && go test ./...`, plus run
  `go run ./cmd/lambdary list --root <testdata fixture>` and eyeball output.

### Unit E — RIE darwin-build spike  *(independent — runs parallel with A)*

- Deliverable: `plans/rie-darwin-spike.md` — a findings report. **No changes
  to Lambdary source.** All build work happens in the session scratchpad
  directory, not the repo.
- Steps: shallow-clone `aws/aws-lambda-runtime-interface-emulator` at tag
  `v1.35`; build `./cmd/aws-lambda-rie` for darwin/arm64
  (`GOTOOLCHAIN=auto` so go1.25.7 is fetched; try `go build` directly rather
  than upstream's Makefile, which may assume Linux).
- Smoke test: create a minimal custom-runtime function — a `bootstrap` shell
  script implementing the Runtime API loop with `curl`
  (GET `/2018-06-01/runtime/invocation/next`, echo the event back via POST
  `…/invocation/<id>/response`). Run the built RIE pointing at it, then
  `curl -XPOST localhost:<port>/2015-03-31/functions/function/invocations
  -d '{"ping":"pong"}'` and confirm the echoed body.
- Report: build outcome (exact commands, toolchain used), run outcome,
  any darwin-specific failures/workarounds, binary size, and a verdict for
  DESIGN.md's recorded fallback decision (is the in-house Runtime API server
  fallback needed, yes/no).
- Verify: the smoke-test curl output captured in the report.

## Sequencing

```
A (scaffold) ──▶ B (manifest) ──▶ C (discovery) ──▶ D (list + changelog)
E (RIE spike) ─────────────────────────────────────▶ (feeds wording into D)
```

Launch A and E together; B, C, D follow sequentially. D receives E's verdict.
