package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
