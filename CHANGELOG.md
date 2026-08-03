# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Prebuilt `lambdary` binaries for Linux and macOS (amd64 + arm64) are now
  attached to every GitHub release, with SHA-256 checksums, and release
  notes are generated automatically from this changelog.
- **Function discovery**: every direct subdirectory of the project root that
  contains a `.lambda.yml` or a recognizable project file (`package.json`,
  `composer.json`, `pyproject.toml`/`requirements.txt`, `go.mod`, `Gemfile`,
  `*.csproj`, `Dockerfile`) is picked up as a Lambda function, with its
  runtime inferred from that marker; a `.lambda.yml` can always override the
  inferred runtime.
- **`.lambda.yml` manifest**: per-function configuration that mirrors AWS
  Lambda's own vocabulary (`runtime`, `handler`, `timeout`, `memory`,
  `architectures`, `environment`, `url`) plus a local-only `local` section
  (`backend`, `image`, `command`, `env_file`) for how the function runs on
  your machine. A project-wide `lambdary.yml` can set `port`, the functions
  `root`, and `defaults` inherited by every function manifest. Unknown keys
  in either file are rejected so typos surface immediately instead of being
  silently ignored.
- **`lambdary list` command**: prints every discovered function with its
  runtime, backend, and local route. Misconfigurations — route collisions,
  duplicate function names, unresolvable runtimes, invalid manifests — are
  reported as warnings alongside the table rather than aborting the listing,
  so a problem in one function never hides the rest.
- macOS groundwork: confirmed AWS's own Lambda Runtime Interface Emulator
  (pinned upstream tag `v1.35`) builds and runs natively on Apple Silicon, so
  local (non-container) function execution will run on AWS's real emulator
  instead of a reimplementation.
- **`lambdary dev`**: runs every discovered function behind one local HTTP
  server, with an AWS-compatible invoke endpoint
  (`POST /2015-03-31/functions/<name>/invocations`) that works unchanged
  with `aws lambda invoke --endpoint-url`. Functions start on their first
  invocation by default; `--eager` starts them all up front instead.
  Ctrl-C (or SIGTERM) shuts the server down gracefully and stops every
  running function before exiting.
- **`lambdary invoke <function>`**: invokes a function once and prints its
  response. It sends the request to an already-running `lambdary dev`
  server when one is listening on `--port`, or otherwise starts the
  function just for that one call and stops it again afterwards. The
  event body comes from `-e <file>`, `-e -` (stdin), or defaults to `{}`.
- Functions run as containers on AWS's own Lambda base images: each gets
  its own container with its code bind-mounted in, starts on demand, and
  is torn down cleanly on shutdown — no manual cleanup needed even after a
  crash, since containers are labelled and discoverable
  (`docker ps --filter label=lambdary`).
- Fail-safe collision handling at runtime: if two functions share a name or
  route, `lambdary` still starts and serves every other function normally;
  hitting the colliding function or route returns a `409` response naming
  every function competing for it.
- Functions are now reachable as plain HTTP endpoints at their local route
  (e.g. `GET /hello`), not just through the invoke passthrough: a request
  arrives at the handler as an AWS Function URL / API Gateway v2 (`2.0`)
  event, with headers, cookies, query strings, and binary bodies mapped in
  both directions. Handlers can return either a plain value (serialized as
  the JSON body, `200`) or a shaped response (`statusCode`, `headers`,
  `cookies`, `isBase64Encoded`) for full control over the HTTP reply.
- **Process backend**: run functions without Docker, using `--backend
  process` (or the default `--backend auto`, which now falls back to it
  automatically — with a clear notice — when no container runtime is
  usable). Functions are driven by AWS's own Lambda Runtime Interface
  Emulator against your host's own Node, Python, or custom (`bootstrap`)
  runtime instead of a container; the emulator is built from pinned
  upstream source and cached on first use (a one-time ~30s build). Node and
  Python functions run through small built-in shims with no extra
  dependencies; `local.command` in `.lambda.yml` still overrides the
  spawned command entirely. Since there's no container image involved, the
  process backend runs on whatever runtime versions are installed on your
  machine rather than AWS's pinned Lambda runtime — closer to your host,
  not a perfect match for AWS's environment.
- **Hot reload**, on by default in `lambdary dev`: editing a function's code
  restarts just that function, cold-starting it fresh on its next request.
  Adding or removing a function directory, or editing a `.lambda.yml` or the
  project's `lambdary.yml`, reconfigures the whole server on the fly —
  re-discovering every function and swapping in the new set without
  dropping the listener. A broken edit is logged with what went wrong and
  the previous, working configuration keeps running rather than taking the
  server down. Disable with `--no-reload`.
- **Live function logs**: each function's combined stdout/stderr — including
  the emulator's Lambda-style `REPORT` lines — streams to `lambdary dev`'s
  own output as soon as an instance starts, prefixed `[<name>]` in a color
  cycled per function. Color is skipped automatically when output isn't a
  terminal, and always when `NO_COLOR` is set.
- **`lambdary init <name>`**: scaffolds a new function directory with a
  ready-to-run handler and `.lambda.yml`, so starting a function needs no
  boilerplate. `--runtime` picks the stub (`nodejs22.x` by default,
  `python3.13`, or `provided.al2023` — a working Runtime API `bootstrap`
  echo loop, not just a placeholder). Refuses to overwrite an existing
  directory.
- **API Gateway REST API (`1.0`) payload format**, selectable per function
  via `url.payload: "1.0"` instead of the default `"2.0"` — events carry
  the REST API shape (`resource`, `pathParameters.proxy`,
  `multiValueHeaders`, …) and, like the real service, the response
  contract is strict: a handler must return a shaped `{statusCode, ...}`
  object.
- **`Dockerfile` functions**: a function directory with a `Dockerfile` and
  no `runtime`/`local.image` now builds and runs automatically — no more
  manual `docker build`. It rebuilds on every start, so hot reload picks up
  `Dockerfile` and code edits the same way any other function does.
- **`.lambdary/lock`**: the container backend records each function's
  resolved image digest the first time it starts and pins subsequent
  starts to that digest. Commit the file for reproducible starts across
  machines; a locally built `Dockerfile` image or an explicit
  `local.image` override is never pinned.
- **`local.env_file`**: loads extra environment variables for a function
  from a dotenv-style file (`KEY=VALUE` per line, `#` comments, optional
  `export` prefix), merged under the manifest's own `environment` block —
  explicit `environment` keys always win on conflict.

### Changed

- Function routes no longer answer with a `501` placeholder — they serve
  real responses from the handler, per the HTTP event mapping above.
