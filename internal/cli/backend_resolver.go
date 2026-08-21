package cli

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/process"
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
// "`local.backend` is not honored per function". cacheDir is the project's
// `.lambdary` directory (see cacheDirFor), threaded into every backend this
// resolver builds.
func resolveManagerBackend(ctx context.Context, mode string, runner backend.Runner, errW io.Writer, lock *lockfile.Lock, cacheDir string) (router.BackendResolver, string, error) {
	global, kind, name, err := resolveBackend(ctx, mode, runner, errW, lock, cacheDir)
	if err != nil {
		return nil, "", err
	}

	if mode == "" {
		mode = "auto"
	}

	r := &perFunctionBackend{
		mode:       mode,
		global:     global,
		globalKind: kind,
		errW:       errW,
		resolveContainer: func(ctx context.Context) (backend.Backend, error) {
			b, _, _, err := resolveContainerBackend(ctx, runner, lock, cacheDir)
			return b, err
		},
		resolveProcess: func(ctx context.Context) (backend.Backend, error) {
			b, _, _, err := resolveProcessBackend(ctx, runner, errW, lock, cacheDir)
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
//
// Whichever backend resolve settles on, it also checks whether that
// backend's kind is process and, if so, whether process.Supports the
// function's runtime — per plans/process-ruby-and-container-fallback.md's
// Unit B, a function whose runtime has no process-backend shim (java21,
// dotnet8, ...) still needs to run somewhere, so resolve transparently
// falls back to the container backend instead of letting the caller hit
// process.ErrRuntimeNotSupported. errW and notified exist only to support
// that fallback's one-notice-per-function stderr message.
type perFunctionBackend struct {
	mode       string
	global     backend.Backend
	globalKind string
	errW       io.Writer

	resolveContainer func(ctx context.Context) (backend.Backend, error)
	resolveProcess   func(ctx context.Context) (backend.Backend, error)

	mu            sync.Mutex
	container     backend.Backend
	containerErr  error
	containerDone bool
	process       backend.Backend
	processErr    error
	processDone   bool
	notified      map[string]bool
}

func (r *perFunctionBackend) resolve(ctx context.Context, fn discovery.Function) (backend.Backend, error) {
	b, kind, err := r.resolveTarget(ctx, fn.Backend)
	if err != nil {
		return nil, err
	}

	if needsContainerFallback(kind, fn) {
		return r.fallbackToContainer(ctx, fn)
	}

	return b, nil
}

// needsContainerFallback reports whether a resolved backend of kind (as
// resolveBackend/resolveTarget return it) needs to fall back to the
// container backend for fn: true exactly when kind is process and
// process.Supports rejects fn.Runtime. Shared by perFunctionBackend.resolve
// (dev's per-function resolver) and invokeStandalone (the standalone invoke
// path, see invoke.go) so both apply the exact same gate before calling
// fallbackToContainerBackend.
func needsContainerFallback(kind string, fn discovery.Function) bool {
	return kind == backendKindProcess && !process.Supports(fn)
}

// resolveTarget resolves fn.Backend's raw hint (see discovery.Function.Backend)
// to a backend.Backend and its kind (backendKindContainer/backendKindProcess).
// Split out of resolve so the container-fallback check only has to run once,
// after the target backend is known, regardless of which of the three cases
// below produced it — global (the common case, no extra resolution), or one
// of the lazily-resolved, cached container/process overrides.
func (r *perFunctionBackend) resolveTarget(ctx context.Context, target string) (backend.Backend, string, error) {
	if target == "" || target == "auto" || target == r.mode {
		return r.global, r.globalKind, nil
	}

	switch target {
	case "container":
		b, err := r.resolveContainerOnce(ctx)
		return b, backendKindContainer, err
	case "process":
		b, err := r.resolveProcessOnce(ctx)
		return b, backendKindProcess, err
	default:
		return r.global, r.globalKind, nil
	}
}

// fallbackToContainer resolves the container backend for fn once resolve
// has determined fn's target backend is process but process.Supports
// rejects its runtime. It reuses resolveContainerOnce, so the fallback
// shares the same cached container backend (and cache entry) any function
// with an explicit local.backend: container hint would also get — no
// separate resolution, no separate failure mode. The stderr notice is
// printed at most once per function name, guarded by notified, since
// resolve runs again on every cold start (see router.Manager).
func (r *perFunctionBackend) fallbackToContainer(ctx context.Context, fn discovery.Function) (backend.Backend, error) {
	return fallbackToContainerBackend(ctx, fn, r.resolveContainerOnce, func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		if r.notified == nil {
			r.notified = make(map[string]bool)
		}

		if r.notified[fn.Name] {
			return
		}

		r.notified[fn.Name] = true

		fmt.Fprint(r.errW, containerFallbackNotice(fn))
	})
}

// containerFallbackNotice is the stderr message printed the first time a
// function's runtime turns out to have no process-backend shim and
// lambdary transparently runs it in a container instead. Shared by
// perFunctionBackend.fallbackToContainer (dev's per-function resolver) and
// invokeStandalone (the standalone invoke path, see invoke.go) so both
// report the fallback identically.
func containerFallbackNotice(fn discovery.Function) string {
	return fmt.Sprintf("function %s: runtime %s has no process-backend shim — running in a container\n", fn.Name, fn.Runtime)
}

// fallbackToContainerBackend resolves the container backend for a function
// whose target backend is process but whose runtime process.Supports
// rejects — shared by perFunctionBackend.fallbackToContainer and
// invokeStandalone so both give identical behavior for
// plans/process-ruby-and-container-fallback.md's Unit B: every official AWS
// runtime still runs, just in a container, since AWS's own base images ship
// a real Runtime Interface Client for every runtime while lambdary's
// embedded shims only cover Node/Python/Ruby (see
// internal/backend/process's Supports). resolveContainer is the caller's
// own container resolver (perFunctionBackend.resolveContainerOnce in
// production dev use, a plain resolveContainerBackend call for standalone
// invoke, fakes in tests); notify is called exactly once, after a
// successful resolution, so callers can print (or dedupe) their own notice.
// A failed container resolution returns an error naming both facts — the
// runtime isn't supported by the process backend, and the container
// fallback has nowhere to go either — wrapping both process.ErrRuntimeNotSupported
// and the container error (Go's multi-%w fmt.Errorf), so both remain
// matchable via errors.Is.
func fallbackToContainerBackend(ctx context.Context, fn discovery.Function, resolveContainer func(context.Context) (backend.Backend, error), notify func()) (backend.Backend, error) {
	containerBackend, err := resolveContainer(ctx)
	if err != nil {
		return nil, fmt.Errorf("function %s: runtime %q: %w (container fallback also unavailable: %w)", fn.Name, fn.Runtime, process.ErrRuntimeNotSupported, err)
	}

	notify()

	return containerBackend, nil
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
