# Lambdary — Initial Design

Local development server for AWS Lambda functions. A single Go binary that
discovers functions in a directory tree, runs each one on the same runtimes AWS
uses (container images or host-installed runtimes), and exposes them all behind
one local HTTP server.

## Goals

- **Directory-based functions.** Each subdirectory of a configured root is one
  function; the directory name is the function name by default.
- **One HTTP entrypoint.** `http://localhost:{port}/{function_name}` routes to
  the right function, whichever backend runs it.
- **AWS-faithful runtimes.** Prefer AWS's official Lambda container images
  (which bundle the Runtime Interface Emulator). When no container runtime
  exists on the host, fall back to the runtimes already installed on the
  machine (node, php, python, …).
- **No proprietary config spec.** `.lambda.yml` reuses AWS Lambda's own
  configuration vocabulary (`runtime`, `handler`, `memory`, `timeout`,
  `architectures`, `environment`) rather than inventing new concepts.

Non-goals for the initial version: deployment/packaging to AWS, Windows
support outside WSL, emulating Lambda orchestration (throttling, scaling,
IAM), X-Ray, layers, extensions.

## Background: how Lambda invocation works locally

Two AWS-defined HTTP APIs anchor everything:

1. **Invoke API** — what callers use. The RIE exposes
   `POST /2015-03-31/functions/function/invocations` on port `8080` inside
   AWS's base images; the request body is the raw JSON event, the response
   body is the handler's return value.
2. **Runtime API** — what runtimes use. A long-poll API
   (`GET /2018-06-01/runtime/invocation/next`, `POST …/response`,
   `POST …/error`) that Runtime Interface Clients (RICs) speak. The RIE is
   simply a server for this API plus the Invoke API in front of it.

Three constraints drive the architecture:

- The RIE **cannot be embedded as a Go library**: its module path is importable
  (`github.com/aws/aws-lambda-runtime-interface-emulator`), but the entire
  implementation sits under `internal/` packages, which the Go compiler
  forbids importing from other modules. Only the `main` package is public.
- AWS only **releases RIE binaries for Linux** (x86-64/arm64). The source has
  no OS-suffixed files, so darwin builds may work — but that needs a spike,
  not an assumption.
- The RIE processes **one invocation at a time** per instance, so each
  function gets its own emulator instance and the router queues or scales
  accordingly.

Consequence: to reuse AWS's code rather than maintain our own emulator, the
RIE is consumed as a **pinned-source subprocess**, not a library — see the
process backend section.

## Architecture

```
                       ┌──────────────────────────────────────────────┐
                       │                lambdary (Go)                 │
 HTTP client ──────────▶  Router / reverse proxy  :8000               │
 (browser, curl,       │    /function_a/*  ─┐                        │
  aws cli, SDK)        │    /function_b/*  ─┼─ event mapping          │
                       │                    │  (HTTP ⇆ Lambda event)  │
                       │  ┌─────────────────┴──────────────────┐      │
                       │  │        Backend manager             │      │
                       │  │  per function: pick + supervise    │      │
                       │  └───────┬───────────────────┬────────┘      │
                       │          │                   │               │
                       │  Container backend    Process backend        │
                       │  docker/podman run    vendored RIE binary    │
                       │  AWS base image       (pinned AWS source)    │
                       │  (RIE built in,       driving host runtime   │
                       │   port 8080)          + official RIC         │
                       └──────────────────────────────────────────────┘
                                  ▲                   ▲
                            fsnotify watcher: restart on code change
```

### Components

| Component | Responsibility |
|---|---|
| `discovery` | Scan the root folder; every direct subdirectory containing a `.lambda.yml` or a recognizable project file is a function. |
| `manifest` | Parse/validate `.lambda.yml`, merge with detected defaults. |
| `backends` | Interface `Backend` with `Start/Stop/InvokeEndpoint/Logs`; implementations: `container`, `process`. |
| `rie` | Manages the vendored RIE binary (built from pinned upstream source, embedded via `go:embed`); spawns/supervises one RIE subprocess per function for the process backend. |
| `router` | HTTP server; path-based routing, HTTP⇆event mapping, invoke passthrough. |
| `watcher` | fsnotify on function dirs; debounce and restart the affected function. |
| `cli` | Cobra-based commands (see below). |

### HTTP layer: stdlib first, Caddy optional

Recommendation: build the router on `net/http` + `httputil.ReverseProxy` for
v0. The routing need (path prefix → one upstream per function, plus
request/response transformation) is small, and the event-mapping step means we
are not a transparent proxy anyway — we terminate and re-issue requests.

Caddy is embeddable and worth adding later behind the same `Router` interface
if we want its value-adds (local TLS with trusted certs, HTTP/3, admin API for
dynamic config). Traefik is not practically embeddable as a library — rule it
out. Decision recorded here so we don't relitigate: **stdlib now, Caddy as a
later opt-in, no Traefik.**

