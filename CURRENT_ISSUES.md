# Current issues

Discovered during orchestrated work; routed here instead of fixed inline.

## RIE internal Runtime API port defaults to 9001 — collision hazard for M3

- **Where:** future `internal/rie` / process backend (M3); upstream reference:
  `internal/lambda/rapidcore/sandbox_builder.go` in
  aws-lambda-runtime-interface-emulator v1.35. Details in
  `plans/rie-darwin-spike.md`.
- **What:** the RIE's *internal* Runtime API server defaults to port 9001. If
  the port Lambdary assigns for the public invoke endpoint
  (`--runtime-interface-emulator-address`) is also 9001, the RIE panics at
  startup with "address already in use".
- **Fix:** when the M3 process backend allocates per-function ephemeral ports,
  explicitly set both the invoke address and the internal runtime API address
  (or exclude 9001 from the allocation pool) so the two can never collide.

## Image-digest lockfile not yet implemented (deferred from M1)

- **Where:** future `.lambdary/lock` handling; decisions log in `DESIGN.md`.
- **What:** the decisions log commits to pinning container image digests, RIC
  versions, and the vendored RIE tag in `.lambdary/lock`. M1 pulls base
  images by tag only.
- **Fix:** after M1's pull/resolve flow settles, record resolved digests on
  first pull and prefer digest pins on subsequent runs.

## Dockerfile-marker functions not runnable yet (deferred from M1)

- **Where:** `internal/backend/container` (M1); discovery already detects
  `Dockerfile` markers.
- **What:** functions whose marker is a `Dockerfile` need a `docker build`
  step before `run`; M1 returns a "not yet supported" error for them.
- **Fix:** add a build step (tag `lambdary/<function>`, rebuild on change
  once M4 watching lands) and then treat the built tag as the run image.

## API Gateway v1 (`payload: "1.0"`) events not supported (deferred from M2)

- **Where:** `internal/event` / `internal/router`; manifest `url.payload`.
- **What:** M2 implements the Function URL / API Gateway v2 (`2.0`) event
  format only; functions configured with `url.payload: "1.0"` get a 501.
- **Fix:** add a v1 request/response mapper in `internal/event` selected by
  the manifest payload field.
