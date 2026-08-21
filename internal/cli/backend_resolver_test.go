package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/process"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// fakeBackends builds three distinct, comparable backend.Backend values
// (global/container/process) so tests can assert exactly which one
// perFunctionBackend.resolve picked, by identity.
func fakeBackends() (global, container, process backend.Backend) {
	return &fakeManagerBackend{name: "global"}, &fakeManagerBackend{name: "container"}, &fakeManagerBackend{name: "process"}
}

// fakeManagerBackend is a minimal, identity-comparable backend.Backend test
// double — perFunctionBackend never calls Start itself (that's Manager's
// job), so only the type needs to exist to satisfy the interface.
type fakeManagerBackend struct {
	name string
}

func (f *fakeManagerBackend) Start(context.Context, discovery.Function) (backend.Instance, error) {
	return nil, errors.New("fakeManagerBackend: Start not implemented")
}

// fnWithBackend builds a discovery.Function for the tests below that don't
// exercise the container fallback (plans/process-ruby-and-container-fallback.md's
// Unit B): its runtime is a process.Supports-supported one (nodejs), so
// perFunctionBackend.resolve's fallback check never triggers and these
// tests keep asserting the pre-Unit-B routing behavior. Tests that need an
// unsupported runtime use fnWithRuntime instead.
func fnWithBackend(name, target string) discovery.Function {
	return fnWithRuntime(name, target, "nodejs22.x")
}

// fnWithRuntime is fnWithBackend plus an explicit runtime, for tests that
// need to control whether process.Supports accepts the function.
func fnWithRuntime(name, target, runtime string) discovery.Function {
	return discovery.Function{Name: name, Backend: target, Runtime: runtime}
}

func TestPerFunctionBackendResolveAutoUsesGlobal(t *testing.T) {
	global, containerB, processB := fakeBackends()
	r := &perFunctionBackend{
		mode:             "container",
		global:           global,
		resolveContainer: func(context.Context) (backend.Backend, error) { return containerB, nil },
		resolveProcess:   func(context.Context) (backend.Backend, error) { return processB, nil },
	}

	for _, target := range []string{"", "auto"} {
		got, err := r.resolve(context.Background(), fnWithBackend("a", target))
		if err != nil {
			t.Fatalf("resolve(%q) unexpected error: %v", target, err)
		}
		if got != global {
			t.Errorf("resolve(%q) = %v, want the global backend", target, got)
		}
	}
}

func TestPerFunctionBackendResolveExplicitMatchingModeUsesGlobalWithoutLazyResolve(t *testing.T) {
	global, _, _ := fakeBackends()
	calls := 0
	r := &perFunctionBackend{
		mode:   "container",
		global: global,
		resolveContainer: func(context.Context) (backend.Backend, error) {
			calls++
			return nil, errors.New("should not be called")
		},
		resolveProcess: func(context.Context) (backend.Backend, error) {
			calls++
			return nil, errors.New("should not be called")
		},
	}

	got, err := r.resolve(context.Background(), fnWithBackend("a", "container"))
	if err != nil {
		t.Fatalf("resolve() unexpected error: %v", err)
	}
	if got != global {
		t.Errorf("resolve() = %v, want the global backend (mode already matches)", got)
	}
	if calls != 0 {
		t.Errorf("lazy resolvers called %d times, want 0", calls)
	}
}

func TestPerFunctionBackendResolveExplicitOverrideDiffersFromMode(t *testing.T) {
	global, containerB, processB := fakeBackends()
	r := &perFunctionBackend{
		mode:             "auto",
		global:           global,
		resolveContainer: func(context.Context) (backend.Backend, error) { return containerB, nil },
		resolveProcess:   func(context.Context) (backend.Backend, error) { return processB, nil },
	}

	got, err := r.resolve(context.Background(), fnWithBackend("a", "process"))
	if err != nil {
		t.Fatalf("resolve() unexpected error: %v", err)
	}
	if got != processB {
		t.Errorf("resolve() = %v, want the process backend", got)
	}

	got, err = r.resolve(context.Background(), fnWithBackend("b", "container"))
	if err != nil {
		t.Fatalf("resolve() unexpected error: %v", err)
	}
	if got != containerB {
		t.Errorf("resolve() = %v, want the container backend", got)
	}
}

func TestPerFunctionBackendResolveCachesLazyResolutionAcrossFunctions(t *testing.T) {
	global, containerB, _ := fakeBackends()
	calls := 0
	r := &perFunctionBackend{
		mode:   "auto",
		global: global,
		resolveContainer: func(context.Context) (backend.Backend, error) {
			calls++
			return containerB, nil
		},
	}

	for _, name := range []string{"a", "b", "c"} {
		got, err := r.resolve(context.Background(), fnWithBackend(name, "container"))
		if err != nil {
			t.Fatalf("resolve(%s) unexpected error: %v", name, err)
		}
		if got != containerB {
			t.Errorf("resolve(%s) = %v, want the container backend", name, got)
		}
	}

	if calls != 1 {
		t.Errorf("resolveContainer called %d times, want 1 (cached across functions)", calls)
	}
}