## Function discovery & runtime detection

A directory under the root is a function if it contains `.lambda.yml` **or**
one of the detectable project files. Detection supplies defaults; the manifest
always wins.

| Marker file | Detected runtime (default) |
|---|---|
| `package.json` | `nodejs22.x` |
| `composer.json` | `provided.al2023` (custom runtime; a `bootstrap` file is expected — Bref projects satisfy this out of the box, but Lambdary has no Bref-specific logic) |
| `pyproject.toml` / `requirements.txt` | `python3.13` |
| `go.mod` | `provided.al2023` (compiled bootstrap) |
| `Gemfile` | `ruby3.3` |
| `*.csproj` | `dotnet8` |
| `Dockerfile` | container backend with that image, as-is |

## `.lambda.yml` spec (v0)

All keys optional; names and value formats mirror AWS Lambda's public
configuration so nothing here is Lambdary-proprietary except the top-level
`backend`/`local` hints, which are inherently local-dev concerns.

```yaml
# root_folder/function_a/.lambda.yml
name: function_a            # default: directory name
runtime: nodejs22.x         # AWS runtime identifier
handler: index.handler
timeout: 30                 # seconds
memory: 512                 # MB (advisory locally; passed to container limits)
architectures: [arm64]      # default: host arch
environment:
  TABLE_NAME: local-table
url:
  path: /function_a         # route prefix; default: /{name}
  payload: "2.0"            # event format: Function URL / API GW v2 (default)

# Local-dev-only section (the one place we go beyond AWS vocabulary)
local:
  backend: auto             # auto | container | process
  image: ""                 # override container image
  command: ""               # override runtime binary for process backend
  env_file: .env            # extra env loaded locally only
```

A root-level `lambdary.yml` (optional) can set the port, root folder, and
per-project defaults that individual `.lambda.yml` files inherit.

## Execution backends

### Selection (`backend: auto`)

1. If a container runtime is available (probe in order: `docker`, `podman`,
   `finch`, `nerdctl` — CLI presence + daemon/socket reachable) → **container**.
2. Else if the required host runtime binary exists (`node`, `php`, `python3`,
   …) → **process**.
3. Else fail with a message that names both remedies ("install Docker, or
   install Node 22+").

### Container backend

- Image: `public.ecr.aws/lambda/{runtime}:{tag}` matching the manifest runtime
  (e.g. `public.ecr.aws/lambda/nodejs:22`); custom runtimes use
  `public.ecr.aws/lambda/provided:al2023`. RIE is already the entrypoint.
- Code is bind-mounted at `/var/task` (read-only) so hot reload needs no image
  rebuild; interpreted runtimes restart the container on change, compiled ones
  rebuild the artifact first.
- Each function container publishes its internal `:8080` on an ephemeral host
  port tracked by the backend manager.
- `memory`/`timeout` map to container limits and
  `AWS_LAMBDA_FUNCTION_TIMEOUT`.

### Process backend

Principle: **do not write our own emulator — run AWS's.** The RIE cannot be
imported as a library (everything is `internal/`), so we consume it as a
binary built from pinned upstream source:

- Lambdary's release pipeline builds `cmd/aws-lambda-rie` from a pinned
  upstream tag for each target OS/arch and embeds the result in the
  `lambdary` binary via `go:embed`, extracted to `~/.lambdary/bin/` on first
  use. Upgrading AWS's emulator = bumping the pinned tag; zero emulator code
  of our own to maintain.
- Per function, Lambdary spawns one RIE subprocess on an ephemeral port; the
  RIE itself spawns the runtime process with `AWS_LAMBDA_RUNTIME_API` wired
  up, exactly as in AWS's base images.
- *(Amended in M3.)* The runtime process is a tiny dependency-free **shim**
  per language family (Node, Python) that speaks the versioned Runtime API
  and loads the handler — not the official RICs as originally written:
  `aws-lambda-ric`/`awslambdaric` both carry native C/C++ components that
  would require per-function compile toolchains, exactly the friction this
  project exists to remove. The Runtime API is small and frozen, so the shim
  surface is ~50 lines per language. Custom runtimes (`provided.*`) run the
  function's own `bootstrap` directly; `local.command` overrides everything
  when a project wants the real RIC or anything else.
- **M0 spike (required):** verify the pinned RIE tag cross-compiles and runs
  on darwin — AWS only ships Linux binaries, though the source shows no
  OS-specific files. If darwin builds fail, the recorded fallback is a
  minimal in-process Runtime API server behind the same interface, used only
  where the real RIE can't run.
- Caveat to document loudly: the process backend runs on the host OS with host
  libraries — faithful to the Runtime API, not to Amazon Linux. The container
  backend is the fidelity option.

## Routing & event mapping

- `ANY /{function_path}/*` → build a **Lambda Function URL / API Gateway v2
  (`2.0`) event** from the HTTP request (method, raw path with the function
  prefix stripped, headers, query, base64 body flag), POST it to the
  function's invoke endpoint, then map the result (`statusCode`, `headers`,
  `cookies`, `body`, `isBase64Encoded`) back to an HTTP response. Non-shaped
  return values follow Function URL rules (serialized as JSON, `200`).
