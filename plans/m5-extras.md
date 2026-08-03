# M5 — Extras: payload 1.0, Dockerfile functions, lockfile, env_file

Closes four items tracked in `CURRENT_ISSUES.md` / the manifest spec:

1. API Gateway v1 (`payload: "1.0"`) event mapping.
2. `Dockerfile`-marker functions become runnable (build + run).
3. `.lambdary/lock` — image-digest + RIE-tag pinning for reproducibility.
4. `local.env_file` — parsed since M0 but honored by neither backend.

Still deferred after M5 (unchanged in CURRENT_ISSUES or DESIGN): Caddy
TLS/HTTP3, per-function concurrency (N instances), SQS/S3 event fixtures,
RIE release embedding, shims beyond Node/Python.

## Units

### Unit A — `internal/event` v1 mapper  *(parallel with B)*

- Files: `internal/event/request_v1.go`, `internal/event/response_v1.go`
  (or one v1.go pair split as sensible), tests.
- `FromHTTPV1(r *http.Request, routePrefix string) (*RequestV1, error)` —
  API Gateway REST (v1 / `1.0`) event: `resource` "/{proxy+}", `path`
  (prefix-stripped), `httpMethod`, `headers` + `multiValueHeaders`,
  `queryStringParameters` + `multiValueQueryStringParameters` (nil when
  none), `pathParameters` {"proxy": stripped-path-sans-leading-slash} or
  nil, `stageVariables` nil, `requestContext` (accountId "anonymous",
  apiId "lambdary", stage "$default", requestId, identity.sourceIp +
  userAgent, httpMethod, path, protocol, requestTime + requestTimeEpoch),
  `body` (null when empty, string otherwise), `isBase64Encoded` (same
  binary heuristic as v2 — reuse the existing helpers).
- `ToHTTPV1(w, payload []byte) error` — v1 response contract is STRICTER
  than v2: the result MUST be a JSON object with integer `statusCode`
  (proxy-integration rules; there is no "bare JSON → 200" fallback).
  Honor `headers`, `multiValueHeaders` (merge, multi wins additively),
  `body`, `isBase64Encoded`. Malformed (no statusCode / bad base64 /
  non-object) → error (router maps to 502, mirroring API Gateway's own
  "malformed Lambda proxy response").
- Shared internals (base64 heuristic, cookie-free header building, source
  IP) refactored for reuse WITHOUT changing v2 behavior — all existing
  event tests must pass untouched.
- Tests: golden full-event JSON (hooks for id/time), multi-value
  header/query fidelity, prefix stripping + proxy pathParameter, empty-body
  null, response happy/multiValue-merge/base64/malformed table.

### Unit B — Dockerfile functions  *(parallel with A)*

- Files: `internal/backend/container/build.go` (+ container.go touch),
  tests; `internal/discovery` NOT touched (it already marks these).
- In `Start` for a Dockerfile function (empty Runtime + empty Image +
  Dockerfile present — replace the current `ErrDockerfileNotSupported`
  path): `docker build -t lambdary/<name>:local <fnDir>` (via Runner;
  stream/capture output, include tail in error on failure), then run that
  tag with the standard argv (no handler arg; the image's own
  ENTRYPOINT/CMD rule — AWS base-image-derived Dockerfiles bundle the RIE
  and listen on 8080, which is our stated support target; document that
  assumption in the package doc). `local.image` set → still no build
  (override wins, existing behavior).
- Because Start rebuilds every time, hot reload (Manager.Restart → next
  Ensure → Start) picks up Dockerfile/code edits with Docker's build cache
  keeping it cheap. No extra reload machinery.
- Remove the now-closed entry from CURRENT_ISSUES.md (leave the lockfile
  entry for Unit C).
- Tests: fake-runner argv assertions (build then run, tag naming, build
  failure surfaces stderr tail, local.image skips build); integration test
  (Docker-gated like the existing one): testdata Dockerfile function
  `FROM public.ecr.aws/lambda/python:3.13` + COPY handler, start, invoke,
  assert echo, stop, no leftovers, AND a rebuild-picks-up-change pass
  (edit the copied handler, Start again, new behavior).

### Unit C — `.lambdary/lock`  *(after B — same package as B's changes)*

- Files: `internal/lockfile/lockfile.go` + tests;
  `internal/backend/container/container.go` (digest resolution + use);
  `internal/rie/rie.go` (record tag); `internal/cli` wiring; `.gitignore`
  (`!.lambdary/lock` negation so the lock is committable while the rest of
  `.lambdary/` stays ignored); CURRENT_ISSUES.md entry removal.
- Format (YAML, versioned): `version: 1`, `rie: v1.35`,
  `images: {<tag-ref>: <sha256 digest>}`. Location: `<config root>/
  .lambdary/lock` (the dir containing lambdary.yml / the resolved root).
- Behavior: lockfile is best-effort and NEVER blocks a run —
  - container backend: when a lock entry exists for the resolved image tag,
    run by digest (`<repo>@sha256:...`); after a successful start of an
    unlocked tag, resolve the digest (`docker inspect` RepoDigests) and
    record it. Missing/corrupt lock → log to stderr once, proceed unpinned,
    rewrite.
  - rie: record `rie: v1.35` alongside (informational — Resolve already
    pins by constant; the lock documents it per-project).
  - `local.image` overrides and `lambdary/<name>:local` build tags are NOT
    pinned (locally built / explicitly chosen).
- Concurrency: lock updates funneled through one writer (mutex in the
  lockfile type); atomic write (temp + rename).
- Tests: round-trip read/write, corrupt file tolerated with warning,
  digest-used-when-present argv assertion (fake runner), record-after-start
  flow, build-tag/override exclusion. Integration (Docker-gated): first
  start records a real digest; second start's run argv uses `@sha256:`.

### Unit D — `env_file` + changelog + verification  *(after A, B, C)*

- Files: `internal/backend/envfile.go` (shared dotenv-lite parser: KEY=VAL
  lines, `#` comments, blank lines, optional `export ` prefix, single/double
  quote stripping — NO interpolation, document), container + process
  backends load `local.env_file` (path relative to the function dir) and
  merge UNDER manifest `environment` (explicit env wins); missing file when
  explicitly configured → clear error at Start. Tests both backends (argv/
  env assertions) + parser table.
- Router: `payload: "1.0"` selection — replace the 501 branch with
  FromHTTPV1/ToHTTPV1 (Unit A's API), same error taxonomy; extend router
  tests (v1 event arrives correctly, v1 strict malformed-response → 502);
  remove the CURRENT_ISSUES payload-1.0 entry and the README "not supported
  yet" line for both closed items (README: drop payload-1.0 + Dockerfile
  from limitations).
- E2E additions (Docker-gated): a `payload: "1.0"` python fixture asserting
  v1 shape end to end. Manual verification: dev with a Dockerfile fixture +
  curl; show lock file created with a digest; env_file demo.
- CHANGELOG [Unreleased]: Added — API Gateway v1 (`1.0`) payload support
  selected per function via `url.payload`; `Dockerfile` functions built and
  run automatically (rebuilt on reload); `.lambdary/lock` pinning image
  digests for reproducible starts; `local.env_file` loading. Changed/Fixed
  as appropriate.

## Sequencing

```
A (event v1) ─────────────┐
B (Dockerfile functions) ─┼─▶ C (lockfile) ─▶ D (env_file + router v1 + docs)
```
A ∥ B first; C after B; D last.
