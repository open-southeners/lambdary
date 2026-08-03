package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
)

// fakeBackend is a backend.Backend test double that records every Start
// call (so tests can assert lazy-start-once behaviour) and hands back
// fakeInstances backed by a real httptest.Server, so the router's
// passthrough exercises a genuine HTTP round trip end to end.
type fakeBackend struct {
	mu sync.Mutex

	// starts counts Start calls per function name, including failed ones.
	starts map[string]int
	// failNext, when true for a name, makes the next Start for that name
	// fail once (and reset to false), so tests can exercise the
	// failed-start-doesn't-poison-the-entry retry path.
	failNext map[string]bool
	// instances records the fakeInstance handed back for each name, for
	// tests to inspect (invocation bodies, concurrency counters) after the
	// fact.
	instances map[string]*fakeInstance

	// newInstance builds the instance Start returns for a given function
	// name; defaults to a canned 200 "ok" response when nil.
	newInstance func(name string) *fakeInstance
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		starts:    make(map[string]int),
		failNext:  make(map[string]bool),
		instances: make(map[string]*fakeInstance),
	}
}

func (b *fakeBackend) Start(_ context.Context, fn discovery.Function) (backend.Instance, error) {
	b.mu.Lock()
	b.starts[fn.Name]++

	if b.failNext[fn.Name] {
		b.failNext[fn.Name] = false
		b.mu.Unlock()
		return nil, errors.New("fake backend: start failed")
	}
	b.mu.Unlock()

	build := b.newInstance
	if build == nil {
		build = func(string) *fakeInstance {
			return newFakeInstance(http.StatusOK, []byte(`{"ok":true}`), "application/json", 0)
		}
	}
	inst := build(fn.Name)

	b.mu.Lock()
	b.instances[fn.Name] = inst
	b.mu.Unlock()

	return inst, nil
}

// startCount reports how many times Start was called for name.
func (b *fakeBackend) startCount(name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts[name]
}

// setFailNext arranges for the next Start of name to fail.
func (b *fakeBackend) setFailNext(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failNext[name] = true
}

// instance returns the most recently started fakeInstance for name, or nil
// if it was never started.
func (b *fakeBackend) instance(name string) *fakeInstance {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.instances[name]
}

// fakeInstance is a backend.Instance test double backed by a real
// httptest.Server, so a request through router.Router genuinely travels
// over HTTP end to end (matching how the real container backend's Instance
// works). It records every invocation body and the peak number of
// concurrently in-flight requests, so tests can assert per-function
// serialization.
type fakeInstance struct {
	srv *httptest.Server

	status      int
	body        []byte
	contentType string
	sleep       time.Duration
	// echo, when true, makes handle reply with the exact body it received
	// instead of the fixed body field — used by router-route tests that
	// need to inspect the JSON event the router actually sent, without a
	// second server hop to capture it.
	echo bool
	// v1Echo is echo's payload-1.0 counterpart: handle wraps the exact body
	// it received in a shaped v1 response (statusCode + body) instead of
	// replying with it directly, since event.ToHTTPV1 requires a shaped
	// response and has no bare-JSON fallback the way v2's echo relies on.
	v1Echo bool
	// v1StatusCode is the shaped response's own "statusCode" field when
	// v1Echo is true — deliberately independent of status (the fake
	// server's raw HTTP status), matching how the real RIE always answers
	// HTTP 200 with the shaped statusCode carried inside the JSON body.
	v1StatusCode int

	active    int32
	maxActive int32

	mu          sync.Mutex
	invocations [][]byte
	stopped     bool
}

func newFakeInstance(status int, body []byte, contentType string, sleep time.Duration) *fakeInstance {
	fi := &fakeInstance{status: status, body: body, contentType: contentType, sleep: sleep}
	fi.srv = httptest.NewServer(http.HandlerFunc(fi.handle))

	return fi
}

// newEchoInstance returns a fakeInstance that replies to every invocation
// with the exact request body it received, under status.
func newEchoInstance(status int) *fakeInstance {
	fi := &fakeInstance{status: status, echo: true}
	fi.srv = httptest.NewServer(http.HandlerFunc(fi.handle))

	return fi
}

// newV1EchoInstance returns a fakeInstance that replies to every invocation
// with a shaped v1 response (statusCode: statusCode) whose body is the
// exact JSON event it received, serialized back out as a string — the
// payload-1.0 counterpart to newEchoInstance, letting router v1 tests
// inspect the event.RequestV1 FromHTTPV1 built without a second server hop.
func newV1EchoInstance(statusCode int) *fakeInstance {
	fi := &fakeInstance{status: http.StatusOK, v1Echo: true, v1StatusCode: statusCode}
	fi.srv = httptest.NewServer(http.HandlerFunc(fi.handle))

	return fi
}

func (fi *fakeInstance) handle(w http.ResponseWriter, r *http.Request) {
	n := atomic.AddInt32(&fi.active, 1)
	defer atomic.AddInt32(&fi.active, -1)

	for {
		peak := atomic.LoadInt32(&fi.maxActive)
		if n <= peak {
			break
		}
		if atomic.CompareAndSwapInt32(&fi.maxActive, peak, n) {
			break
		}
	}

	reqBody, _ := io.ReadAll(r.Body)

	fi.mu.Lock()
	fi.invocations = append(fi.invocations, reqBody)
	fi.mu.Unlock()

	if fi.sleep > 0 {
		select {
		case <-time.After(fi.sleep):
		case <-r.Context().Done():
			return
		}
	}

	respBody := fi.body
	switch {
	case fi.echo:
		respBody = reqBody
	case fi.v1Echo:
		shaped, _ := json.Marshal(map[string]any{ //nolint:errcheck // test double: map[string]any{int, string} always marshals.
			"statusCode": fi.v1StatusCode,
			"body":       string(reqBody),
		})
		respBody = shaped
	}

	if fi.contentType != "" {
		w.Header().Set("Content-Type", fi.contentType)
	}
	w.WriteHeader(fi.status)
	w.Write(respBody) //nolint:errcheck // test double, best-effort write.
}

func (fi *fakeInstance) InvokeURL() string { return fi.srv.URL }

func (fi *fakeInstance) Stop(context.Context) error {
	fi.mu.Lock()
	defer fi.mu.Unlock()

	if fi.stopped {
		return nil
	}
	fi.stopped = true
	fi.srv.Close()

	return nil
}

func (fi *fakeInstance) Logs() io.Reader { return strings.NewReader("") }

// peakConcurrency reports the highest number of simultaneously in-flight
// requests handle observed.
func (fi *fakeInstance) peakConcurrency() int32 { return atomic.LoadInt32(&fi.maxActive) }

// invocationCount reports how many requests handle served.
func (fi *fakeInstance) invocationCount() int {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return len(fi.invocations)
}

// lastInvocationBody returns the body of the most recent invocation, or nil
// if there wasn't one.
func (fi *fakeInstance) lastInvocationBody() []byte {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if len(fi.invocations) == 0 {
		return nil
	}
	return fi.invocations[len(fi.invocations)-1]
}

// errStoppedBackend is a stand-in Stop failure, for StopAll error-join
// tests.
var errStoppedBackend = errors.New("fake instance: stop failed")

// failingStopInstance wraps a fakeInstance so Stop always fails, without
// otherwise changing its behaviour.
type failingStopInstance struct {
	*fakeInstance
}

func (f failingStopInstance) Stop(context.Context) error {
	return errStoppedBackend
}
