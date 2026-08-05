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
