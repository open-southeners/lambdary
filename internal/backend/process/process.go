// Package process implements backend.Backend by driving AWS's Runtime
// Interface Emulator (RIE) — a subprocess built from pinned upstream source
// by internal/rie (Unit A; this package does not import it, per
// plans/m3-process-path.md's Unit B — the resolved binary path is injected
// into New) — against a host-installed runtime instead of a container. Per
// DESIGN.md's "Process backend" amendment, the runtime side is one of
// Lambdary's own dependency-free shims (shims/bootstrap.mjs for Node,
// shims/bootstrap.py for Python, both embedded via go:embed) or, for
// provided.* custom runtimes, the function's own `bootstrap`; `local.command`
// overrides the spawned runtime command entirely. See
// plans/rie-darwin-spike.md for the RIE flags this package hardcodes and the
// port-9001 pitfall ports.go works around.
//
// # Runner vs os/exec
//
// backend.Runner (internal/backend/exec.go) has no cwd, env, or
// process-group knobs — Run/Start only take a command name and argv. The
// RIE, though, needs all three: cwd must be the function's directory (the
// spike report found the RIE resolves a bare `./bootstrap` relative to its
// working directory, and RIE_darwin behaves the same for any relative
// runtime command), env carries the manifest's environment plus the
// AWS_LAMBDA_FUNCTION_* variables, and the RIE needs its own process group
// so Stop can signal it and the runtime process it spawns (node/python3/
// bootstrap) as one unit without also signalling Lambdary itself.
//
// Routing the spawn through Runner would mean building a `sh -c 'cd ... &&
// env ... exec riePath ...'` string — Runner's only real implementation,
// ExecRunner, never sets a process group either, so that still couldn't
// give Stop a safe kill target, while turning argv construction into
// fragile shell-quoting. This package therefore spawns the long-lived RIE
// process directly with os/exec (see spawn.go), the same primitive
// backend.ExecRunner itself is built on. process.New still accepts a
// backend.Runner to match the pinned constructor signature from
// plans/m3-process-path.md's Unit B and to leave room for any genuinely
// short-lived command a future change might add; nothing in this package
// uses it today.
//
// # Fidelity caveats: host OS, and layer content built for Amazon Linux
//
// Document loudly, per DESIGN.md's "Process backend" amendment: this
// package runs the runtime process on the host OS with host libraries —
// faithful to the Runtime API, not to Amazon Linux. The container backend
// is the fidelity option.
//
// Layers (plans/layers.md Unit D) compound the same caveat rather than
// escaping it: when a function configures layers:, Start points the
// runtime's own search-path env vars (NODE_PATH, PYTHONPATH,
// RUBYLIB/GEM_PATH, PATH — see layerenv.go) at the staged layer content
// instead of AWS's /opt, so pure-code layers (a shared JS/Python module, a
// vendored gem) resolve exactly like they would on real Lambda. But layer
// content *compiled* for Amazon Linux — a native .so a Python package
// links against, Bref's own `php` binary — will not run on the host no
// matter where the search paths point; only the container backend, which
// actually runs Amazon Linux, has that fidelity. A provided.* function
// whose bootstrap itself comes from a layer is a hard case of this: see
// runtime.go's RequiresLayerBootstrap, which routes it to the container
// backend automatically instead of letting the process backend try and
// fail to exec Linux-built layer content.
package process

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/layers"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// defaultReadyTimeout is how long Start waits for a freshly spawned
// instance's invoke port to answer before giving up, per
// plans/m3-process-path.md Unit B ("~15s, no image pull here"). A zero
// processBackend.readyTimeout falls back to this.
const defaultReadyTimeout = 15 * time.Second

// stopGrace is how long Stop waits after SIGTERM before escalating to
// SIGKILL, per plans/m3-process-path.md Unit B.
const stopGrace = 5 * time.Second

// processBackend is the backend.Backend implementation returned by New.
// readyTimeout and home are unexported and default (to defaultReadyTimeout
// and the resolved $LAMBDARY_HOME) when zero/empty; tests in this package
// construct processBackend directly to override them.
type processBackend struct {
	riePath string
	runner  backend.Runner // reserved for future short-lived commands; see package doc.

	home         string
	readyTimeout time.Duration

	// cacheDir is the project's `.lambdary` directory, passed to
	// layers.Stage so Start's env assembly can point the runtime's
	// search-path env vars at `<cacheDir>/staging/<fn>/` — see layerenv.go
	// and plans/layers.md's Unit D.
	cacheDir string
}

// New returns a backend.Backend that runs functions by spawning riePath
// (the RIE binary — see internal/rie, Unit A, for how callers resolve it)
// against a host runtime, issuing every command through runner so callers
// can substitute a fake in tests. cacheDir is the project's `.lambdary`
// directory (see processBackend's cacheDir field and plans/layers.md's
// Unit D). See the package doc for why the RIE process itself is spawned
// via os/exec rather than runner.
func New(riePath string, runner backend.Runner, cacheDir string) backend.Backend {
	return &processBackend{riePath: riePath, runner: runner, cacheDir: cacheDir}
}

