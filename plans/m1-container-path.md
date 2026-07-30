# M1 — Container path

Milestone M1 from `DESIGN.md`: container backend + router with invoke
passthrough; `lambdary dev` serves functions via Docker; `lambdary invoke`
works. HTTP↔event mapping is **M2** — out of scope here; the router serves
the raw invoke passthrough only.

Environment facts (verified): host has `docker` CLI 29.4.3; podman/finch/
nerdctl absent. Daemon was initially stopped — probing must check daemon
reachability, not just CLI presence. M0 landed: `internal/manifest`,
`internal/discovery`, cobra CLI in `internal/cli`.

## Conventions (all units)

- Deps: stdlib only beyond what's already in go.mod (cobra, yaml). Container
  control via the CLI (`docker`/`podman`/…), NOT the Docker SDK — per
  DESIGN.md.
- All exec calls go through a small runner interface so unit tests can stub
  the CLI; integration tests against the real daemon are guarded with
  `testing.Short()`/availability checks and `t.Skip` when unavailable.
- Table-driven tests; errors wrapped with `%w`; user-facing errors name the
  remedy. Verify: `go build ./... && go vet ./... && go test ./...`,
  `gofmt -l .` clean.
- Report — don't fix — anything out of scope.

## Deferred (logged, not in M1)

- `.lambdary/lock` image-digest pinning (decisions log) — deferred until the
  pull/resolve flow settles; entry to add in CURRENT_ISSUES.md.
- Building `Dockerfile`-marker functions (`docker build`) — deferred to keep
  M1 to AWS base images; Dockerfile functions error with "not yet supported".

## Units

### Unit A — Backend interface, probe, image map  *(sequential: first)*

- Files: `internal/backend/backend.go`, `internal/backend/exec.go`,
  `internal/backend/probe.go`, `internal/backend/images.go`, tests.
- `backend.go`: the contract every backend implements —
  `type Backend interface { Start(ctx, fn discovery.Function) (Instance, error) }`
  and `type Instance interface { InvokeURL() string; Stop(ctx) error;
  Logs() io.Reader }` (adjust shape if implementation argues for it, but keep
  Start/Stop/InvokeURL/Logs semantics from DESIGN.md).
- `exec.go`: `Runner` interface wrapping `os/exec` (Run/Output/Start with
  ctx), real impl + test fake.
- `probe.go`: `DetectContainerCLI(runner) (cli string, ok bool)` — order
  docker → podman → finch → nerdctl; a CLI counts only if the daemon
  answers (`<cli> version --format {{.Server.Version}}` succeeds, or
  `<cli> info` exit 0). Distinguish "no CLI" from "CLI found, daemon down"
  in the returned error so the CLI layer can say "start Docker Desktop".
- `images.go`: runtime identifier → AWS base image:
  `nodejs22.x → public.ecr.aws/lambda/nodejs:22`,
  `python3.13 → public.ecr.aws/lambda/python:3.13`,
  `ruby3.3 → public.ecr.aws/lambda/ruby:3.3`,
  `dotnet8 → public.ecr.aws/lambda/dotnet:8`,
  `provided.al2023 → public.ecr.aws/lambda/provided:al2023`,
  plus the general pattern (family + version split) so other AWS runtime ids
  resolve without new table entries where the naming is regular. Unknown
  runtime → error naming the runtime.
- Verify: unit tests with fake runner (probe orders, daemon-down case, image
  mapping table).

### Unit B — Container backend  *(sequential: after A)*

- Files: `internal/backend/container/container.go` (+ helpers), tests.
- `container.New(cli string, runner backend.Runner) backend.Backend`.
- `Start`: `docker run -d --rm --label lambdary=1 --label
  lambdary.function=<name> -p 127.0.0.1:0:8080 -v <fnDir>:/var/task:ro
  [-e K=V per manifest environment] -e AWS_LAMBDA_FUNCTION_NAME=<name>
  [-e AWS_LAMBDA_FUNCTION_TIMEOUT=<timeout>] [--memory <mem>m
  -e AWS_LAMBDA_FUNCTION_MEMORY_SIZE=<mem>] [--platform linux/<arch> when
  manifest architectures set] <image> [<handler>]` — image from
  `backend.ImageFor(runtime)` unless manifest `local.image` overrides;
  handler passed as container CMD when set.
- After start: resolve the ephemeral host port (`docker port <id> 8080`),
  wait for readiness with backoff until deadline. *(Implementation note: a
  bare TCP dial is NOT sufficient on Docker Desktop — docker-proxy accepts
  host-side connections before the in-container RIE listens, causing
  intermittent EOFs; readiness requires one full HTTP round trip.)* (~30s,
  covers image pull on the run itself — actually pull happens before run
  returns; keep run without a separate pull step, document that first run
  is slow), expose `InvokeURL() = http://127.0.0.1:<port>/2015-03-31/functions/function/invocations`.
