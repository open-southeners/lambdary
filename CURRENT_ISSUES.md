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
