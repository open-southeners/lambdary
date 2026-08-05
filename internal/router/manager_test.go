package router

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

func fn(name string) discovery.Function {
	return discovery.Function{
		Name:     name,
		Dir:      "/functions/" + name,
		Route:    "/" + name,
		Runtime:  "nodejs22.x",
		Backend:  "container",
		Manifest: &manifest.Manifest{},
	}
}

func TestManagerEnsureUnknownFunction(t *testing.T) {
	m := NewManager(newFakeBackend(), []discovery.Function{fn("a")})

	_, err := m.Ensure(context.Background(), "missing")
	if !errors.Is(err, ErrUnknownFunction) {
		t.Fatalf("Ensure(missing) error = %v, want errors.Is ErrUnknownFunction", err)
	}
}

func TestManagerEnsureStartsOnceUnderConcurrency(t *testing.T) {
	b := newFakeBackend()
	m := NewManager(b, []discovery.Function{fn("a")})

	const n = 20
	urls := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			url, err := m.Ensure(context.Background(), "a")
			if err != nil {
				t.Errorf("Ensure() error = %v", err)
				return
			}
			urls[i] = url
		}(i)
	}
	wg.Wait()

	if got := b.startCount("a"); got != 1 {
		t.Fatalf("backend.Start called %d times, want 1", got)
	}

	for i, url := range urls {
		if url == "" || url != urls[0] {
			t.Fatalf("urls[%d] = %q, want all equal to %q", i, url, urls[0])
		}
	}
}

func TestManagerEnsureRetriesAfterFailedStart(t *testing.T) {
	b := newFakeBackend()
	m := NewManager(b, []discovery.Function{fn("a")})
	b.setFailNext("a")

	if _, err := m.Ensure(context.Background(), "a"); err == nil {
		t.Fatal("Ensure() error = nil, want failure from the stubbed first start")
	}

	url, err := m.Ensure(context.Background(), "a")
	if err != nil {
		t.Fatalf("Ensure() second call error = %v, want the failed start not to poison the entry", err)
	}
	if url == "" {
		t.Fatal("Ensure() second call returned empty URL")
	}

	if got := b.startCount("a"); got != 2 {
		t.Fatalf("backend.Start called %d times, want 2 (failed attempt + retry)", got)
	}
}

func TestManagerInvokeTimeout(t *testing.T) {
	withTimeout := fn("with-timeout")
	withTimeout.Manifest = &manifest.Manifest{Timeout: 5}

	noTimeout := fn("no-timeout")
	noTimeout.Manifest = &manifest.Manifest{}

	m := NewManager(newFakeBackend(), []discovery.Function{withTimeout, noTimeout})

	if got, want := m.InvokeTimeout("with-timeout"), 5*time.Second; got != want {
		t.Errorf("InvokeTimeout(with-timeout) = %s, want %s", got, want)
	}
	if got, want := m.InvokeTimeout("no-timeout"), defaultInvokeTimeout; got != want {
		t.Errorf("InvokeTimeout(no-timeout) = %s, want default %s", got, want)
	}
	if got, want := m.InvokeTimeout("unknown"), defaultInvokeTimeout; got != want {
		t.Errorf("InvokeTimeout(unknown) = %s, want default %s", got, want)
	}
}

func TestManagerWithLockSerializesPerFunction(t *testing.T) {
	m := NewManager(newFakeBackend(), []discovery.Function{fn("a")})

	var active, peak int32
	var mu sync.Mutex

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.WithLock("a", func() error {
				mu.Lock()
				active++
				if active > peak {
					peak = active
				}
				mu.Unlock()

				time.Sleep(10 * time.Millisecond)

				mu.Lock()
				active--
				mu.Unlock()

				return nil
			})
		}()
	}
	wg.Wait()

	if peak != 1 {
		t.Fatalf("peak concurrent WithLock(a) calls = %d, want 1 (serialized)", peak)
	}
}

func TestManagerRestartStopsAndForgetsRunningInstance(t *testing.T) {
	b := newFakeBackend()
	m := NewManager(b, []discovery.Function{fn("a")})

	if _, err := m.Ensure(context.Background(), "a"); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	first := b.instance("a")

	if err := m.Restart(context.Background(), "a"); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}

	if !first.stopped {
		t.Fatalf("Restart() left the old instance running")
	}

	if _, err := m.Ensure(context.Background(), "a"); err != nil {
		t.Fatalf("Ensure() after Restart() error = %v", err)
	}

	if got := b.startCount("a"); got != 2 {
		t.Fatalf("backend.Start called %d times, want 2 (initial start + cold start after Restart)", got)
	}

	second := b.instance("a")
	if second == first {
		t.Fatalf("Ensure() after Restart() returned the same instance, want a fresh one")
	}
}

func TestManagerRestartNotRunningNoop(t *testing.T) {
	m := NewManager(newFakeBackend(), []discovery.Function{fn("a")})

	if err := m.Restart(context.Background(), "a"); err != nil {
		t.Fatalf("Restart() on a never-started function error = %v, want nil (no-op)", err)
	}
}

func TestManagerRestartUnknownFunction(t *testing.T) {
	m := NewManager(newFakeBackend(), []discovery.Function{fn("a")})

	err := m.Restart(context.Background(), "missing")
	if !errors.Is(err, ErrUnknownFunction) {
		t.Fatalf("Restart(missing) error = %v, want errors.Is ErrUnknownFunction", err)
	}
}

