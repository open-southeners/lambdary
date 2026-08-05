# Ruby shim + container fallback for unsupported process-backend runtimes

Two related capabilities, closing part of CURRENT_ISSUES.md's "Process backend
limited to Node, Python, and custom runtimes":

1. A Ruby shim so `ruby*` runtimes run natively on the process backend, same
   pattern as the existing Node/Python shims.
2. Per-function fallback to the container backend when a function's runtime
   family has no shim (java, dotnet, …), instead of `ErrRuntimeNotSupported`
   killing the invoke. AWS base images ship real runtime interface clients,
   so containers cover every official runtime with zero shims.

All units are **sequential** (A → B → C): A and B both touch
`internal/backend/process/runtime.go`; C documents what A+B shipped.

## Unit A — Ruby shim (sequential, first)

**Files:**
- `internal/backend/process/shims/bootstrap.rb` (new)
- `internal/backend/process/shims.go` (add embed + name + hash + write)
- `internal/backend/process/runtime.go` (add `ruby*` case to `runtimeCommand`)
- `internal/backend/process/testdata/ruby/handler.rb` (new)
- `internal/backend/process/shim_contract_test.go` (add `TestRubyShim`)
- `internal/backend/process/shims_test.go`, `runtime_test.go` (extend)

**bootstrap.rb** mirrors `bootstrap.py` one-for-one (read its header comment
and structure first):
- Ruby stdlib only: `net/http`, `json`, `uri`. No gems.
- `ruby bootstrap.rb <handler>`; reads `AWS_LAMBDA_RUNTIME_API` (host:port,
  no scheme — the RIE sets it). Missing env/arg → message on stderr, exit 1.
- Handler notation is AWS Ruby's own: first `.` splits the require file from
  the rest; the last `.` of the rest splits an optional class/module const
  path (may contain `::`, resolved via `Object.const_get`) from the method
  name. `func.handler` → `require` `func.rb` from cwd (the function dir —
  spawn sets cwd), call top-level `handler`; `source.MyModule::Handler.process`
  → `Object.const_get("MyModule::Handler").process`. AWS Ruby handlers take
  **keyword args**: `handler(event:, context:)`.
- Context object: `aws_request_id`, `function_name` (from
  `AWS_LAMBDA_FUNCTION_NAME`), `get_remaining_time_in_millis` (from the
  `Lambda-Runtime-Deadline-Ms` response header) — same trio as the other shims.
- Loop: GET `.../2018-06-01/runtime/invocation/next` (no read timeout — it's
  a long poll; make sure Net::HTTP's default read_timeout of 60s is raised or
  disabled, mirroring the delayed-next contract-test case), POST result JSON
  to `.../invocation/<id>/response`, exceptions to `.../invocation/<id>/error`
  as `{errorMessage, errorType, stackTrace}`, load failures to
  `.../runtime/init/error` + exit 1.
- Target compatibility: Ruby >= 2.6 syntax (macOS system ruby) — no
  3.x-only syntax.

**shims.go:** add `rubyShimName = "bootstrap.rb"`, `//go:embed`, include in
`computeShimsHash` and `writeShims`/`shimsPresent`. Keep hash-input order
deterministic (node, python, ruby).

**runtime.go:** add `case strings.HasPrefix(fn.Runtime, "ruby")` returning
`"ruby", []string{filepath.Join(shimDir, rubyShimName), fn.Handler}` —
update the resolution-order doc comment.

**Tests:** `testdata/ruby/handler.rb` defines an echo handler (returns
`{echoed:, requestId: context.aws_request_id, functionName: context.function_name}`)
and a raising handler — mirror `testdata/node/handler.mjs` /
`testdata/python/handler.py`. `TestRubyShim` mirrors `TestNodeShim`'s cases
(happy path, handler exception → error endpoint with errorType, bad handler →
init/error + exit 1, delayed `next`) using `requireBin(t, "ruby")`. Extend
`runtime_test.go` (`ruby3.3` → ruby command) and `shims_test.go` (three files
written; hash covers all three).

**Verify:** `go build ./... && go test ./internal/backend/process/`