- `Stop`: `docker stop <id>` (with --rm cleanup implied). `Logs`: stream
  `docker logs -f <id>` stdout+stderr combined.
- Dockerfile-marker functions (empty runtime, backend container, no image):
  return the "not yet supported" error per Deferred section.
- Tests: fake-runner unit tests asserting the exact argv built for a full
  manifest (env, memory, platform, handler) and a minimal one; readiness
  timeout path; PLUS one integration test (skipped unless the real daemon
  answers and `-short` not set): start a python3.13 hello function from
  testdata, invoke via HTTP POST, assert echo/result, stop, and assert the
  container is gone (`docker ps` label filter).
- Verify: unit tests always; integration test if daemon up.

### Unit C — Router  *(sequential: after A, parallel with B)*

- Files: `internal/router/router.go`, `internal/router/manager.go`, tests.
- `manager.go`: `Manager` — lazy per-function lifecycle over a
  `backend.Backend`: `Ensure(ctx, name) (invokeURL string, err error)`
  starts on first use (guarded per function with singleflight-style mutex —
  stdlib only, a per-function `sync.Mutex`/`sync.Once` map is fine),
  `StopAll(ctx)`. Per-function serialization: `Acquire(name)/Release(name)`
  or expose a per-function `sync.Mutex` the router holds for the duration of
  one invoke — DESIGN.md: RIE handles one invocation at a time. Timeout: use
  function timeout (default 30s per DESIGN default? manifest default is
  unset — use 30s when unset, document) as the queue+invoke deadline.
- `router.go`: `http.Handler` serving:
  - `POST /2015-03-31/functions/{name}/invocations` — passthrough: body →
    function's invoke URL (via Manager.Ensure), response body/status back.
    Unknown function → 404 JSON `{"message":"function not found: <name>"}`
    listing known names. Function on a colliding route or with a
    route-collision warning → still invocable by NAME here (name collisions:
    409 per below).
  - Colliding resources per DESIGN.md decisions log: any request to a route
    or name marked colliding → `409` JSON naming all competitors.
  - `ANY /{route}/*` (function URL paths) → `501` JSON: "HTTP event mapping
    lands in M2 — invoke via POST /2015-03-31/functions/<name>/invocations".
  - `GET /` (no function match) → tiny JSON index of functions + their
    routes (harmless, aids discoverability; keep minimal).
- Router takes `[]discovery.Function` + Manager; no global state; graceful
  shutdown via context.
- Tests: httptest with a fake Backend (records invocations, returns canned
  responses): passthrough happy path, 404 unknown, 409 collision, 501 route
  hit, per-function serialization (two concurrent invokes to one function
  are serialized; to two functions run concurrently), lazy start happens
  once under concurrency.

### Unit D — CLI `dev` + `invoke` + changelog  *(sequential: after B & C)*

- Files: `internal/cli/dev.go`, `internal/cli/invoke.go`, changes to
  `internal/cli/root.go` (register), `CHANGELOG.md` update, tests where
  factorable.
- `lambdary dev`: flags `--port` (default from lambdary.yml or 8000),
  `--eager` (start all backends up front), `--backend` (auto|container;
  "process" errors "lands in M3"). Flow: resolve root+config (same
  resolution as `list`), discover, probe container CLI (fail with remedy
  message if none — e.g. "no container runtime found: install Docker or
  start Docker Desktop; the process backend lands in M3"), build Manager +
  Router, serve on port; print startup summary (functions + routes + invoke
  hint), warnings to stderr; SIGINT/SIGTERM → graceful StopAll then exit 0.
- `lambdary invoke <fn> [-e event.json] [--port]`: if a dev server is
  reachable on the target port (GET / responds as lambdary), POST the event
  (default `{}`, or file via -e, or stdin via `-e -`) to its passthrough and
  print the response body to stdout (exit 1 on non-2xx with body to
  stderr). If no server: standalone mode — discover, start just that
  function's backend, invoke once, print, stop. Document both in help text.
- CHANGELOG `### Added` entries: `lambdary dev` (one local HTTP server,
  AWS-compatible invoke endpoint usable with `aws lambda invoke
  --endpoint-url`), `lambdary invoke`, container execution on AWS base
  images (bind-mounted code, per-function containers, lazy start, graceful
  shutdown), fail-safe collision behavior at runtime (409 naming
  competitors). Mention Dockerfile-function limitation under a `### Known
  limitations`-style note ONLY if Keep a Changelog section fits — otherwise
  omit (changelog sections are fixed; put limitation in README later).
- Verify: full suite + manual: `go run ./cmd/lambdary dev --root <fixture>`
  with daemon up → curl passthrough works end-to-end (capture output);
  `lambdary invoke` against it; Ctrl-C cleanup leaves no `lambdary=1`
  containers.

## Sequencing

```
A (interface/probe/images) ──▶ B (container backend) ──▶ D (dev+invoke+changelog)
                           └─▶ C (router)            ──▶
```

A first; B and C in parallel; D last, fed with B/C results.