- **Invoke passthrough:** Lambdary also serves
  `POST /2015-03-31/functions/{name}/invocations`, forwarding the body as a
  raw event. This makes `aws lambda invoke --endpoint-url http://localhost:8000`
  and any AWS SDK work against Lambdary unchanged — useful for testing
  function-to-function calls and non-HTTP event shapes (SQS, S3 fixtures).
- Since each emulator instance is single-invocation, the router serializes
  requests per function (queue with the function's `timeout` as deadline).
  Optional later: scale-out by starting N instances per function.

## Lifecycle

- **Lazy start** by default: a function's backend starts on first request
  (cold start, appropriately enough); `--eager` flag to prewarm all.
- **Hot reload:** fsnotify per function dir, 300ms debounce, restart only the
  affected function. Manifest changes re-run discovery.
- **Logs:** each backend streams stdout/stderr, multiplexed to the CLI with a
  per-function color-coded prefix (honest `REPORT`-style line per invocation:
  duration, billed duration, memory — matching Lambda's log format).
- **Shutdown:** SIGINT stops all containers/processes; containers run with
  `--rm` and a `lambdary` label so orphans are findable (`docker ps
  --filter label=lambdary`).

## CLI surface (v0)

```
lambdary dev [root]        # discover, watch, serve (the main command)
  --port 8000  --eager  --backend auto|container|process
lambdary list              # discovered functions, runtime, backend, route
lambdary invoke <fn> [-e event.json]   # one-shot raw invoke, prints result
lambdary logs [fn]         # follow logs
lambdary init [runtime]    # scaffold a function dir + .lambda.yml
```

## Proposed repo layout

```
cmd/lambdary/            # main + cobra commands
internal/discovery/
internal/manifest/
internal/backend/        # backend.go (interface), container/, process/
internal/rie/            # vendored RIE binary management (go:embed, extraction, supervision)
internal/router/         # http server, event mapping
internal/watcher/
internal/logging/
```

Key dependencies: `spf13/cobra`, `fsnotify/fsnotify`, `goccy/go-yaml` (or
`gopkg.in/yaml.v3`). Container control via the `docker` CLI first (works for
docker/podman/finch alike through their CLI compatibility) — the Docker SDK
can come later if we need event streams.

## Milestones

1. **M0 — Skeleton:** CLI scaffold, discovery, manifest parsing, `list`.
   Includes the RIE darwin-build spike (build pinned upstream tag for
   `darwin/arm64`, smoke-test an invocation).
2. **M1 — Container path:** container backend + router with invoke
   passthrough; `dev` serves functions via Docker; `invoke` works.
3. **M2 — HTTP events:** Function URL (`2.0`) event mapping both directions;
   browser-usable endpoints.
4. **M3 — Process path:** vendored-RIE subprocess supervision + Node process
   backend (first language), then Python and custom runtimes (`bootstrap`).
5. **M4 — DX:** hot reload, log multiplexing, `init` scaffolding, `--eager`.
6. **M5 — Extras (post-v0):** Caddy embedding (TLS/HTTP3), per-function
   concurrency, event fixtures for SQS/S3 shapes, env-file layering.

## Decisions log

- **RIE consumption:** as a subprocess built from pinned upstream source and
  embedded in the `lambdary` binary — never reimplemented, never imported as
  a library (upstream keeps everything under `internal/`, so library
  embedding is impossible). A minimal in-house Runtime API server exists only
  as a recorded fallback if darwin builds of upstream fail. **M0 spike
  outcome: not needed** — RIE v1.35 builds with plain `go build` on
  darwin/arm64 and passes an invoke round trip (see
  `plans/rie-darwin-spike.md`).
- **PHP / custom runtimes:** stay frameworkless. Anything declaring (or
  detected as) `provided.*` follows AWS's own custom-runtime contract — a
  `bootstrap` entrypoint in the function directory. Bref works because Bref
  follows that contract on real Lambda; Lambdary carries no Bref-specific
  logic, and the same transparency applies to any other custom-runtime
  framework.
- **Version pinning:** yes — a `.lambdary/lock` file pins container image
  digests, RIC versions, and the vendored RIE tag for reproducible dev
  environments.
- **Route collisions:** detected at discovery time, but the binary is
  fail-safe — the dev server never refuses to start over a collision.
  Non-colliding functions serve normally; a warning is logged at startup, and
  any request hitting a colliding path gets an error response (`409`) that
  names the functions competing for that route so the user is notified
  exactly where they'd notice it.
