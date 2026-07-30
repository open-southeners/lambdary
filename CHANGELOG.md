# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

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
