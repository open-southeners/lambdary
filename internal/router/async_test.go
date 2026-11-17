package router

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/metrics"
)

// flakyBackend starts instances that fail with a function error for their
// first failures invocations and succeed afterwards.
type flakyBackend struct {
	failures int32
	calls    atomic.Int32
}

func (b *flakyBackend) Start(context.Context, discovery.Function) (backend.Instance, error) {
	inst := &flakyInstance{b: b}
	inst.srv = httptest.NewServer(http.HandlerFunc(inst.handle))

	return inst, nil
}

type flakyInstance struct {
	b   *flakyBackend
	srv *httptest.Server
}

func (i *flakyInstance) handle(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body) //nolint:errcheck // test double.

	w.Header().Set("Content-Type", "application/json")

	if i.b.calls.Add(1) <= i.b.failures {
		w.Write([]byte(`{"errorType":"Boom","errorMessage":"nope"}`)) //nolint:errcheck // test double.
		return
	}

	w.Write([]byte(`{"ok":true}`)) //nolint:errcheck // test double.
}

func (i *flakyInstance) InvokeURL() string          { return i.srv.URL }
func (i *flakyInstance) Stop(context.Context) error { i.srv.Close(); return nil }
func (i *flakyInstance) Logs() io.Reader            { return strings.NewReader("") }
func (b *flakyBackend) attempts() int               { return int(b.calls.Load()) }
func asyncRouter(b backend.Backend, rec *metrics.Recorder, opts Options) *Router {
	fns := []discovery.Function{fn("greet")}
	opts.Metrics = rec

	return NewServer(NewManager(b, fns), fns, opts)
}

func invokeWithType(h http.Handler, name, invocationType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/"+name+"/invocations", strings.NewReader(body))
	if invocationType != "" {
		req.Header.Set(invocationTypeHeader, invocationType)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

var fastRetries = []time.Duration{time.Millisecond, time.Millisecond}

func TestEventInvokeAnswers202AndRunsInBackground(t *testing.T) {
	b := newFakeBackend()
	rt := asyncRouter(b, metrics.New(), Options{AsyncRetryDelays: fastRetries})

	got := invokeWithType(rt, "greet", "Event", `{"n":1}`)
	if got.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", got.Code)
	}

	if got.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", got.Body.String())
	}

	if v := got.Header().Get("X-Amz-Executed-Version"); v != "$LATEST" {
		t.Errorf("X-Amz-Executed-Version = %q, want $LATEST", v)
	}

	waitUntil(t, func() bool { return b.instance("greet") != nil && b.instance("greet").invocationCount() == 1 }, "the background invocation")

	if body := b.instance("greet").lastInvocationBody(); !bytes.Equal(body, []byte(`{"n":1}`)) {
		t.Errorf("invoked with %s, want the request body", body)
	}

	if err := rt.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestEventInvokeRetriesFunctionErrorsUntilSuccess(t *testing.T) {
	fb := &flakyBackend{failures: 1}
	rec := metrics.New()
	rt := asyncRouter(fb, rec, Options{AsyncRetryDelays: fastRetries})

	if got := invokeWithType(rt, "greet", "Event", `{}`); got.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", got.Code)
	}

	waitUntil(t, func() bool { return fb.attempts() == 2 }, "the retry")

	if err := rt.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := fb.attempts(); got != 2 {
		t.Errorf("attempts = %d, want 2 (stop on success)", got)
	}

	if got := queryTotal(rec, metrics.Invocations, "greet", ""); got != 2 {
		t.Errorf("Invocations = %v, want 2", got)
	}

	if got := queryTotal(rec, metrics.Errors, "greet", ""); got != 1 {
		t.Errorf("Errors = %v, want 1", got)
	}
}

func TestEventInvokeGivesUpAfterTwoRetries(t *testing.T) {
	fb := &flakyBackend{failures: 100}

	var logged syncLog

	rt := asyncRouter(fb, metrics.New(), Options{AsyncRetryDelays: fastRetries, Log: &logged})

	invokeWithType(rt, "greet", "Event", `{}`)

	waitUntil(t, func() bool { return strings.Contains(logged.String(), "failed after 3 attempts") }, "the failure log")

	if err := rt.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := fb.attempts(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}

	if !strings.Contains(logged.String(), "greet") || !strings.Contains(logged.String(), "Boom") {
		t.Errorf("log = %q, want the function and the error", logged.String())
	}
}