## Unit B — container fallback for unsupported runtimes (after A)

**Files:**
- `internal/backend/process/runtime.go` (export `Supports`)
- `internal/cli/backend.go` (return a machine-readable kind)
- `internal/cli/backend_resolver.go` (fallback in per-function resolve)
- `internal/cli/invoke.go` (same fallback on the standalone path)
- matching `_test.go` files in `internal/cli`

**process.Supports:** `func Supports(fn discovery.Function) bool` — true when
the manifest sets `local.command`, or `fn.Runtime` has one of the prefixes
`runtimeCommand` handles (`nodejs`, `python`, `ruby`, `provided`). Refactor so
`Supports` and `runtimeCommand`'s switch share one prefix list — they must
not drift. Note: `provided.*` counts as supported even if `bootstrap` is
missing; `ErrBootstrapMissing` stays a hard error, not a fallback trigger
(the container backend needs the same bootstrap file anyway).

**Kind plumbing:** `resolveBackend`, `resolveContainerBackend`,
`resolveProcessBackend`, `resolveAutoBackend` additionally return a kind
constant (`"container"` / `"process"`) alongside the display name, so callers
know what "auto" picked without parsing the display string. Update all call
sites (`dev.go`, `invoke.go`, `backend_resolver.go`, tests).

**Fallback in `perFunctionBackend.resolve`:** once the target backend for fn
is known to be the process kind and `!process.Supports(fn)`:
- resolve the container backend via the existing `resolveContainerOnce`
  (cached); on success, print one notice per function to errW (add an errW
  field), e.g.
  `function <name>: runtime <runtime> has no process-backend shim — running in a container`,
  and return the container backend. Print the notice only once per function
  (guard with a map), since resolve runs on every cold start.
- if container resolution fails, return an error that names both facts:
  the runtime isn't supported by the process backend **and** the container
  fallback is unavailable (wrap the container error; keep
  `process.ErrRuntimeNotSupported` matchable with `errors.Is`).

**Standalone invoke (`invokeStandalone`):** after `resolveBackend`, if the
kind is process and `!process.Supports(fn)`, resolve the container backend
(same notice to errOut, same combined error when unavailable) before
`Start`.

**Tests (fakes already exist in cli tests — follow their style):**
- per-function resolver: global process + unsupported runtime (`java21`) →
  container backend returned + notice written; container resolution failing →
  combined error, `errors.Is(err, process.ErrRuntimeNotSupported)`;
  supported runtime (`nodejs22.x`, and a `local.command` function) → no
  fallback, no notice; notice printed once across two resolves.
- standalone path: same three shapes at whatever seam invoke's tests use.

**Verify:** `go build ./... && go test ./internal/... && go vet ./...`

## Unit C — docs + changelog (after B)

**Files:** `CHANGELOG.md`, `README.md`, `DESIGN.md`, `CURRENT_ISSUES.md`

- `CHANGELOG.md`: the 1.0.0 release is not tagged yet — append both entries
  at the **end of the `### Added` list inside `## [1.0.0]`** (NOT under
  Unreleased). Product-voice, per the changelog conventions in the user's
  global CLAUDE.md: one bullet for Ruby on the process backend (dependency-free
  embedded shim, `ruby*` runtimes, AWS handler notation incl.
  `file.Class.method`), one for the automatic per-function container fallback
  (what it does for the reader: every official runtime now runs even without
  a shim; notice on stderr; only errors when no container runtime exists).
- `README.md` (~line 50): "The process backend supports Node, Python, and
  custom runtimes" → add Ruby; add a sentence on the automatic container
  fallback for families without a shim (java, dotnet).
- `DESIGN.md` line ~191 "per language family (Node, Python)" → include Ruby;
  scan for other Node/Python-only claims about the process backend.
- `CURRENT_ISSUES.md`: rewrite the "Process backend limited to Node, Python,
  and custom runtimes (M3)" entry: ruby shipped, fallback shipped; remaining
  work is dotnet/java shims (now optional perf work, not a functional gap).

**Verify:** none beyond reading; keep formatting consistent.
