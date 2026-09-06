# Response streaming (`InvokeMode: RESPONSE_STREAM`)

Closes the gap recorded in CURRENT_ISSUES.md as "Streaming responses are relayed
as raw framed bytes". Real Lambda lets a Function URL run in `RESPONSE_STREAM`
invoke mode; the Node runtime's `awslambda.streamifyResponse` then frames its
response as `application/vnd.awslambda.http-integration-response`. Lambdary has
no concept of invoke mode, so those bytes reach the caller unparsed — the
function's real status code and headers are silently discarded and the frame
leaks into the body.

## What was measured (2026-09-06, lambdary 1.0.1, RIE v1.35)

A `nodejs22.x` function wrapped in `awslambda.streamifyResponse`, run on the
container backend against `public.ecr.aws/lambda/nodejs:22`:

- **The `awslambda` global exists and works under RIE.** `streamifyResponse` and
  `HttpResponseStream.from()` are provided by the image's runtime interface
  client; the handler runs and every byte it writes comes back. This is not a
  runtime-support problem.
- **RIE returns the frame through the ordinary buffered invoke endpoint**
  (`POST /2015-03-31/functions/function/invocations`), with
  `Content-Type: application/octet-stream`. Exact bytes:

  ```
  00000040  65 22 3a 22 79 65 73 22  7d 7d 00 00 00 00 00 00  |e":"yes"}}......|
  00000050  00 00 63 68 75 6e 6b 2d  31 3b 63 68 75 6e 6b 2d  |..chunk-1;chunk-|
  ```

  i.e. `<prelude JSON>` + eight `NUL` bytes + `<body>`.
- **On the HTTP route path the frame is served verbatim as JSON.**
  `GET /streamfn` returned `200`, `Content-Type: application/json`, body
  `{"statusCode":200,"headers":{...}}\0\0\0\0\0\0\0\0chunk-1;chunk-2;`. The
  function's own `text/plain` and its `X-Probe` header were lost.

  Cause: `shapedStatusCode` (`internal/event/response.go:38`) `json.Unmarshal`s
  the whole payload; the trailing NULs and body make it invalid JSON, so
  `ToHTTP` falls through to `writeUnshaped` (`:80`) and writes the frame
  unchanged with a default `application/json`.

**Consequence:** this is a correctness bug today, not merely a missing feature.
A developer testing a streaming handler locally gets a wrong response and no
diagnostic.

## Scope: parse the frame, do not chase incremental delivery

Two separable capabilities hide behind the words "response streaming":

1. **Frame-aware responses** — map the prelude to the real HTTP status, headers
   and cookies, and write the body after it. Fixes the bug above. Self-contained,
   no architectural change, works with the pinned RIE.
2. **Incremental delivery** — bytes reaching the client as the function writes
   them, i.e. a genuine time-to-first-byte win.

**Only (1) is in scope here.** RIE buffers the whole invocation before
responding on the endpoint lambdary uses, so (2) cannot be achieved by parsing
alone; it needs a streaming invoke endpoint upstream plus a rewrite of
`doInvoke`/`attempt`/`invoke` off `[]byte`, a re-scoping of the per-instance
invoke lock to the response lifetime, and a "response already committed" guard
that disables the transparent retry in `(*Router).invoke`
(`internal/router/router.go:209`). That is a milestone of its own — see the
closing note.

Doing (1) first is also what makes (2) safe later: the frame parser is the same
either way, and shipping it now means the local response is *correct* even while
it stays buffered.

All units are **sequential** (A → B → C): B consumes A's parser, C documents
what A+B shipped.

## Unit A — frame parser in `internal/event` (sequential, first)

**Files:**
- `internal/event/stream.go` (new)
- `internal/event/stream_test.go` (new)

Add a pure, stdlib-only parser alongside the existing mappers. Follow the
package doc's rule (`internal/event/request.go:1-9`): no HTTP serving of its
own beyond the writer it is handed, no I/O outside the reader.

```go
// Prelude mirrors shapedResponse minus the body: the JSON object a
// streaming handler passes to HttpResponseStream.from().
type Prelude struct {
    StatusCode int                 `json:"statusCode"`
    Headers    map[string]string   `json:"headers"`
    Cookies    []string            `json:"cookies"`
}

// SplitFrame splits an http-integration-response payload into its prelude
// and body at the eight-NUL delimiter.
func SplitFrame(payload []byte) (prelude []byte, body []byte, ok bool)

// ToHTTPStream writes a framed payload as a real HTTP response.
func ToHTTPStream(w http.ResponseWriter, payload []byte) error
```

Details that matter:

- The delimiter is exactly eight `0x00` bytes. Split on the **first**
  occurrence; a body may legitimately contain NULs.
- `ok == false` when there is no delimiter, or the prelude is not a JSON object
  with an integer `statusCode`. Callers then fall back to the buffered path —
  never error the request over it.
- Reuse the existing header/cookie handling rather than duplicating it: the
  `Set-Cookie`-per-cookie loop and the `hasContentType` default in
  `writeShaped` (`internal/event/response.go:88-121`) apply unchanged. Factor
  the shared part out rather than copy it.
- A prelude with no `Content-Type` must **not** inherit `application/json`;
  a streamed body is arbitrary. Default to `application/octet-stream`.
- Empty body after the delimiter is valid — keep the `len(body) > 0` guard from
  `writeShaped` (`:109`) so body-less statuses stay legal (this is what 1.0.1
  fixed for Go 1.26).