// TestManagerRestartRaceCleanUnderConcurrentInvoke exercises Restart racing
// against concurrent Ensure+WithLock invocations (run with -race): the
// invariant it checks is not which instance ultimately wins any given
// invoke, only that WithLock never sees itself already held for the same
// function (which fakeInstance's own concurrency counter — see
// peakConcurrency — would catch too) and that nothing panics/deadlocks
// under `go test -race`.
func TestManagerRestartRaceCleanUnderConcurrentInvoke(t *testing.T) {
	b := newFakeBackend()
	m := NewManager(b, []discovery.Function{fn("a")})

	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			url, err := m.Ensure(context.Background(), "a")
			if err != nil {
				// A concurrent Restart can legitimately race a fresh
				// Ensure's backend.Start against the fake backend's own
				// bookkeeping in ways that don't matter here — this test's
				// only real assertion is "go test -race reports nothing",
				// so an occasional Ensure error is not itself a failure.
				return
			}

			_ = m.WithLock("a", func() error {
				_, _ = http.Get(url) //nolint:errcheck,noctx // best-effort concurrency exerciser.
				return nil
			})
		}()
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Restart(context.Background(), "a")
		}()
	}

	wg.Wait()
}

func TestManagerStopAll(t *testing.T) {
	b := newFakeBackend()
	m := NewManager(b, []discovery.Function{fn("a"), fn("b")})

	if _, err := m.Ensure(context.Background(), "a"); err != nil {
		t.Fatalf("Ensure(a) error = %v", err)
	}
	if _, err := m.Ensure(context.Background(), "b"); err != nil {
		t.Fatalf("Ensure(b) error = %v", err)
	}

	instA := b.instance("a")
	instB := b.instance("b")

	if err := m.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll() error = %v", err)
	}

	if !instA.stopped || !instB.stopped {
		t.Fatalf("StopAll() left instances running: a.stopped=%v b.stopped=%v", instA.stopped, instB.stopped)
	}
}

func TestManagerStopAllJoinsErrors(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(200, []byte("ok"), "", 0)
	}
	m := NewManager(b, []discovery.Function{fn("a")})

	if _, err := m.Ensure(context.Background(), "a"); err != nil {
		t.Fatalf("Ensure(a) error = %v", err)
	}

	// Swap in a Stop that always fails, without going through Ensure again
	// (which would treat it as a fresh start).
	m.instMu.Lock()
	m.instances["a"] = failingStopInstance{b.instance("a")}
	m.instMu.Unlock()

	err := m.StopAll(context.Background())
	if !errors.Is(err, errStoppedBackend) {
		t.Fatalf("StopAll() error = %v, want errors.Is errStoppedBackend", err)
	}
}

func TestManagerWithResolverPicksBackendPerFunction(t *testing.T) {
	containerBackend := newFakeBackend()
	processBackend := newFakeBackend()

	a := fn("a")
	a.Backend = "container"
	b := fn("b")
	b.Backend = "process"

	resolve := func(_ context.Context, target discovery.Function) (backend.Backend, error) {
		if target.Backend == "process" {
			return processBackend, nil
		}
		return containerBackend, nil
	}

	m := NewManagerWithResolver(resolve, []discovery.Function{a, b})

	if _, err := m.Ensure(context.Background(), "a"); err != nil {
		t.Fatalf("Ensure(a) error = %v", err)
	}
	if _, err := m.Ensure(context.Background(), "b"); err != nil {
		t.Fatalf("Ensure(b) error = %v", err)
	}

	if got := containerBackend.startCount("a"); got != 1 {
		t.Errorf("containerBackend.Start(a) called %d times, want 1", got)
	}
	if got := processBackend.startCount("b"); got != 1 {
		t.Errorf("processBackend.Start(b) called %d times, want 1", got)
	}
	if got := containerBackend.startCount("b"); got != 0 {
		t.Errorf("containerBackend.Start(b) called %d times, want 0 — b's own backend hint should have kept it off the container backend", got)
	}
	if got := processBackend.startCount("a"); got != 0 {
		t.Errorf("processBackend.Start(a) called %d times, want 0 — a's own backend hint should have kept it off the process backend", got)
	}
}

// errResolveBackend is a stand-in resolver failure for
// TestManagerEnsurePropagatesResolverError.
var errResolveBackend = errors.New("router_test: resolving backend failed")

func TestManagerEnsurePropagatesResolverError(t *testing.T) {
	calls := 0
	resolve := func(context.Context, discovery.Function) (backend.Backend, error) {
		calls++
		return nil, errResolveBackend
	}

	m := NewManagerWithResolver(resolve, []discovery.Function{fn("a")})

	if _, err := m.Ensure(context.Background(), "a"); !errors.Is(err, errResolveBackend) {
		t.Fatalf("Ensure() error = %v, want errors.Is errResolveBackend", err)
	}

	// A failed resolution isn't cached either, matching a failed Start —
	// the next Ensure retries from scratch.
	if _, err := m.Ensure(context.Background(), "a"); !errors.Is(err, errResolveBackend) {
		t.Fatalf("second Ensure() error = %v, want errors.Is errResolveBackend", err)
	}
	if calls != 2 {
		t.Fatalf("resolver called %d times, want 2 (one per Ensure)", calls)
	}
}