func TestPerFunctionBackendResolveCachesError(t *testing.T) {
	global, _, _ := fakeBackends()
	wantErr := errors.New("container backend unavailable")
	calls := 0
	r := &perFunctionBackend{
		mode:   "auto",
		global: global,
		resolveContainer: func(context.Context) (backend.Backend, error) {
			calls++
			return nil, wantErr
		},
	}

	for i := 0; i < 2; i++ {
		_, err := r.resolve(context.Background(), fnWithBackend("a", "container"))
		if !errors.Is(err, wantErr) {
			t.Fatalf("resolve() error = %v, want errors.Is wantErr", err)
		}
	}

	if calls != 1 {
		t.Errorf("resolveContainer called %d times, want 1 (failure cached too)", calls)
	}
}

// TestPerFunctionBackendResolveFallsBackToContainerForUnsupportedRuntime
// covers plans/process-ruby-and-container-fallback.md's Unit B: a global
// process backend with a function whose runtime (java21) has no
// process-backend shim gets the container backend instead, with a notice
// written to errW.
func TestPerFunctionBackendResolveFallsBackToContainerForUnsupportedRuntime(t *testing.T) {
	global, containerB, _ := fakeBackends()
	var errW bytes.Buffer

	r := &perFunctionBackend{
		mode:       "process",
		global:     global,
		globalKind: backendKindProcess,
		errW:       &errW,
		resolveContainer: func(context.Context) (backend.Backend, error) {
			return containerB, nil
		},
	}

	got, err := r.resolve(context.Background(), fnWithRuntime("legacy", "", "java21"))
	if err != nil {
		t.Fatalf("resolve() unexpected error: %v", err)
	}
	if got != containerB {
		t.Errorf("resolve() = %v, want the container backend (fallback)", got)
	}

	wantNotice := "function legacy: runtime java21 has no process-backend shim — running in a container\n"
	if errW.String() != wantNotice {
		t.Errorf("errW = %q, want %q", errW.String(), wantNotice)
	}
}

// TestPerFunctionBackendResolveFallsBackToContainerForLayersBootstrap covers
// plans/layers.md Unit D's carve-out: a global process backend with a
// provided.* function that configures layers: and has no local ./bootstrap
// gets the container backend too, like the unsupported-runtime case above,
// but with wording that names the real reason (the bootstrap comes from a
// layer, which needs Amazon Linux) instead of the generic "no
// process-backend shim" message, which would be misleading here — the
// process backend does understand provided.* runtimes in general.
func TestPerFunctionBackendResolveFallsBackToContainerForLayersBootstrap(t *testing.T) {
	global, containerB, _ := fakeBackends()
	var errW bytes.Buffer

	r := &perFunctionBackend{
		mode:       "process",
		global:     global,
		globalKind: backendKindProcess,
		errW:       &errW,
		resolveContainer: func(context.Context) (backend.Backend, error) {
			return containerB, nil
		},
	}

	fn := discovery.Function{
		Name:     "bref-fn",
		Runtime:  "provided.al2023",
		Dir:      t.TempDir(),
		Manifest: &manifest.Manifest{Layers: []string{"arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1"}},
	}

	got, err := r.resolve(context.Background(), fn)
	if err != nil {
		t.Fatalf("resolve() unexpected error: %v", err)
	}
	if got != containerB {
		t.Errorf("resolve() = %v, want the container backend (fallback)", got)
	}

	wantNotice := "function bref-fn: runtime provided.al2023 has no local ./bootstrap and its layers: entries must run on Amazon Linux — running in a container\n"
	if errW.String() != wantNotice {
		t.Errorf("errW = %q, want %q", errW.String(), wantNotice)
	}
}

// TestContainerFallbackNotice covers containerFallbackNotice's wording
// directly: the generic "no process-backend shim" message for a runtime
// family with genuinely no shim, versus the layers-specific message for
// plans/layers.md Unit D's provided.*+layers+no-bootstrap carve-out (see
// process.RequiresLayerBootstrap).
func TestContainerFallbackNotice(t *testing.T) {
	t.Run("runtime with no process-backend shim at all", func(t *testing.T) {
		fn := discovery.Function{Name: "legacy", Runtime: "java21"}

		got := containerFallbackNotice(fn)
		want := "function legacy: runtime java21 has no process-backend shim — running in a container\n"
		if got != want {
			t.Errorf("containerFallbackNotice() = %q, want %q", got, want)
		}
	})

	t.Run("provided.* with layers and no local bootstrap", func(t *testing.T) {
		fn := discovery.Function{
			Name:     "bref-fn",
			Runtime:  "provided.al2023",
			Dir:      t.TempDir(),
			Manifest: &manifest.Manifest{Layers: []string{"arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1"}},
		}

		got := containerFallbackNotice(fn)
		want := "function bref-fn: runtime provided.al2023 has no local ./bootstrap and its layers: entries must run on Amazon Linux — running in a container\n"
		if got != want {
			t.Errorf("containerFallbackNotice() = %q, want %q", got, want)
		}
	})
}

