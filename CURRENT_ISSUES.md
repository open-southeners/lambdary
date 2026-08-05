# Current issues

Discovered during orchestrated work; routed here instead of fixed inline.

## Process backend has no native shim for dotnet/java (optional perf work)

- **Where:** `internal/backend/process` runtime table (`Supports`,
  `runtimeCommand`).
- **What:** Node, Python, and Ruby run natively on the process backend
  through embedded shims; PHP (and anything else declaring `provided.*`)
  works via the custom-runtime `bootstrap` convention. Dotnet and Java
  (`dotnet8`, `java21`, …) still have no shim, but that's no longer a
  functional gap: a function whose runtime the process backend doesn't
  recognize now falls back to the container backend automatically, with a
  one-line notice, so it always runs — just in a container rather than as a
  bare host process.
- **Fix:** only worth doing for the process backend's own value proposition
  (skip Docker, run on host runtime versions) — add a dotnet/java shim per
  family if demand appears; each is small (~120 lines) against the frozen
  Runtime API. Not required for correctness, since the container fallback
  already covers these runtimes.

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

## Stale pre-M3 process-backend claims in DESIGN.md (doc drift)

- **Where:** `DESIGN.md` — backend-selection list (~line 158) and the
  architecture diagram (~line 74).
- **What:** the selection list checks for a `php` host binary, but PHP
  actually runs via the generic `provided.*`/`bootstrap` convention, not a
  `php`-specific probe; the diagram still says the process backend drives
  "host runtime + official RIC", which M3 replaced with the embedded shims
  (the prose right below it is already amended, the diagram never was).
- **Fix:** drop `php` from the binary-probe list and reword the diagram's
  process-backend box to say shim (or `bootstrap`) instead of RIC — pure
  doc edits, no behavior involved.