// Start writes Lambdary's embedded shims to $LAMBDARY_HOME/shims (once,
// idempotently), stages fn's layers: entries (if any) into a per-function
// staging directory, allocates two distinct ephemeral ports, resolves the
// runtime command per runtimeCommand's resolution order, spawns the RIE
// with both address flags and that command as trailing args, and blocks
// until the invoke port answers before returning — per backend.Backend.
// Start's contract that the returned Instance is immediately ready to
// invoke. On any failure after the RIE process starts, Start stops it
// before returning so a failed Start never leaks a running process (or its
// runtime child).
func (b *processBackend) Start(ctx context.Context, fn discovery.Function) (backend.Instance, error) {
	absDir, err := filepath.Abs(fn.Dir)
	if err != nil {
		return nil, fmt.Errorf("process: %s: resolving function directory: %w", fn.Name, err)
	}

	home := b.home
	if home == "" {
		home, err = defaultHome()
		if err != nil {
			return nil, fmt.Errorf("process: %s: %w", fn.Name, err)
		}
	}

	shimDir, err := writeShims(home)
	if err != nil {
		return nil, fmt.Errorf("process: %s: writing shims: %w", fn.Name, err)
	}

	runtimeName, runtimeArgs, err := runtimeCommand(fn, absDir, shimDir)
	if err != nil {
		return nil, fmt.Errorf("process: %s: %w", fn.Name, err)
	}

	fileEnv, err := loadEnvFile(fn, absDir)
	if err != nil {
		return nil, fmt.Errorf("process: %s: %w", fn.Name, err)
	}

	layerRefs, err := parseFunctionLayerRefs(fn)
	if err != nil {
		return nil, fmt.Errorf("process: %s: %w", fn.Name, err)
	}

	staging, err := layers.Stage(fn.Name, layerRefs, absDir, b.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("process: %s: %w", fn.Name, err)
	}

	invokePort, rapiPort, err := allocatePorts()
	if err != nil {
		return nil, fmt.Errorf("process: %s: allocating ports: %w", fn.Name, err)
	}

	argv := rieArgs(invokePort, rapiPort, runtimeName, runtimeArgs)
	env := append(os.Environ(), manifestEnv(fn, fileEnv)...)

	if staging != "" {
		env = layerSearchPathEnv(env, fn, staging)
	}

	proc, err := spawn(b.riePath, argv, absDir, env)
	if err != nil {
		return nil, fmt.Errorf("process: %s: start: %w", fn.Name, err)
	}

	timeout := b.readyTimeout
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}

	if err := waitReady(ctx, invokePort, timeout); err != nil {
		// Stop (kill + wait for exit) *before* snapshotting the output
		// tail, not after: proc.stop only returns once cmd.Wait() has
		// returned, and — because spawn wires the process's combined
		// stdout+stderr into out (an io.Writer, not an *os.File) —
		// os/exec's own Cmd.Wait contract guarantees Wait doesn't return
		// until the goroutine copying that output has drained its pipe to
		// EOF. Snapshotting before stopping would race that drain: the
		// process may already have written its last log line (e.g. a boot
		// error) to the pipe, but this side might not have read it into
		// out yet, silently dropping it from the error below. stop's own
		// grace period (stopGrace) plus SIGKILL escalation keeps this
		// bounded rather than an open-ended wait.
		proc.stop(stopGrace) //nolint:errcheck // best-effort cleanup of a process that never became ready; the readiness error is what matters to the caller.
		tail := proc.out.tail(2048)

		return nil, fmt.Errorf("process: %s: %w%s", fn.Name, err, tail)
	}

	return &instance{proc: proc, port: invokePort}, nil
}

// loadEnvFile loads fn's local.env_file (manifest.Local.EnvFile), resolving
// a relative path against absDir (fn's directory, already made absolute) —
// per plans/m5-extras.md's Unit D. Mirrors internal/backend/container's
// loadEnvFile for the container backend. It returns a nil map and nil error
// when fn has no local.env_file configured at all. A configured path that
// can't be opened or parsed is a Start error naming the path: env_file was
// set explicitly, so silently running without it would hide a typo or a
// missing file rather than surfacing it.
func loadEnvFile(fn discovery.Function, absDir string) (map[string]string, error) {
	m := fn.Manifest
	if m == nil || m.Local.EnvFile == "" {
		return nil, nil
	}

	path := m.Local.EnvFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(absDir, path)
	}

	env, err := backend.ParseEnvFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading local.env_file: %w", err)
	}

	return env, nil
}

// parseFunctionLayerRefs parses fn.Manifest.Layers (already validated at
// manifest load time — see internal/manifest's ErrInvalidLayers) into the
// []manifest.LayerRef layers.Stage consumes. A nil manifest or an empty
// Layers list returns a nil slice, so Stage's own len(refs) == 0 check
// short-circuits and returns "" without touching the filesystem: functions
// without a `layers:` key pay no staging cost at all, matching
// internal/layers' own package doc. A parse failure here would mean the
// manifest was loaded without going through validation (or was mutated
// after); it is still reported as a Start error naming the bad entry rather
// than panicking.
func parseFunctionLayerRefs(fn discovery.Function) ([]manifest.LayerRef, error) {
	m := fn.Manifest
	if m == nil || len(m.Layers) == 0 {
		return nil, nil
	}

	refs := make([]manifest.LayerRef, 0, len(m.Layers))

	for _, raw := range m.Layers {
		ref, err := manifest.ParseLayerRef(raw)
		if err != nil {
			return nil, fmt.Errorf("layer %q: %w", raw, err)
		}

		refs = append(refs, ref)
	}

	return refs, nil
}

// defaultHome resolves $LAMBDARY_HOME, the shared root for Lambdary's local
// state — this package writes its shims under home/shims/<hash>. Defaults
// to ~/.lambdary. internal/rie (Unit A) resolves the same variable
// independently for its own binary cache (~/.lambdary/bin), since this
// package must not import internal/rie — see the package doc.
func defaultHome() (string, error) {
	if home := os.Getenv("LAMBDARY_HOME"); home != "" {
		return home, nil
	}

	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving $LAMBDARY_HOME: %w", err)
	}

	return filepath.Join(userHome, ".lambdary"), nil
}
