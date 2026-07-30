# M2 — HTTP events

Milestone M2 from `DESIGN.md`: Function URL / API Gateway v2 (`2.0`) event
mapping in both directions, replacing the router's `501` stub so functions are
browser-usable at `localhost:{port}/{route}`.

State going in (M1 landed): `internal/router` serves the invoke passthrough
and answers function-route hits with `501`; per-function serialization, lazy
start, and collision 409s all live in the router/manager; container backend
verified end-to-end against real Docker.

## Scope decisions

- **`2.0` payloads only.** Manifest `url.payload: "1.0"` (API Gateway REST v1
  format) is NOT implemented in M2 — a function configured with `1.0` gets a
  clear `501` naming the limitation. Deferred entry goes to
  `CURRENT_ISSUES.md`.
- Event shape is the **Lambda Function URL** flavor of the v2 payload
  (`routeKey`/`stage` `$default`, `requestContext.accountId` `"anonymous"`),
  since DESIGN.md models local routes on Function URLs.
- The function's route prefix is **stripped** from `rawPath` (DESIGN.md
  routing section): request `/hello/users?x=1` for route `/hello` → event
  `rawPath: "/users"`; bare `/hello` → `rawPath: "/"`.

## Conventions

Same as M1: stdlib only, table-driven tests, `%w` wrapping, gofmt clean,
verify with `go build ./... && go vet ./... && go test ./...`. Report — don't
fix — out-of-scope findings.

## Units

### Unit A — `internal/event` package  *(first)*

Pure request/response mapping, no HTTP serving, maximum testability.

- Files: `internal/event/request.go`, `internal/event/response.go`,
  `internal/event/event_test.go` (split as sensible).
- `FromHTTP(r *http.Request, routePrefix string) (*RequestV2, error)` builds
  the v2 event struct (JSON-tagged, marshals to the exact AWS shape):
  - `version` "2.0", `routeKey` "$default", `rawPath` (prefix-stripped,
    always leading "/"), `rawQueryString` (r.URL.RawQuery verbatim),
  - `cookies`: split from `Cookie` header (one string per cookie pair);
    `Cookie` header itself EXCLUDED from `headers`,
  - `headers`: lowercased keys, multi-values joined with ",",
  - `queryStringParameters`: decoded, multi-values joined with ",", omitted
    when none,
  - `requestContext`: `accountId` "anonymous", `apiId`/`domainName` local
    placeholders ("lambdary"/"localhost"), `domainPrefix`, `http` {method,
    path (the FULL original path, unstripped, per AWS semantics),
    protocol (r.Proto), sourceIp (r.RemoteAddr host part), userAgent},
    `requestId` (crypto/rand-derived), `routeKey`/`stage` "$default",
    `time` (RFC3339-ish `02/Jan/2006:15:04:05 -0700`) + `timeEpoch` ms,
  - `body` + `isBase64Encoded`: read body; base64-encode iff content-type is
    not textual (allowlist: `text/*`, `application/json`, `application/xml`,
    `application/x-www-form-urlencoded`, `application/javascript`, suffixes
    `+json`/`+xml`) OR bytes are not valid UTF-8; empty body → empty string,
    flag false.
- `ToHTTP(w http.ResponseWriter, payload []byte) error` interprets the raw
  invoke result per **Function URL response rules**:
  - JSON object containing an integer `statusCode` → shaped response: apply
    `headers` (default content-type `application/json` when absent),
    `cookies` (one `Set-Cookie` each), decode `body` per `isBase64Encoded`,
    write status + body.
  - Anything else (non-object JSON, object without `statusCode`, invalid
    JSON string payload) → `200`, `Content-Type: application/json`, payload
    written as-is (AWS serializes the return value as JSON; the emulator
    already gives us serialized JSON bytes).
  - Malformed shaped responses (e.g. bad base64) → error (caller turns it
    into a 502).
- Tests: golden-style JSON assertions for the full event (stable requestId/
  time injected via a nowFunc/idFunc hook), prefix stripping cases, cookie
  splitting, multi-value joins, base64 in/out (binary POST, binary shaped
  response), each response rule branch, bad-base64 error.

### Unit B — Router wiring  *(after A)*

- Files: `internal/router/router.go` (replace the 501 branch),
  `internal/router/router_test.go` (extend), possibly `manager.go` untouched.
- Function-route requests (`fn.Route` exact or prefix + "/"): keep the 409
  collision branch; payload version check — `Manifest.URL.Payload` other
  than ""/"2.0" → 501 naming the `1.0` limitation; otherwise: Ensure +
  per-function lock + timeout exactly like the passthrough (factor the
  shared ensure/lock/POST/timeout plumbing into one helper used by both
  paths), build event via `event.FromHTTP`, POST marshaled event to the
  instance's InvokeURL, then `event.ToHTTP` the result. Emulator-level
  invoke errors (non-2xx from RIE, e.g. handler exception envelope
  `{"errorType":...}`) → 502 JSON carrying the error payload. Timeout → 504
  (existing semantics).
- Tests (fake instance echoing the received event JSON): a GET with query +
  cookies + custom headers arrives as a correct v2 event (assert key fields
  incl. stripped rawPath, unstripped requestContext.http.path); shaped
  response honored (status/headers/cookies/base64 body); unshaped response
  → 200 application/json; binary request base64 round trip; `payload: "1.0"`
  → 501; handler-error envelope → 502; route collision still 409; deep
  nested path + root path cases.

### Unit C — End-to-end verification + changelog  *(after B)*

- Files: `internal/cli/e2e_test.go` (Docker-gated like Unit B M1's
  integration test: skip on `-short` or daemon down), `CHANGELOG.md`.
- E2E test: start the real stack (discover the cli testdata demo fixture →
  container backend → manager → router on an httptest server); update the
  fixture handler (or add a second fixture function) to return a SHAPED
  response for one path (statusCode 201, custom header, cookie) and echo the
  event for another; assert: browser-style `GET /hello/users?x=1` returns
  the echo with correct rawPath/query; shaped path returns 201 + header +
  Set-Cookie; passthrough endpoint still works. Clean up containers
  (t.Cleanup) and assert none remain.
- Manual verification: `go run ./cmd/lambdary dev` against the demo fixture,
  real curl of `GET /hello?x=1` captured in the report.
- CHANGELOG `### Added`: functions reachable as plain HTTP endpoints under
  their local route — requests arrive as AWS Function URL / API Gateway
  v2 (`2.0`) events with headers/cookies/query/binary bodies mapped both
  ways, shaped and unshaped handler responses honored. `### Changed`: the
  former 501 hint on function routes is gone (now real responses).
  Note the `1.0` payload limitation honestly under Added text or omit.

## Sequencing

A → B → C, strictly sequential (each builds on the previous API).
