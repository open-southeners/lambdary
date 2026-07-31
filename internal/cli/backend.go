package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/container"
	"github.com/open-southeners/lambdary/internal/backend/process"
	"github.com/open-southeners/lambdary/internal/rie"
)

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

// resolveBackend builds the backend.Backend --backend=mode names, shared by
// `dev` (runDev) and `invoke`'s standalone mode (invokeStandalone), per
// plans/m3-process-path.md's Unit C. It returns the backend along with a
// human-readable, already-formatted name for the startup/summary output
// ("container (docker)", "process (host runtimes)", ...).
//
//   - "container": resolves via backend.DetectContainerCLI, or fails.
//   - "process": resolves the RIE binary via internal/rie.Resolve (which
//     itself prints a first-build progress note to stderr — see
//     rie.Resolve's doc comment) and builds internal/backend/process, or
//     fails; rie.Resolve's own errors (including ErrBuildToolsMissing) are
//     surfaced as-is, since they already name the remedy.
//   - "auto" (also "", the flag's default): tries container first; on
//     errors.Is backend.ErrNoContainerCLI or backend.ErrDaemonUnreachable,
//     prints a single notice to errW and falls back to process. Any other
//     container-detection error fails outright rather than falling back,
//     since it isn't one of the "no usable container runtime" cases.
func resolveBackend(ctx context.Context, mode string, runner backend.Runner, errW io.Writer) (backend.Backend, string, error) {
	switch mode {
	case "", "auto":
		return resolveAutoBackend(ctx, runner, errW)
	case "container":
		return resolveContainerBackend(ctx, runner)
	case "process":
		return resolveProcessBackend(ctx, runner)
	default:
		return nil, "", fmt.Errorf(`unknown --backend %q: want "auto", "container", or "process"`, mode)
	}
}

// resolveContainerBackend implements the "container" mode: DetectContainerCLI
// or fail.
func resolveContainerBackend(ctx context.Context, runner backend.Runner) (backend.Backend, string, error) {
	cli, err := backend.DetectContainerCLI(ctx, runner)
	if err != nil {
		return nil, "", err
	}

	return container.New(cli, runner), fmt.Sprintf("container (%s)", cli), nil
}

// resolveProcessBackend implements the "process" mode: rie.Resolve + build
// the process backend, or fail (rie.Resolve's error already names the
// remedy — missing build tools, a bad $LAMBDARY_RIE_PATH, etc.).
func resolveProcessBackend(ctx context.Context, runner backend.Runner) (backend.Backend, string, error) {
	riePath, err := rie.Resolve(ctx, runner)
	if err != nil {
		return nil, "", err
	}

	return process.New(riePath, runner), "process (host runtimes)", nil
}

// resolveAutoBackend implements the "auto" mode: container first, falling
// back to process with a single clear notice on errW when the container
// runtime simply isn't usable (no CLI on PATH, or a CLI present but its
// daemon unreachable) — any other error (e.g. a mode="process" fallback
// itself failing to resolve the RIE) is returned as-is.
func resolveAutoBackend(ctx context.Context, runner backend.Runner, errW io.Writer) (backend.Backend, string, error) {
	b, name, err := resolveContainerBackend(ctx, runner)
	if err == nil {
		return b, name, nil
	}

	if !errors.Is(err, backend.ErrNoContainerCLI) && !errors.Is(err, backend.ErrDaemonUnreachable) {
		return nil, "", err
	}

	fmt.Fprintf(errW, "no usable container runtime (%s) — falling back to the process backend using host runtimes\n", err)

	return resolveProcessBackend(ctx, runner)
}
