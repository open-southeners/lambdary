package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/container"
	"github.com/open-southeners/lambdary/internal/backend/process"
	"github.com/open-southeners/lambdary/internal/lockfile"
	"github.com/open-southeners/lambdary/internal/rie"
)

// cacheDirFor returns the project's `.lambdary` cache directory for root —
// the same root lockfile.Load(root) reads/writes `.lambdary/lock` under, so
// `.lambdary/lock` (internal/lockfile), `.lambdary/layers/`, and
// `.lambdary/staging/` (plans/layers.md's Units B–D) all co-locate. Callers
// resolve this once, at the same place they call lockfile.Load, and thread
// it alongside lock through the resolve chain.
func cacheDirFor(root string) string {
	return filepath.Join(root, ".lambdary")
}

// validateBackend checks --backend's value: "" and "auto" both mean the
// automatic selection DESIGN.md's "Selection (backend: auto)" describes
// (container, falling back to process); "container" and "process" are
// accepted explicitly; anything else errors as unrecognized.
func validateBackend(v string) error {
	switch v {
	case "", "auto", "container", "process":
		return nil
	default:
		return fmt.Errorf(`unknown --backend %q: want "auto", "container", or "process"`, v)
	}
}

// backendKindContainer and backendKindProcess are the machine-readable kind
// constants resolveBackend and friends return alongside their
// human-readable display name ("container (docker)", "process (host
// runtimes)", ...) — added for
// plans/process-ruby-and-container-fallback.md's Unit B, so callers that
// need to know *which* kind "auto" actually picked (to decide whether a
// container fallback applies) can compare against these instead of parsing
// the display string.
const (
	backendKindContainer = "container"
	backendKindProcess   = "process"
)

// resolveBackend builds the backend.Backend --backend=mode names, shared by
// `dev` (runDev) and `invoke`'s standalone mode (invokeStandalone), per
// plans/m3-process-path.md's Unit C. It returns the backend along with its
// machine-readable kind (backendKindContainer/backendKindProcess — see
// plans/process-ruby-and-container-fallback.md's Unit B) and a
// human-readable, already-formatted name for the startup/summary output
// ("container (docker)", "process (host runtimes)", ...). lock is the
// project's `.lambdary/lock` (see internal/lockfile.Load), or nil for
// callers that don't want digest pinning/recording — see
// plans/m5-extras.md's Unit C. cacheDir is the project's `.lambdary`
// directory (see cacheDirFor), threaded into the backends' own cacheDir
// field for the layer staging plans/layers.md's Units C and D will add.
//
//   - "container": resolves via backend.DetectContainerCLI, or fails; the
//     returned backend pins/records image digests through lock when it's
//     non-nil (container.NewWithLock), otherwise behaves exactly as before
//     Unit C (container.New).
//   - "process": resolves the RIE binary via internal/rie.Resolve (which
//     itself prints a first-build progress note to stderr — see
//     rie.Resolve's doc comment) and builds internal/backend/process, or
//     fails; rie.Resolve's own errors (including ErrBuildToolsMissing) are
//     surfaced as-is, since they already name the remedy. On success, and
//     when lock is non-nil, records the resolved rie.Version into the lock
//     — informational only, per DESIGN.md's version-pinning decision; a
//     failure to record is noted on errW rather than failing backend
//     resolution.
//   - "auto" (also "", the flag's default): tries container first; on
//     errors.Is backend.ErrNoContainerCLI or backend.ErrDaemonUnreachable,
//     prints a single notice to errW and falls back to process. Any other
//     container-detection error fails outright rather than falling back,
//     since it isn't one of the "no usable container runtime" cases.
func resolveBackend(ctx context.Context, mode string, runner backend.Runner, errW io.Writer, lock *lockfile.Lock, cacheDir string) (b backend.Backend, kind, name string, err error) {
	switch mode {
	case "", "auto":
		return resolveAutoBackend(ctx, runner, errW, lock, cacheDir)
	case "container":
		return resolveContainerBackend(ctx, runner, lock, cacheDir)
	case "process":
		return resolveProcessBackend(ctx, runner, errW, lock, cacheDir)
	default:
		return nil, "", "", fmt.Errorf(`unknown --backend %q: want "auto", "container", or "process"`, mode)
	}
}

// resolveContainerBackend implements the "container" mode: DetectContainerCLI
// or fail; lock (nil-able) is threaded into container.NewWithLock/New, and
// cacheDir (the project's `.lambdary` directory) into its cacheDir field.
func resolveContainerBackend(ctx context.Context, runner backend.Runner, lock *lockfile.Lock, cacheDir string) (backend.Backend, string, string, error) {
	cli, err := backend.DetectContainerCLI(ctx, runner)
	if err != nil {
		return nil, "", "", err
	}

	var b backend.Backend
	if lock != nil {
		b = container.NewWithLock(cli, runner, lock, cacheDir)
	} else {
		b = container.New(cli, runner, cacheDir)
	}

	return b, backendKindContainer, fmt.Sprintf("container (%s)", cli), nil
}

// resolveProcessBackend implements the "process" mode: rie.Resolve + build
// the process backend, or fail (rie.Resolve's error already names the
// remedy — missing build tools, a bad $LAMBDARY_RIE_PATH, etc.). On
// success, a non-nil lock records rie.Version (SetRIE) — best-effort, per
// plans/m5-extras.md's Unit C: a recording failure is noted on errW rather
// than failing backend resolution. cacheDir (the project's `.lambdary`
// directory) is threaded into process.New's cacheDir field.
func resolveProcessBackend(ctx context.Context, runner backend.Runner, errW io.Writer, lock *lockfile.Lock, cacheDir string) (backend.Backend, string, string, error) {
	riePath, err := rie.Resolve(ctx, runner)
	if err != nil {
		return nil, "", "", err
	}

	if lock != nil {
		if err := lock.SetRIE(rie.Version); err != nil {
			fmt.Fprintf(errW, "warning: recording rie version in .lambdary/lock: %s\n", err)
		}
	}

	return process.New(riePath, runner, cacheDir), backendKindProcess, "process (host runtimes)", nil
}

// resolveAutoBackend implements the "auto" mode: container first, falling
// back to process with a single clear notice on errW when the container
// runtime simply isn't usable (no CLI on PATH, or a CLI present but its
// daemon unreachable) — any other error (e.g. a mode="process" fallback
// itself failing to resolve the RIE) is returned as-is.
func resolveAutoBackend(ctx context.Context, runner backend.Runner, errW io.Writer, lock *lockfile.Lock, cacheDir string) (backend.Backend, string, string, error) {
	b, kind, name, err := resolveContainerBackend(ctx, runner, lock, cacheDir)
	if err == nil {
		return b, kind, name, nil
	}

	if !errors.Is(err, backend.ErrNoContainerCLI) && !errors.Is(err, backend.ErrDaemonUnreachable) {
		return nil, "", "", err
	}

	fmt.Fprintf(errW, "no usable container runtime (%s) — falling back to the process backend using host runtimes\n", err)

	return resolveProcessBackend(ctx, runner, errW, lock, cacheDir)
}
