package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
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

func fnWithBackend(name, target string) discovery.Function {
	return discovery.Function{Name: name, Backend: target}
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
