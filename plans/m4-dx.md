# M4 — Developer experience

Milestone M4 from `DESIGN.md`: hot reload, log multiplexing, `lambdary init`
scaffolding. (`--eager` landed in M1.)

State going in: M1–M3 complete — container + process backends, router with
v2 event mapping, `dev`/`invoke`/`list`. The RIE already emits Lambda-style
`REPORT` lines per invocation in both backends (spike-confirmed), so log
*multiplexing* is what's missing, not log *formatting*. Instances expose
`Logs() io.Reader` (container: `docker logs -f`; process: combined output
buffer with non-blocking multi-read).

## Scope decisions

- New dependency allowed: `github.com/fsnotify/fsnotify` (named in DESIGN.md
  key dependencies). Nothing else.
- Reload semantics (pragmatic, matches lazy-start philosophy):
  - **Code change** inside a function dir → stop that function's running
    instance (if any); the next request cold-starts it fresh. Under
    `--eager`, restart immediately after stopping.
  - **Config/structure change** (`.lambda.yml`, root `lambdary.yml`, a
    function dir added/removed) → full reload: stop all instances,
    re-discover, rebuild manager+router, atomically swap the HTTP handler.
  - Every reload action logs one clear line to stderr (what changed → what
    was done).
- Log streaming: when an instance starts, its `Logs()` stream is copied to
  Lambdary's stderr, each line prefixed `[<function>] ` (ANSI color per
  function when stderr is a TTY, plain otherwise; NO_COLOR respected).
  Startup/shutdown notices keep their current format.

## Units

### Unit A — `internal/watcher`  *(parallel with C)*

- Files: `internal/watcher/watcher.go`, `internal/watcher/watcher_test.go`.
- API:
  - `type Event struct { Function string; Structural bool; Paths []string }`
    — `Structural=true` means re-discovery is needed (manifest/root config/
    dir add-remove); otherwise `Function` names the function whose code
    changed.
  - `func Watch(ctx context.Context, root string, fns []discovery.Function)
    (<-chan Event, error)` — watches the root dir (for dir add/remove and
    `lambdary.yml`) and each function dir recursively (fsnotify is
    non-recursive: walk subdirs and add them; add newly created subdirs on
    the fly). 300ms debounce: events within the window coalesce into one
    Event (structural wins over code-change; multiple functions changed in
    one window → emit one event per function, or one structural if mixed
    with structural). Closes the channel when ctx ends.
  - Classification: path is root `lambdary.yml` or any `.lambda.yml`, or a
    create/remove/rename of a direct child dir of root → Structural. Else if
    under a function dir → that function's code change. Editor noise
    (`.swp`, `~`, `.tmp`, `4913`, hidden dotfiles) ignored.
- Tests: real filesystem (t.TempDir + real fsnotify): code-change event for
  the right function; manifest edit → structural; new dir at root →
  structural; debounce coalescing (burst of writes → one event); nested
  subdir file change detected; editor-noise ignored; ctx cancel closes
  channel. Use generous poll deadlines (macOS FSEvents/kqueue latency).

### Unit B — dev wiring: reload + log streaming  *(after A)*

- Files: `internal/router/manager.go` (add `Restart(ctx, name) error` — stop
  + forget the instance so the next Ensure cold-starts; no-op if not
  running; also export a way to iterate/`Started()` names if needed),
  `internal/cli/dev.go` (+ maybe `internal/cli/logs.go` helper), tests,
  `internal/cli/e2e_test.go` (extend), `CHANGELOG.md`.
- dev flow additions:
  - After discovery, start `watcher.Watch`; consume events in a goroutine:
    code change → `mgr.Restart` (+ immediate re-`Ensure` under `--eager`),
    log `reloaded <name> (<path> changed)`; structural → stop all,
    re-resolve config + re-discover, rebuild manager+router, swap via
    `atomic.Pointer[http.Handler]`-style indirection (outer handler
    delegates to the current inner), log `reconfigured: <n> functions`.
    Discovery errors during reload: log and KEEP the old state (never kill
    the dev server on a broken edit — fail-safe rule).
  - Log streaming: wrap Manager or hook instance start so each started
    instance's `Logs()` is pumped line-by-line to stderr prefixed
    `[<name>] ` with per-function color (small palette cycling; TTY +
    NO_COLOR detection). Pump goroutine exits when the reader closes
    (instance stopped).
  - `--no-reload` flag to disable watching (default on).
- Tests: Manager.Restart unit tests (running → stopped+forgotten, not
  running → no-op, restart after failed start); handler-swap indirection
  test (requests hit new function set after swap); log-prefix writer unit
  test (line splitting, partial lines, color off). E2E (extend, Docker-free
  process backend for speed): start dev-equivalent stack on the demo
  fixtures copied into a t.TempDir (do NOT mutate the repo's testdata),
  invoke hello, EDIT the handler file (change echo key), wait, invoke again
  → response reflects the edit; add a new function dir with .lambda.yml →
  becomes routable. Generous deadlines; skip -short/missing runtimes.
- CHANGELOG [Unreleased] Added: hot reload (code edits restart just that
  function; config/structure edits reconfigure the server; broken edits
  never take the server down), function logs streamed live with
  per-function prefixes/colors (including Lambda-style `REPORT` lines from
  the emulator), `--no-reload`; plus the `lambdary init` entry (wording
  from Unit C's report).

### Unit C — `lambdary init`  *(parallel with A; no CHANGELOG edits)*

- Files: `internal/cli/init.go`, `internal/cli/init_test.go`, register in
  root.go.
- `lambdary init <name> [--runtime nodejs22.x|python3.13|provided.al2023]`
  (default nodejs22.x): creates `<root>/<name>/` with `.lambda.yml`
  (runtime, handler) and a matching handler stub: node `handler.mjs`
  (`export const handler = async (event) => ...`), python
  `lambda_function.py`, provided → executable `bootstrap` sh stub speaking
  nothing (comment pointing at the Runtime API contract + exit 1 "implement
  me"). Refuses to overwrite an existing dir (clear error). Prints next
  steps ("lambdary dev", the function's local URL).
- Tests: each runtime's scaffold content + exec bit on bootstrap; existing
  dir refusal; the scaffolded node/python function actually passes
  discovery (`discovery.Discover` on the created root finds it with the
  right runtime/route).
- Report the exact created-file wording back so Unit B can write the
  changelog entry.

## Sequencing

```
A (watcher) ──▶ B (dev wiring + e2e + changelog)
C (init)    ──▶ (feeds wording into B)
```
