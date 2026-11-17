package router

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/metrics"
)

const startFailDetail = "container exited with code 255: exec /var/runtime/bootstrap: exec format error"

// startFailBackend's Start always fails as a function container that exits
// at once does.
type startFailBackend struct{ starts atomic.Int32 }

func (b *startFailBackend) Start(_ context.Context, fn discovery.Function) (backend.Instance, error) {
	b.starts.Add(1)
	return nil, &backend.StartFailedError{Function: fn.Name, Detail: startFailDetail}
}

func TestInvokeStartFailureIsServiceExceptionAndNotRecorded(t *testing.T) {
	rec := metrics.New()
	rt := asyncRouter(&startFailBackend{}, rec, Options{})

	got := invokeWithType(rt, "greet", "", `{}`)
	if got.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", got.Code)
	}

	if h := got.Header().Get(errorTypeHeader); h != "ServiceException" {
		t.Errorf("%s = %q, want ServiceException", errorTypeHeader, h)
	}

	var body map[string]string
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if body["Type"] != "Service" || !strings.Contains(body["Message"], startFailDetail) {
		t.Errorf("body = %v, want Type Service and the detailed message", body)
	}

	if n := queryTotal(rec, metrics.Invocations, "greet", ""); n != 0 {
		t.Errorf("Invocations = %v, want 0", n)
	}

	if n := queryTotal(rec, metrics.Errors, "greet", ""); n != 0 {
		t.Errorf("Errors = %v, want 0", n)
	}
}

func TestRouterInvokeStartFailureNotRecorded(t *testing.T) {
	rec := metrics.New()
	rt := asyncRouter(&startFailBackend{}, rec, Options{})

	if _, err := rt.Invoke(context.Background(), "greet", nil); err == nil || !strings.Contains(err.Error(), startFailDetail) {
		t.Fatalf("Invoke error = %v, want the start failure", err)
	}

	if n := queryTotal(rec, metrics.Invocations, "greet", ""); n != 0 {
		t.Errorf("Invocations = %v, want 0", n)
	}
}

func TestFunctionRouteStartFailureIs502WithMessage(t *testing.T) {
	rec := metrics.New()
	rt := asyncRouter(&startFailBackend{}, rec, Options{})

	got := serve(rt, http.MethodGet, "/greet", "")
	if got.Code != http.StatusBadGateway || !strings.Contains(got.Body.String(), startFailDetail) {
		t.Errorf("got %d %s, want 502 with the detailed message", got.Code, got.Body.String())
	}

	if n := queryTotal(rec, metrics.Invocations, "greet", ""); n != 0 {
		t.Errorf("Invocations = %v, want 0", n)
	}
}

func TestAsyncStartFailureRetriesLogsAndIsNotRecorded(t *testing.T) {
	b := &startFailBackend{}
	rec := metrics.New()

	var logged syncLog

	rt := asyncRouter(b, rec, Options{AsyncRetryDelays: fastRetries, Log: &logged})

	invokeWithType(rt, "greet", "Event", `{}`)

	waitUntil(t, func() bool { return strings.Contains(logged.String(), "failed after 3 attempts") }, "the failure log")

	if err := rt.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := b.starts.Load(); got != 3 {
		t.Errorf("starts = %d, want 3 attempts", got)
	}

	if !strings.Contains(logged.String(), startFailDetail) {
		t.Errorf("log = %q, want the detailed message", logged.String())
	}

	if n := queryTotal(rec, metrics.Invocations, "greet", ""); n != 0 {
		t.Errorf("Invocations = %v, want 0", n)
	}
}
