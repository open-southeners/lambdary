package cli

import (
	"context"
	"io"
	"sync"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/lockfile"
	"github.com/open-southeners/lambdary/internal/router"
)

// resolveManagerBackend builds `dev`'s router.BackendResolver: mode
// resolves exactly once, eagerly, exactly as resolveBackend always has (so
// the startup banner's name is unchanged, and functions using the default
// "auto" local.backend pay no extra cost). A function whose own
// local.backend hint (discovery.Function.Backend, already defaulted to
// "auto"/"container"/"process" by discovery) differs from mode gets an
// independent, lazily resolved backend of that kind instead of silently
// running on whatever mode picked — the gap CURRENT_ISSUES.md tracked as
// "`local.backend` is not honored per function".
func resolveManagerBackend(ctx context.Context, mode string, runner backend.Runner, errW io.Writer, lock *lockfile.Lock) (router.BackendResolver, string, error) {
	global, name, err := resolveBackend(ctx, mode, runner, errW, lock)
	if err != nil {
		return nil, "", err
	}

	if mode == "" {
		mode = "auto"
	}

	r := &perFunctionBackend{
		mode:   mode,
		global: global,
		resolveContainer: func(ctx context.Context) (backend.Backend, error) {
			b, _, err := resolveContainerBackend(ctx, runner, lock)
			return b, err
		},
		resolveProcess: func(ctx context.Context) (backend.Backend, error) {
			b, _, err := resolveProcessBackend(ctx, runner, errW, lock)
			return b, err
		},
	}

	return r.resolve, name, nil
}

// perFunctionBackend implements resolveManagerBackend's router.BackendResolver
// (via its resolve method): a function whose Backend is "auto", "", or
// already equal to mode reuses global (no extra resolution). An explicit
// hint that differs from mode resolves its own backend independently, on
// first use — via the injected resolveContainer/resolveProcess, real
// resolveContainerBackend/resolveProcessBackend calls in production, fakes
// in tests — caching the result (and any error) so a second function
// asking for the same kind doesn't repeat a possibly slow detection.
type perFunctionBackend struct {
	mode   string
	global backend.Backend

	resolveContainer func(ctx context.Context) (backend.Backend, error)
	resolveProcess   func(ctx context.Context) (backend.Backend, error)

	mu            sync.Mutex
	container     backend.Backend
	containerErr  error
	containerDone bool
	process       backend.Backend
	processErr    error
	processDone   bool
}

func (r *perFunctionBackend) resolve(ctx context.Context, fn discovery.Function) (backend.Backend, error) {
	target := fn.Backend
	if target == "" || target == "auto" || target == r.mode {
		return r.global, nil
	}

	switch target {
	case "container":
		return r.resolveContainerOnce(ctx)
	case "process":
		return r.resolveProcessOnce(ctx)
	default:
		return r.global, nil
	}
}

func (r *perFunctionBackend) resolveContainerOnce(ctx context.Context) (backend.Backend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.containerDone {
		r.containerDone = true
		r.container, r.containerErr = r.resolveContainer(ctx)
	}

	return r.container, r.containerErr
}

func (r *perFunctionBackend) resolveProcessOnce(ctx context.Context) (backend.Backend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.processDone {
		r.processDone = true
		r.process, r.processErr = r.resolveProcess(ctx)
	}

	return r.process, r.processErr
}
