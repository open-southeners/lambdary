# M3 — Process path

Milestone M3 from `DESIGN.md`: run functions WITHOUT a container runtime, by
driving host-installed runtimes through AWS's Runtime Interface Emulator
(RIE) built from pinned upstream source. Node and Python shims + custom
runtimes (`bootstrap`) in scope; `--backend auto` falls back container →
process.

Environment facts (verified): darwin/arm64, go1.25.5 (+GOTOOLCHAIN=auto),
node v24.13.0, python3 3.14.2, php 8.5.7 on PATH. RIE v1.35 builds and runs
on this host (`plans/rie-darwin-spike.md`); flags and the port-9001 pitfall
are documented there and in `CURRENT_ISSUES.md`.

## Design decisions for this milestone

- **RIE acquisition chain** (embedding via `go:embed` needs a release
  pipeline that doesn't exist yet — deferred, logged in CURRENT_ISSUES):
  1. `$LAMBDARY_RIE_PATH` env override (power users / CI).
  2. Cached binary `~/.lambdary/bin/aws-lambda-rie-v1.35-<os>-<arch>`.
  3. Build from source: shallow-clone upstream at the pinned tag into a temp
     dir, `GOTOOLCHAIN=auto go build ./cmd/aws-lambda-rie`, move into cache.
     Requires `git` + `go`; error messages must name the missing tool and
     the `LAMBDARY_RIE_PATH` escape hatch.
- **Shims, not RICs** (recorded as a DESIGN.md amendment): Lambdary embeds
  (go:embed — they're small text files, no release pipeline needed) a Node
  shim and a Python shim speaking the Runtime API
  (`GET /2018-06-01/runtime/invocation/next` →
  `POST …/invocation/<id>/response` | `…/error`, plus
  `POST /2018-06-01/runtime/init/error` on load failure). Custom runtimes
  run the function dir's own `bootstrap`. `local.command` overrides the
  spawned command entirely.
- **Port allocation:** two ports per function (public invoke + internal
  runtime API), both allocated from the OS ephemeral range by binding :0 and
  closing; NEVER pass a port without also passing the other flag — the RIE's
  internal runtime API defaults to 9001 and collides otherwise (spike
  finding). Exclude 9001 defensively.

## Conventions

Same as M1/M2. stdlib only. Report — don't fix — out of scope findings.

## Units

### Unit A — `internal/rie`: binary acquisition  *(parallel with B)*

- Files: `internal/rie/rie.go`, `internal/rie/rie_test.go`.
- API (pinned — Unit C wires it, Unit B does NOT import it):
  `func Resolve(ctx context.Context, runner backend.Runner) (string, error)`
  plus `const Version = "v1.35"` and
  `var ErrBuildToolsMissing` (errors.Is-able).
- Behavior: the acquisition chain above. Cache dir:
  `~/.lambdary/bin` (respect `$LAMBDARY_HOME` override for testability;
  default `~/.lambdary`). After a source build, verify the binary runs
  (`--help` or `-h` exits 0-ish) before caching. All external commands (git,
  go, the built binary) through `backend.Runner` so unit tests can fake.
- Tests (fake runner): env override wins; cache hit skips build; build path
  issues the right git/go argv sequence and caches; missing git → 
  ErrBuildToolsMissing naming git; missing go → same naming go. One
  integration test (skipped on -short; requires network+git+go): real
  Resolve into a temp LAMBDARY_HOME produces a runnable binary — keep it,
  this host can run it (~30s clone+build).

### Unit B — shims + `internal/backend/process`  *(parallel with A)*

- Files: `internal/backend/process/process.go`, `shims/bootstrap.mjs`,
  `shims/bootstrap.py` (embedded via `go:embed`), `runtime.go` (runtime →
  spawn command), tests.
- `process.New(riePath string, runner backend.Runner) backend.Backend` —
  riePath injected (Unit C resolves it via internal/rie); package does NOT
  import internal/rie.
- Start flow per function:
  1. Write shims to `~/.lambdary/shims/<version>/` (or `$LAMBDARY_HOME`)
     once (idempotent, content-addressed by Lambdary version or file hash).
  2. Allocate invokePort + rapiPort (bind :0, skip 9001).
  3. Determine the runtime command: `local.command` override (run via
     `sh -c` in the function dir) → else runtime family: `nodejs*` → `node
     <shim.mjs> <handler>`; `python*` → `python3 <shim.py> <handler>`;
     `provided.*` → `./bootstrap` (must exist + be executable — clear error
     otherwise); other families → error "runtime not supported by the
     process backend yet".
  4. Spawn via runner.Start: `<riePath>
     --runtime-interface-emulator-address 127.0.0.1:<invokePort>
     --runtime-api-address 127.0.0.1:<rapiPort> <runtime command...>`
     — VERIFY flag names against plans/rie-darwin-spike.md and upstream
     v1.35 (`--help`) before hardcoding; adjust to reality. cwd = function
     dir; env: manifest environment + AWS_LAMBDA_FUNCTION_NAME,
     AWS_LAMBDA_FUNCTION_TIMEOUT/MEMORY_SIZE when set, HANDLER passed as the
     shim's argv (not env), PATH inherited.
  5. Readiness: HTTP round trip against the invoke endpoint (same lesson as
     the container backend — reuse the pattern, and the RIE accepts any
     POST), deadline ~15s (no image pull here). On failure: kill process,
     return error with recent stderr tail.
- Instance: InvokeURL → invoke endpoint; Stop → SIGTERM the RIE process
  group (setpgid on spawn so the runtime child dies too), escalate SIGKILL
  after ~5s; Logs → combined stdout/stderr stream.
- Shim contracts (both languages): argv[1] = handler `file.export` /
  `module.function`; loop forever: GET next (header
  Lambda-Runtime-Aws-Request-Id), call handler(event, context-lite {
  requestId, functionName from env, getRemainingTimeInMillis stub }),
  POST JSON result to /response; exceptions → POST {errorMessage, errorType,
  stackTrace} to /error; import/load failure → POST init/error then exit 1.
  Node shim: ESM, dynamic import(), supports async and callback-free
  handlers only (document); Python: importlib, supports plain functions.
  No dependencies beyond the language stdlib in either shim.
- Tests: unit (fake runner): spawn argv construction per runtime family incl.
  ports both passed and ≠9001, local.command override, provided.* missing
  bootstrap error, unsupported family error. Shim tests WITHOUT the RIE:
  run each shim as a child against a stub Runtime API served by httptest
  (Go test drives node/. python3 directly — skip if the interpreter is
  missing) asserting next→response happy path, handler exception → /error
  with errorType, bad handler spec → init/error. Process integration test
  (skip on -short; NO network needed if a fake rie path...) — full RIE
  integration lands in Unit C's e2e instead; here use a FAKE rie binary
  (tiny shell script in testdata that execs the runtime command and serves
  nothing) only if cheap; otherwise rely on argv unit tests + Unit C e2e.

### Unit C — wiring, e2e, changelog  *(after A & B)*

- Files: `internal/cli/dev.go`, `internal/cli/invoke.go`,
  `internal/cli/list.go` (BACKEND column: show resolved auto → "container"/
  "process" is NOT resolvable without probing — leave list as-is, note it),
  `internal/cli/backend.go` (new shared helper), `internal/cli/e2e_test.go`
  (extend), `CHANGELOG.md`.
- Shared backend construction helper: `--backend` auto → try
  DetectContainerCLI; on ErrNoContainerCLI/ErrDaemonUnreachable fall back to
  process (resolve RIE via internal/rie, warn to stderr what happened:
  "docker daemon unreachable — using the process backend (host runtimes)");
  container → container or fail; process → process or fail (rie.Resolve
  errors surface with remedies). Used by dev + invoke standalone mode.
  Startup summary should name the active backend.
- E2E (extend e2e_test.go with a second, process-backend test): fixture
  functions run via process backend on THIS host — hello (python) + a new
  node function testdata/demo/node-hello (echo handler, nodejs22.x — runs on
  host node 24; fine, document that process backend uses host versions);
  assert the same browser-style + shaped + passthrough behaviors WITHOUT
  docker involvement. Gate: skip on -short or when node/python3 absent; RIE
  resolved via rie.Resolve (cache warm after first run; allow generous
  first-build deadline, reuse LAMBDARY_HOME temp to avoid polluting the real
  one — or prime the real cache, your call, document it).
- Manual verification (REQUIRED): `lambdary dev --backend process` against
  the demo root; curl /hello?x=1, /shaped, /node-hello; SIGINT; confirm no
  lambdary processes remain (pgrep aws-lambda-rie empty).
- CHANGELOG Added: run functions without Docker — process backend on host
  runtimes via AWS's RIE built from pinned source (first run builds/caches),
  Node+Python+custom-runtime (`bootstrap`) support, automatic
  container→process fallback with a clear notice, `local.command` override.
  Honest note: process backend runs on host OS/runtime versions (fidelity
  caveat per DESIGN.md).

## Sequencing

```
A (internal/rie) ──┐
                   ├─▶ C (CLI wiring + e2e + changelog)
B (process backend)┘
```

## Deferred (log in CURRENT_ISSUES.md)

- go:embed of the RIE binary into lambdary releases (needs release
  pipeline; acquisition chain covers dev use).
- PHP/other interpreted families beyond Node/Python in the process backend
  (custom-runtime `bootstrap` covers Bref-style PHP today).
- Port-allocation race: bind-and-close allocation has a TOCTOU window;
  acceptable locally, revisit if it ever flakes.
