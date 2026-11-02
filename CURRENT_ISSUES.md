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

## No incremental delivery for streaming responses

- **Where:** `internal/router/router.go` (`invoke`/`attempt`/`doInvoke`),
  `internal/backend`.
- **What:** `url.invoke_mode: RESPONSE_STREAM` (`internal/event/stream.go`,
  `handleRouteV2`) now parses a `streamifyResponse` handler's
  `http-integration-response` frame into its real status code, headers,
  cookies, and body, so the response itself is correct. It is still not
  *incremental*: the whole invocation is buffered — `io.ReadAll` at
  `internal/router/router.go:301` — and there is no `http.Flusher` use
  anywhere in the repo, so a streaming handler gets no time-to-first-byte
  benefit locally. Several consumers need the full body before anything is
  written (`isRuntimeExit`, `isFunctionError`, `shapedStatusCode`, and now
  `event.SplitFrame`), and the transparent retry in `invoke` assumes nothing
  has been sent yet.
- **Fix:** blocked on confirming the pinned RIE (`internal/rie/rie.go`,
  `v1.35`) serves a streaming invoke endpoint at all — upstream documents
  only `/2015-03-31/functions/function/invocations`; verify this before
  designing further, since if it doesn't the ceiling is upstream, not here.
  If it does, in order: `doInvoke` (`:289`) returning an `io.ReadCloser`
  instead of `[]byte`, with body ownership moving to the caller (it
  currently closes at `:299`); the per-instance invoke lock held until the
  body is drained, not until `doInvoke` returns (`attempt`, `:246-260`); a
  committed-response guard disabling the transparent retry in `invoke`
  (`:216`) once bytes are on the wire; `http.Flusher` per chunk; and a
  timeout model that bounds the function rather than the stream, since the
  current `context.WithTimeout` around the whole invoke (`:209`) would kill
  a long response mid-flight. Sized as its own milestone — see "Deferred:
  incremental delivery" in `plans/response-streaming.md`.

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

## .NET functions are not built before container start (DESIGN gap)

- **Where:** `internal/backend/container`.
- **What:** DESIGN's "compiled ones rebuild the artifact first" now holds for
  `Dockerfile` functions and Go functions (`internal/gobuild`, run on every
  backend start), but a bare `*.csproj` (`dotnet8`) function still has no
  compile-the-artifact step — the container runs whatever the developer last
  published by hand.
- **Fix:** a `dotnet publish` hook alongside `internal/gobuild`, or document
  that .NET functions need a `Dockerfile` or a manual publish.

## Go builds need a host Go toolchain

- **Where:** `internal/gobuild` (`Build`).
- **What:** Go functions are built with the host's `go` for both backends;
  with no `go` on `PATH`, every Go function fails to start (the error names
  the failed `env … go build` command).
- **Fix:** fall back to building inside a `golang` container when the
  container backend is in use and the host has no toolchain.

## A published Go version is tied to the backend that built it

- **Where:** `internal/versions/publish.go` (`snapshot`),
  `internal/router/manager.go` (`BuildBootstrap`).
- **What:** a version freezes one bootstrap, built for the backend that would
  run the function at publish time. Where host and container differ (macOS, or
  a container architecture other than the host's), changing `--backend` or
  `local.backend` afterwards runs a binary built for the wrong platform, and
  the failure only appears at invoke time.
- **Fix:** record the target OS and architecture in the version and fail with a
  clear error on a mismatch, or build a bootstrap for each target at publish.

## Version snapshots and instances are never cleaned up

- **Where:** `internal/versions`, `internal/router/manager.go`.
- **What:** there is no way to delete a version, so snapshots (full copies,
  including `node_modules`) accumulate under `.lambdary/versions/`. A version's
  instance also keeps running until the dev server stops or reconfigures, so
  each version invoked at least once holds a container or process.
- **Fix:** add `DeleteFunction` with a qualifier, and stop version instances
  after an idle period.

## Merged manifest defaults are never re-validated

- **Where:** `internal/discovery/discovery.go` (`discoverFunction`, the
  `manifest.Load` → `cfg.ApplyDefaults` → `ApplyBuiltinDefaults` sequence),
  `internal/manifest/manifest.go` (`validateCommon`).
- **What:** validation runs against each document as written — the function's
  own `.lambda.yml` inside `Load`, and `lambdary.yml`'s `defaults:` block
  inside `Config.validate` — but never against the *merged* result. A
  cross-field rule can therefore be satisfied by both documents individually
  and still be violated after inheritance. Surfaced by the new
  `ErrStreamingPayloadV1` check: a manifest setting only
  `url.invoke_mode: RESPONSE_STREAM` that inherits `url.payload: "1.0"` from
  the project defaults escapes it, and the router then serves that function
  through `handleRouteV1`, where invoke mode is never consulted — the exact
  silent no-op the validation was added to prevent. Any future cross-field
  rule inherits the same hole.
- **Fix:** re-run `validateCommon` on the merged manifest after
  `ApplyDefaults`/`ApplyBuiltinDefaults`, reporting a failure through
  `discoverFunction`'s existing `Warnings` channel rather than as a hard
  error — matching how a failed `manifest.Load` is already surfaced, and
  keeping discovery fail-safe per DESIGN.md's route-collision decision.
