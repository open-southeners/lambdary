# Layers — `/opt` emulation for both backends

Implements DESIGN.md's "Layers (post-v0)" section: a top-level `layers:`
manifest key (AWS vocabulary — list of layer version ARNs and/or local
paths, max 5), resolved and merged into a per-function staging directory,
then exposed as `/opt` in the container backend and as runtime search-path
env vars in the process backend. Parity target: functions that use layers
in production — Laravel Sidecar (`layers()` ARNs), Bref's public PHP layer
ARNs, Serverless/SAM local-path layers — run locally unchanged.

Core insight (verified against the embedded RIE binary): layers are pure
filesystem + environment. The RIE and AWS base images already handle `/opt`
semantics; no emulator changes anywhere in this plan.

Out of scope (unchanged non-goals / deferred):

- Extensions lifecycle guarantees. `/opt/extensions` content may work
  incidentally in containers (the RIE ships the Extensions API); a dedicated
  spike would be its own plan.
- Watching local layer directories for hot reload. A function restart
  re-stages, so edits to a layer are picked up on the next restart of a
  consuming function; fsnotify on layer dirs can come later.
- Publishing/packaging layers to AWS.

## Units

### Unit A — manifest `layers:` key  *(first; everything depends on it)*

- Files: `internal/manifest/manifest.go` (+ `config.go` if the root
  `lambdary.yml` defaults block should inherit it — it should, matching
  `environment`'s layering), tests.
- `Layers []string` on `Manifest`, top-level (NOT under `local:` — it is
  AWS's own configuration vocabulary, per the Goals section).
- Validation (new `ErrInvalidLayers`, wrapped with path/value like the
  existing errors): max 5 entries (AWS's limit); each entry non-empty; an
  entry is an ARN iff it matches
  `arn:aws:lambda:<region>:<account>:layer:<name>:<version>` (version must
  be numeric) — anything not starting with `arn:` is a local path. A string
  starting with `arn:` that does NOT parse as a layer-version ARN is an
  error (catches truncated ARNs missing the version, which
  `GetLayerVersion` requires).
- Export the classification (`ParseLayerRef(s) (LayerRef, error)` with
  `Kind: arn|path` or similar) so B/E consume parsed refs, not raw strings.
- Tests: table over valid ARN / local dir / zip path / >5 entries / empty
  entry / versionless ARN; defaults-inheritance test mirroring the existing
  environment ones.

### Unit B — resolution & staging (`internal/layers`)  *(after A)*

- Files: new `internal/layers/` package (`layers.go`, `stage.go`, zip
  extraction helper), tests with fixture zips/dirs under `testdata/`.
- `Stage(refs []LayerRef, fnDir, cacheDir string) (stagingPath string, err error)`:
  - local dir ref → treated as already-extracted layer content;
  - local `.zip` ref → extracted (path-traversal-safe: reject entries
    escaping the target, preserve the executable bit — `bootstrap` and
    `bin/*` must stay runnable);
  - ARN refs → looked up in the cache only (`<cacheDir>/layers/<sanitized-arn>/`);
    a cache miss is an error naming Unit E's fetch step, so B is buildable
    and testable with zero AWS dependency.
  - Merge all resolved layers **in declared order** into one staging dir
    per function (`<cacheDir>/staging/<fn>/`), later entries overwriting
    earlier — AWS's documented layer-merge semantics. Staging is rebuilt on
    every call (Start-time), which is what makes restart pick up layer
    edits; cheap because it is local file copies.
- No layers configured → `Stage` returns "" and backends skip all layer
  wiring (zero behavior change for existing projects — the common case).
- Tests: merge-order override wins, exec-bit preservation, zip-slip
  rejection, dir + zip + cached-ARN mix, deterministic staging content,
  cache-miss error names the ARN.

### Unit C — container backend `/opt`  *(after B; parallel with D)*

- Files: `internal/backend/container/argv.go`, `container.go`, tests.
- `runArgs` gains the staging path (empty = no layers): append
  `-v <staging>:/opt:ro` after the `/var/task` mount.
- `provided.*` bootstrap search order (the Bref win): today
  `<fnDir>/bootstrap` is bind-mounted to `/var/runtime/bootstrap`
  unconditionally. Emulate real Lambda's order instead: if
  `<fnDir>/bootstrap` exists → mount it (current behavior, `/var/task`
  wins); else if staging contains `bootstrap` → mount
  `<staging>/bootstrap` to `/var/runtime/bootstrap`; else keep today's
  behavior (mount the missing path; Docker's empty-dir quirk and its
  documented failure mode are unchanged). Update `argv.go`'s comment block
  accordingly.
- Result: a Bref/`composer.json` function with a `layers:` ARN needs **no
  local `bootstrap` file** — same contract as AWS.
- Tests: fake-runner argv assertions (staging mount present/absent;
  provided.* three-way bootstrap resolution); integration test
  (Docker-gated): python function + local layer dir providing a module the
  handler imports from `/opt/python`, invoke, assert; provided.* fixture
  with bootstrap only in the layer.

### Unit D — process backend search paths  *(after B; parallel with C)*

- Files: `internal/backend/process/process.go`, `runtime.go`, tests;
  `internal/cli` fallback wiring.
- With a non-empty staging dir, the env assembly (around
  `process.go`'s `append(os.Environ(), manifestEnv(...))`) adds, per
  runtime family, AWS's standard layer paths pointed at staging instead of
  `/opt` (appending to any pre-existing host value with the OS list
  separator):
  - node: `NODE_PATH` += `<staging>/nodejs/node_modules` and
    `<staging>/nodejs/node<major>/node_modules`;
  - python: `PYTHONPATH` += `<staging>/python` and
    `<staging>/python/lib/python<ver>/site-packages`;
  - ruby: `RUBYLIB` += `<staging>/ruby/lib`, `GEM_PATH` +=
    `<staging>/ruby/gems/<abi>`;
  - all families: `PATH` += `<staging>/bin`.
  Paths are added whether or not the subdirectory exists (matching AWS,
  which sets them unconditionally); resolvers tolerate missing entries.
- Fidelity caveat documented in the package doc next to the existing
  host-OS caveat: layer content compiled for Amazon Linux (native `.so`,
  Bref's `php`) will not run on the host — the container backend is the
  fidelity option, unchanged.
- Fallback: a `provided.*` function with layers and no `<fnDir>/bootstrap`
  cannot run as a host process (its bootstrap is Linux-built layer
  content) → route through the existing "no process-backend shim" container
  fallback in `internal/cli`, same one-line notice. `--backend
  process`/`auto` still never fail a function outright.
- Tests: env assertions per family (append vs create, separator, both
  node paths), shim import-from-staging round trip per shim (extend
  `shim_contract_test.go` style), provided.*+layer fallback decision test.

### Unit E — ARN fetch + lock pinning  *(after A; parallel with C/D)*

- Files: `internal/layers/fetch.go` + tests, `internal/lockfile` (new
  `layers:` map), `internal/cli` wiring, `go.mod`.
- Decision (record in DESIGN's log if changed): use
  `aws-sdk-go-v2/service/lambda` for `GetLayerVersion` — modular enough,
  and shelling out to an optionally-installed `aws` CLI would add a runtime
  dependency Lambdary can't vendor. Credentials/region resolve through the
  SDK's default chain; the ARN's own region component wins over the
  ambient region (Bref ARNs are region-specific).
- Flow: cache hit (`<cacheDir>/layers/<sanitized-arn>/` exists) → done, no
  network, no credentials needed. Miss → `GetLayerVersion` (works for
  third-party public layers like Bref's — SAM CLI's mechanism), download
  the returned presigned `Content.Location` URL, verify
  `Content.CodeSha256`, extract into the cache (same safe extraction as
  Unit B), record `<arn>: <codesha256>` under a new `layers:` map in
  `.lambdary/lock` (schema stays version 1 — the field is additive and
  old readers ignore it; keep Load forgiving per the package contract).
  On later hits, a lock digest mismatch against the cached content warns
  and re-fetches (layer versions are immutable on AWS, so this only
  catches local cache corruption).
- Errors are actionable: no credentials → name the ARN and say which
  profile/env the SDK looked for; AccessDenied → say the layer may be
  private. Fetch failures fail only that function's start, mirroring how a
  bad image pull behaves today.
- Tests: fake HTTP layer service (httptest) for fetch/verify/extract,
  cache-hit-no-network, sha mismatch, lockfile round trip with `layers:`
  present and absent; no live-AWS test in CI.

### Unit F — docs, changelog, E2E  *(last)*

- README: layers paragraph in Configuration (ARN + local path example,
   5-layer/merge-order semantics, process-backend native-binary caveat,
  Bref-without-local-bootstrap callout); remove any "layers not
  supported" phrasing if present.
- `lambdary list`: no change required, but verify layered functions show
  normally.
- E2E (Docker-gated): local-dir layer fixture end to end through `dev`
  (HTTP route → handler imports from layer). Manual verification recorded
  in the PR: a real Bref ARN fetch + invoke, and a Sidecar-style Node
  function with a local layer.
- CHANGELOG `[Unreleased]` Added: **Lambda layers** — `layers:` manifest
  key accepting layer version ARNs (fetched with your AWS credentials and
  cached) and local directories/zips, merged at `/opt` in order like AWS;
  Bref-style custom runtimes can now get `bootstrap` from a layer;
  process backend resolves pure-code layers via runtime search paths.

## Sequencing

```
A (manifest) ─▶ B (staging) ─▶ C (container /opt) ─┐
                          └──▶ D (process paths)  ─┼─▶ F (docs + E2E)
A ───────────▶ E (ARN fetch + lock) ──────────────┘
```

A first; B after A; C ∥ D after B; E after A (independent of B/C/D — B
consumes E's cache layout, so agree the `<cacheDir>/layers/<sanitized-arn>/`
contract up front); F last.
