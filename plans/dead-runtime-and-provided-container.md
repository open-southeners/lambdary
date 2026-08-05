# Fix: dead runtimes never replaced + provided.* broken on the container backend

Resolves two CURRENT_ISSUES.md entries:

1. **A dead process-backend runtime is never retried** — `Manager.Ensure`
   returns cached instances with no liveness signal; a runtime-side crash
   (RIE child dies, RIE keeps answering `Runtime.ExitError`) bricks the
   function until a code-change restart. Fix: evict the cached instance when
   an invocation reveals it dead, so the next invoke cold-starts — real
   Lambda's replace-on-crash behavior. Key constraint: the RIE process (and
   container) stay *alive* when only the runtime child dies, so detection
   must key off the invoke outcome, not process liveness.
2. **`provided.*` on the container backend cannot work without a
   Dockerfile** — `runArgs` passes no CMD when `handler` is unset (always,
   for `provided.*`), so the AWS base image's entrypoint exits immediately;
   and its `RUNTIME_ENTRYPOINT` is hardcoded to `/var/runtime/bootstrap`,
   never `/var/task/bootstrap`. Fix: placeholder CMD + bind-mount the
   function's `bootstrap` to `/var/runtime/bootstrap`.

Verification for every unit: `go build ./... && go test ./...` from the repo
root (plus `gofmt -l .` staying empty). No new dependencies.

## Unit A — `Manager.Discard` (independent)

**Files:** `internal/router/manager.go`, `internal/router/manager_test.go`.

Add to `Manager`:

```go
// Discard stops and forgets name's cached instance, but only if it is
// still the one whose InvokeURL is invokeURL — the identity check keeps a
// caller that just watched an instance die from evicting a healthy
// replacement some other request already started.
func (m *Manager) Discard(ctx context.Context, name, invokeURL string) error
```

Semantics, mirroring `Restart`'s structure except for the invoke lock:

- Unknown name → `ErrUnknownFunction` (same wrapping as Restart/Ensure).
- Take the **start guard only** (`m.namedMutex(m.starts, name)`), NOT the
  invoke lock: the instance is presumed dead, and blocking behind a doomed
  in-flight invoke (up to 30s) would serialize recovery for nothing. Callers
  invoke Discard *after* releasing their own WithLock.
- Under `instMu`: if `m.instances[name]` exists AND its `InvokeURL() ==
  invokeURL`, delete it; otherwise return nil without touching anything
  (someone already evicted/replaced it).
- Stop the evicted instance; wrap a Stop error `"router: discarding %s: %w"`
  (callers treat it as best-effort — the backing process/container is
  usually already gone, and e.g. `docker stop` on an already-removed
  `--rm` container errors harmlessly).

**Tests** (conventions: existing fakes in `manager_test.go`):
- Discard with matching URL → instance stopped, next Ensure starts a second
  instance (backend Start call count == 2).
- Discard with stale URL (instance replaced meanwhile) → no-op, cached
  instance untouched.
- Discard unknown name → `errors.Is(err, ErrUnknownFunction)`.
- Discard when nothing cached → nil, no Stop calls.

## Unit B — router-side dead-runtime detection (sequential, after A)

**Files:** `internal/router/router.go`, `internal/router/router_test.go`.

In `(*Router).invoke`, after the `WithLock` block returns:

1. **Transport-level failure** (`invokeErr != nil` and NOT deadline/cancel:
   `!errors.Is(invokeErr, context.DeadlineExceeded) &&
   !errors.Is(invokeErr, context.Canceled) && ctx.Err() == nil`): the
   request never reached a runtime. `mgr.Discard(ctx, name, invokeURL)`
   (ignore its error), then **retry exactly once**: re-`Ensure` (cold-starts
   a fresh instance) and re-run the WithLock/doInvoke step. A single retry is
   safe here — nothing executed — and mirrors real Lambda, which never routes
   an invoke to a dead environment. If the retry's Ensure or invoke fails,
   return that error via `newInvokeError` as today. No second retry.
2. **`Runtime.ExitError` envelope** (invoke "succeeded" at the HTTP layer
   but the body is an error envelope whose `errorType` is
   `"Runtime.ExitError"`, any HTTP status): the runtime died. Call
   `mgr.Discard(ctx, name, invokeURL)` (ignore error) and return the
   response to the caller **unchanged, no retry** — the handler may have
   been mid-execution, so replaying risks double side effects; real Lambda
   returns the error and replaces the environment for the *next* invoke.
3. Timeouts (`invokeError.timedOut` conditions) must NOT evict — a slow
   function is not a dead one.

Add a helper next to `isErrorEnvelope`:

```go
// isRuntimeExit reports whether body is a Lambda error envelope whose
// errorType is Runtime.ExitError — the RIE's signal that the runtime
// process behind this instance has exited and every future invoke against
// it is doomed.
func isRuntimeExit(body []byte) bool
```