func TestEventInvokeDefaultsToTwoRetries(t *testing.T) {
	rt := New(NewManager(newFakeBackend(), []discovery.Function{fn("greet")}), []discovery.Function{fn("greet")}).(*Router)

	if len(rt.retryDelays) != 2 || rt.retryDelays[0] != time.Second || rt.retryDelays[1] != 2*time.Second {
		t.Errorf("retryDelays = %v, want [1s 2s]", rt.retryDelays)
	}

	rt = asyncRouter(newFakeBackend(), nil, Options{AsyncRetryDelays: []time.Duration{}})
	if len(rt.retryDelays) != 0 {
		t.Errorf("retryDelays = %v, want none for an empty setting", rt.retryDelays)
	}
}

func TestDryRunValidatesAndRunsNothing(t *testing.T) {
	b := newFakeBackend()
	rt := asyncRouter(b, nil, Options{})

	got := invokeWithType(rt, "greet", "DryRun", `{}`)
	if got.Code != http.StatusNoContent || got.Body.Len() != 0 {
		t.Fatalf("DryRun = %d %q, want 204 with no body", got.Code, got.Body.String())
	}

	if n := b.startCount("greet"); n != 0 {
		t.Errorf("DryRun started the function %d times", n)
	}

	if got := invokeWithType(rt, "missing", "DryRun", `{}`); got.Code != http.StatusNotFound {
		t.Errorf("DryRun of an unknown function = %d, want 404", got.Code)
	}
}

func TestInvocationTypeValidation(t *testing.T) {
	b := newFakeBackend()
	rt := asyncRouter(b, nil, Options{})

	got := invokeWithType(rt, "greet", "Sometimes", `{}`)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got.Code)
	}

	if typ := got.Header().Get(errorTypeHeader); typ != errInvalidParameter {
		t.Errorf("error type = %q, want %s", typ, errInvalidParameter)
	}

	if !strings.Contains(got.Body.String(), "Sometimes") {
		t.Errorf("body = %s, want it to name the bad value", got.Body.String())
	}

	if got := invokeWithType(rt, "missing", "Event", `{}`); got.Code != http.StatusNotFound {
		t.Errorf("Event of an unknown function = %d, want 404", got.Code)
	}

	if got := invokeWithType(rt, "greet:nope", "Event", `{}`); got.Code != http.StatusNotFound {
		t.Errorf("Event of an unknown qualifier = %d, want 404", got.Code)
	}

	for _, typ := range []string{"", "RequestResponse"} {
		got := invokeWithType(rt, "greet", typ, `{}`)
		if got.Code != http.StatusOK {
			t.Errorf("InvocationType %q = %d, want 200", typ, got.Code)
		}
	}
}

func TestShutdownCancelsPendingRetries(t *testing.T) {
	fb := &flakyBackend{failures: 100}
	rt := asyncRouter(fb, nil, Options{AsyncRetryDelays: []time.Duration{time.Hour, time.Hour}})

	invokeWithType(rt, "greet", "Event", `{}`)

	waitUntil(t, func() bool { return fb.attempts() == 1 }, "the first attempt")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rt.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown waited for the retry delay: %v", err)
	}

	if got := fb.attempts(); got != 1 {
		t.Errorf("attempts = %d, want 1 (retries cancelled)", got)
	}

	if got := invokeWithType(rt, "greet", "Event", `{}`); got.Code != http.StatusServiceUnavailable {
		t.Errorf("Event after Shutdown = %d, want 503", got.Code)
	}

	if err := rt.InvokeAsync(context.Background(), "greet", nil); err == nil {
		t.Error("InvokeAsync after Shutdown succeeded")
	}
}

func TestInvokeAsyncValidatesAndRetries(t *testing.T) {
	fb := &flakyBackend{failures: 2}
	rt := asyncRouter(fb, nil, Options{AsyncRetryDelays: fastRetries})

	if err := rt.InvokeAsync(context.Background(), "missing", nil); err == nil {
		t.Error("InvokeAsync of an unknown function succeeded")
	}

	if err := rt.InvokeAsync(context.Background(), "greet", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, func() bool { return fb.attempts() == 3 }, "both retries")

	if err := rt.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// syncLog is a goroutine-safe log sink.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}