**Tests** (table-driven, `t.Run` subtests, matching `response_test.go`): prelude
+ body; prelude + empty body; body containing NUL bytes; no delimiter; prelude
that is not JSON; prelude without `statusCode`; cookies; missing `Content-Type`;
a header whose value contains the delimiter as text.

## Unit B — `url.invoke_mode` and route wiring (after A)

**Files:**
- `internal/manifest/manifest.go` (add field, validation, default)
- `internal/manifest/manifest_test.go`, plus a YAML fixture under `testdata/`
- `internal/router/router.go` (`handleRouteV2`)
- `internal/router/router_test.go`
- `internal/cli/e2e_test.go` (one streaming case)

**Manifest.** Extend the existing `URL` struct (`internal/manifest/manifest.go:79-86`),
keeping AWS's own vocabulary as DESIGN.md's "no proprietary config spec" goal
requires. AWS calls this `InvokeMode` on a Function URL, with `BUFFERED` and
`RESPONSE_STREAM`:

```yaml
url:
  path: /stream
  payload: "2.0"
  invoke_mode: RESPONSE_STREAM   # default BUFFERED
```

- `DefaultInvokeMode = "BUFFERED"`, filled by `ApplyBuiltinDefaults`
  (`:126-130`) next to `DefaultPayload`.
- Validate in `validateCommon` alongside the payload check; an unknown value is
  a manifest error, consistent with `ErrInvalidBackend` (`:26`).
- `Config.ApplyDefaults` should inherit it from `lambdary.yml` `defaults:` the
  same way `payload` is inherited.

**Why explicit rather than sniffed.** RIE labels the framed response
`application/octet-stream`, not the `vnd.awslambda.*` type, so content-type
detection would be guesswork against an undocumented upstream detail. Real
Lambda treats invoke mode as function configuration, not something inferred per
response — matching that is both more faithful and more predictable.

**Routing.** In `handleRouteV2` (`internal/router/router.go:386`), when the
function's invoke mode is `RESPONSE_STREAM`, call `event.ToHTTPStream` instead
of `event.ToHTTP`; if `SplitFrame` reports `ok == false`, fall back to
`event.ToHTTP` so a buffered handler declared as streaming still behaves.

Order matters: the existing `isFunctionError` check (`:412`) must run **first
and unchanged**. An unhandled exception is reported by the runtime as a normal
JSON error payload, not as a frame, so the 502 envelope keeps working exactly
as it does today.

**v1 is out of scope.** `RESPONSE_STREAM` is a Function URL feature; API Gateway
REST payload 1.0 has no equivalent. Declaring both should be a manifest
validation error rather than a silent no-op.

**Passthrough is deliberately untouched.** `handleInvoke`
(`internal/router/router.go:114`) is a raw byte relay that never consults
`internal/event`, and `aws lambda invoke` against real Lambda in streaming mode
returns the frame too. Leaving it verbatim keeps it faithful.

**Tests:** manifest parse/validate/default cases; a router test per branch
(streaming declared + framed payload → parsed; streaming declared + plain
payload → buffered fallback; buffered declared + framed payload → today's
behaviour, proving no silent change); an e2e using a `streamifyResponse`
handler, Docker-gated like the existing `TestE2E`.

## Unit C — docs and changelog (after B)

**Files:** `README.md`, `DESIGN.md`, `CHANGELOG.md`, `CURRENT_ISSUES.md`,
`examples/` (optional).

- **README:** `invoke_mode` in the manifest reference; a short paragraph stating
  what is emulated (prelude → status/headers/cookies) and what is not
  (incremental delivery — the response is still buffered by the emulator).
- **DESIGN:** amend the non-goals line in place, as the layers work did, and add
  a short "Response streaming (post-v0)" section plus a decision-log entry
  recording the two-level split and why explicit `invoke_mode` beat sniffing.
- **CHANGELOG:** one bullet under `## [Unreleased]` → `### Added`, written for
  the user. Mention the correctness fix, since anyone already using
  `streamifyResponse` locally is getting wrong responses today.
- **CURRENT_ISSUES:** replace the streaming entry with a narrower one covering
  only incremental delivery (see below).
- **examples/:** a fourth example is the natural place to demonstrate this, and
  `screenshot-node` is already the binary-response example — a streaming variant
  would exercise it end to end. Optional; skip if it bloats the unit.

## Deferred: incremental delivery

Keep in CURRENT_ISSUES.md after this lands. It needs, in order:

1. Confirmation that the pinned RIE (`internal/rie/rie.go:35`, `v1.35`) serves a
   streaming invoke endpoint at all. Nothing in this repo exercises one, and the
   upstream README documents only `/2015-03-31/functions/function/invocations`.
   **Verify before designing further** — if it does not, the ceiling is upstream,
   not here.
2. `doInvoke` (`internal/router/router.go:282`) returning an `io.ReadCloser`
   instead of `[]byte`, with body ownership moving to the caller (it currently
   closes at `:292`).
3. The invoke lock held until the body is drained, not until `doInvoke` returns
   (`attempt`, `:239-251`) — otherwise a second request reaches an instance
   mid-stream.
4. A committed-response guard disabling the transparent retry (`:209`) once
   bytes are on the wire.
5. `http.Flusher` per chunk — there is currently no `Flusher` use anywhere in
   the repo, so plumbing a reader through is necessary but not sufficient.
6. A timeout model that bounds the function rather than the stream: the current
   `context.WithTimeout` around the whole invoke (`:202`) would kill a long
   response mid-flight.