(JSON-parse `errorType` as a string; false on any parse miss.)

Structure suggestion: extract the `Ensure + WithLock(doInvoke)` sequence
into a small unexported method so `invoke` can run it, inspect the outcome,
and run it a second time for the transport-retry case without duplicating
the plumbing. Keep the invoke lock held only *during* each attempt, never
across the Discard.

**Tests** (conventions: `newFakeInstance`/fake backends in
`router_test.go`):
- Invoke returns a `Runtime.ExitError` envelope → response relayed verbatim
  to the caller, AND a subsequent invoke gets a fresh instance (fake backend
  Start count == 2).
- First invoke hits a connection-refused URL (e.g. fake instance whose
  InvokeURL points at a closed port / a `httptest.Server` already closed),
  fresh instance succeeds → caller sees the *success* (transparent retry),
  Start count == 2.
- Both attempts connection-refused → 502, and only one retry happened.
- Function timeout → 504 as today and NO eviction (Start count stays 1).
- Ordinary handler error envelope (e.g. `errorType: "ValueError"`, as in the
  existing test at router_test.go:409) → NO eviction.

## Unit C — `provided.*` on the container backend (independent)

**Files:** `internal/backend/container/argv.go`,
`internal/backend/container/argv_test.go`,
`internal/backend/container/container.go`,
`internal/backend/container/container_test.go`.

`argv.go` (`runArgs` stays pure — no I/O):

- When `strings.HasPrefix(fn.Runtime, "provided.")`:
  - add a second bind mount, `-v <fnDir>/bootstrap:/var/runtime/bootstrap:ro`,
    immediately after the existing `/var/task` mount — AWS's provided base
    images hardcode `RUNTIME_ENTRYPOINT=/var/runtime/bootstrap` (empty in
    the image), so this is the only path the entrypoint will exec;
  - after the image, when `fn.Handler == ""`, append the placeholder CMD
    arg `"bootstrap"` — the entrypoint requires *a* handler argument (it
    becomes `_HANDLER`, which custom runtimes are free to ignore) and
    exits immediately without one.
- Applies regardless of a `local.image` override (a custom provided-family
  image mimics the AWS base); Dockerfile-marker functions are unaffected
  because they always have `Runtime == ""` (see `isDockerfileFunction`).

`container.go` (`Start`, which does the I/O):

- Before building `runArgs`, when `fn.Runtime` is `provided.*`, `os.Stat`
  `<absDir>/bootstrap`; if missing, fail with a clear error, e.g.
  `provided.* on the container backend needs a bootstrap file in the
  function directory (or a function-owned Dockerfile)` — Docker would
  otherwise auto-create a *directory* at the mount source and the container
  would fail obscurely.

**Tests:**
- `argv_test.go`: a `provided.al2023` function with no handler → argv
  contains both mounts in order and trailing `"bootstrap"` CMD; a
  `nodejs22.x` function → argv unchanged from today (no new mount, handler
  CMD as before).
- `container_test.go`: Start of a provided.* function whose dir lacks
  `bootstrap` → error naming bootstrap, and the fake runner saw no `run`
  call; with a `bootstrap` file present (t.TempDir) → run argv includes the
  new mount + CMD.

## Unit D — docs, changelog, issue log (sequential, after A–C)

**Files:** `CURRENT_ISSUES.md`, `CHANGELOG.md`, `README.md`,
`examples/checksum-go/Dockerfile` (comment only), `examples/README.md` (if
it mentions the limitation).

- Remove the two resolved entries from `CURRENT_ISSUES.md` ("A dead
  process-backend runtime is never retried…" and "`provided.*` on the
  container backend cannot work without a Dockerfile"). Leave every other
  entry untouched.
- `CHANGELOG.md` under `## [Unreleased]` → `### Fixed`, product-voice per
  repo convention (outcome, not diff), e.g.:
  - crashed function runtimes are now replaced on the next invocation
    instead of failing forever (`Runtime.ExitError` / connection failures
    evict the dead instance; connection failures are retried once
    transparently);
  - `provided.*` functions now run on the container backend without a
    `Dockerfile` — the function's `bootstrap` is mounted where the AWS base
    image expects it.
- `README.md`: fix any claim that `provided.*` on the container backend
  requires a `Dockerfile` (grep for it); the backend table / custom-runtime
  notes should now say a `bootstrap` file in the function directory is
  enough (compiled runtimes still need the binary built — that's the
  separate "no build step" issue, which stays open).
- `examples/checksum-go/Dockerfile`: soften the comment claiming the
  entrypoint can *never* find the function's bootstrap — the Dockerfile is
  still the right choice there (it compiles the Go binary), but the stated
  reason changes.
