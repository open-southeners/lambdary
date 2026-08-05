# Current issues

Discovered during orchestrated work; routed here instead of fixed inline.

## Process backend limited to Node, Python, and custom runtimes (M3)

- **Where:** `internal/backend/process` runtime table.
- **What:** other interpreted families (ruby, dotnet, java) have no shim, even
  though discovery detects them (`Gemfile` → `ruby3.3`, `*.csproj` →
  `dotnet8`); those functions currently run only via the container backend.
  PHP works via the custom-runtime `bootstrap` convention.
- **Fix:** add shims per family as demand appears; each is small (~120 lines)
  against the frozen Runtime API.

## Port-allocation TOCTOU race in the process backend (M3)

- **Where:** `internal/backend/process/ports.go` (`allocatePorts`).
- **What:** ports are allocated by binding `127.0.0.1:0` and closing before
  the RIE binds them, leaving a window where another process could grab the
  port. Acceptable locally; documented in the code comment.
- **Fix:** only if it ever flakes in practice — retry `Start` on
  bind-failure, or pass pre-bound listeners if upstream ever supports it.

## `lambdary logs [fn]` command missing (DESIGN gap)

- **Where:** `internal/cli` (DESIGN.md "CLI surface" lists it; 4 of 5
  commands exist).
- **What:** log streaming exists only inside `dev` (`internal/cli/logs.go`);
  there is no standalone `logs` command to follow a running dev server's
  function output from another terminal.
- **Fix:** needs a transport first (e.g. an SSE or plain-text stream endpoint
  on the dev server, `GET /_lambdary/logs[?fn=]`), then a thin CLI command
  consuming it; alternatively drop the command from DESIGN if `dev`'s inline
  streaming is deemed sufficient.

## Compiled non-Dockerfile runtimes are not built before container start (DESIGN gap)

- **Where:** `internal/backend/container`.
- **What:** DESIGN's "compiled ones rebuild the artifact first" only happens
  for `Dockerfile` functions (`docker build` each start). A bare `go.mod`
  (`provided.al2023`) or `*.csproj` (`dotnet8`) function has no
  compile-the-artifact step — the container just runs whatever binary the
  developer last built by hand.
- **Fix:** per-runtime build hooks (e.g. `go build -o bootstrap` in-container
  or on host) before start/restart, or document that compiled runtimes
  require a `Dockerfile` or a manual build step.

## `provided.*` on the container backend cannot work without a Dockerfile (bug)

- **Where:** `internal/backend/container/argv.go` (`runArgs`).
- **What:** worse than the gap above — even a hand-built `bootstrap` binary
  sitting in the function directory can never run, Dockerfile or not.
  `runArgs` only appends a CMD argument when `fn.Handler` is set, but
  `provided.*` functions have no `handler` key (see `internal/cli/init.go`'s
  own `provided.al2023` scaffold), so no CMD arg reaches the container.
  `public.ecr.aws/lambda/provided:al2023`'s entrypoint
  (`/lambda-entrypoint.sh`) then exits immediately with "entrypoint requires
  the handler name to be the first argument". Even past that, its
  `RUNTIME_ENTRYPOINT` is hardcoded to `/var/runtime/bootstrap` — empty in
  the base image — never `/var/task/bootstrap`, so the function's own
  bind-mounted `bootstrap` would never be found either way. Confirmed by
  hand against the real image while building `examples/checksum-go`, which
  works around it with a function-owned `Dockerfile` that `COPY`s the
  compiled binary to `/var/runtime/bootstrap` instead.
- **Fix:** either always pass a placeholder CMD arg and bind-mount/copy the
  function's `bootstrap` to `/var/runtime/bootstrap` before `docker run`, or
  accept this combination isn't supported and say so — in `validateInitRuntime`
  / README's backend table — that `provided.*` needs a `Dockerfile` to run on
  the container backend.

## A dead process-backend runtime is never retried, only its symptom (idle timeout) is fixed

- **Where:** `internal/router/manager.go` (`Manager.Ensure`).
- **What:** `Ensure` caches an instance in `m.instances[name]` the first
  time it starts one and, on every later call, returns it straight from
  that map with no liveness check. If the runtime process backing it ever
  exits on its own (the node shim's 5-minute idle `fetch()` crash fixed
  above was one real cause; an unhandled promise rejection or any other
  runtime-side crash would do the same), every subsequent invocation of
  that function keeps failing with `Runtime.ExitError` — reproduced by
  hand: after the idle-timeout crash, a second `screenshot-node` request
  minutes later still failed, even though the underlying bug (the crash
  itself) was already fixed by then. Restart only happens via
  `Manager.Restart`, which only `dev`'s watcher calls, on a code change.
- **Fix:** have `Ensure` (or the instance itself) detect a dead backing
  process and transparently start a fresh instance instead of returning a
  stale, unusable URL — closer to real Lambda's behavior, where a crashed
  execution environment is simply replaced by a new one on the next
  invocation.