// TestPerFunctionBackendResolveFallbackErrorCombinesBothFacts checks that
// when the process backend doesn't support fn's runtime *and* the container
// fallback itself fails to resolve, resolve returns an error naming both
// facts while keeping process.ErrRuntimeNotSupported errors.Is-matchable.
func TestPerFunctionBackendResolveFallbackErrorCombinesBothFacts(t *testing.T) {
	global, _, _ := fakeBackends()
	containerErr := errors.New("no usable container runtime")

	r := &perFunctionBackend{
		mode:       "process",
		global:     global,
		globalKind: backendKindProcess,
		errW:       &bytes.Buffer{},
		resolveContainer: func(context.Context) (backend.Backend, error) {
			return nil, containerErr
		},
	}

	_, err := r.resolve(context.Background(), fnWithRuntime("legacy", "", "java21"))
	if err == nil {
		t.Fatal("resolve() expected an error, got nil")
	}
	if !errors.Is(err, process.ErrRuntimeNotSupported) {
		t.Errorf("resolve() error = %v, want errors.Is process.ErrRuntimeNotSupported", err)
	}
	if !errors.Is(err, containerErr) {
		t.Errorf("resolve() error = %v, want errors.Is the container fallback failure %v", err, containerErr)
	}
}

// TestPerFunctionBackendResolveSupportedRuntimeNoFallback checks that a
// process-kind global backend is returned unchanged, with no notice, for
// runtimes process.Supports accepts (nodejs22.x) and for a function whose
// manifest sets local.command regardless of its runtime.
func TestPerFunctionBackendResolveSupportedRuntimeNoFallback(t *testing.T) {
	global, containerB, _ := fakeBackends()

	fns := []discovery.Function{
		fnWithRuntime("node-fn", "", "nodejs22.x"),
		{
			Name:     "custom-fn",
			Runtime:  "java21",
			Manifest: &manifest.Manifest{Local: manifest.Local{Command: "./my-custom-runtime"}},
		},
	}

	for _, fn := range fns {
		var errW bytes.Buffer
		r := &perFunctionBackend{
			mode:       "process",
			global:     global,
			globalKind: backendKindProcess,
			errW:       &errW,
			resolveContainer: func(context.Context) (backend.Backend, error) {
				return containerB, nil
			},
		}

		got, err := r.resolve(context.Background(), fn)
		if err != nil {
			t.Fatalf("resolve(%s) unexpected error: %v", fn.Name, err)
		}
		if got != global {
			t.Errorf("resolve(%s) = %v, want the global backend (no fallback)", fn.Name, got)
		}
		if errW.String() != "" {
			t.Errorf("resolve(%s) wrote a notice %q, want none", fn.Name, errW.String())
		}
	}
}

// TestPerFunctionBackendResolveFallbackNoticeOncePerFunction checks the
// once-per-function notice guard: resolve runs again on every cold start
// (see router.Manager), so the same function resolving twice must only
// print the fallback notice once.
func TestPerFunctionBackendResolveFallbackNoticeOncePerFunction(t *testing.T) {
	global, containerB, _ := fakeBackends()
	var errW bytes.Buffer
	calls := 0

	r := &perFunctionBackend{
		mode:       "process",
		global:     global,
		globalKind: backendKindProcess,
		errW:       &errW,
		resolveContainer: func(context.Context) (backend.Backend, error) {
			calls++
			return containerB, nil
		},
	}

	fn := fnWithRuntime("legacy", "", "java21")

	for i := 0; i < 2; i++ {
		got, err := r.resolve(context.Background(), fn)
		if err != nil {
			t.Fatalf("resolve() call %d unexpected error: %v", i, err)
		}
		if got != containerB {
			t.Errorf("resolve() call %d = %v, want the container backend", i, got)
		}
	}

	wantNotice := "function legacy: runtime java21 has no process-backend shim — running in a container\n"
	if errW.String() != wantNotice {
		t.Errorf("errW = %q, want it printed exactly once: %q", errW.String(), wantNotice)
	}
	if calls != 1 {
		t.Errorf("resolveContainer called %d times, want 1 (cached across resolves)", calls)
	}
}
