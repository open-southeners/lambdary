package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// decodeJSON decodes an httptest.ResponseRecorder's body into v, failing
// the test on malformed JSON.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding response body %q: %v", rec.Body.String(), err)
	}
}

func TestRouterInvokePassthroughHappyPath(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte(`{"echoed":true}`), "application/json", 0)
	}

	fns := []discovery.Function{fn("greet")}
	h := New(NewManager(b, fns), fns)

	body := `{"hello":"world"}`
	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/greet/invocations", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Body.String(), `{"echoed":true}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	inst := b.instance("greet")
	if inst == nil {
		t.Fatal("backend was never started")
	}
	if got := inst.lastInvocationBody(); string(got) != body {
		t.Errorf("upstream received body %q, want %q (forwarded verbatim)", got, body)
	}
}

func TestRouterInvokeUnknownFunction(t *testing.T) {
	fns := []discovery.Function{fn("a"), fn("b")}
	h := New(NewManager(newFakeBackend(), fns), fns)

	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/missing/invocations", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Message   string   `json:"message"`
		Functions []string `json:"functions"`
	}
	decodeJSON(t, rec, &payload)

	if payload.Message != "function not found: missing" {
		t.Errorf("message = %q, want %q", payload.Message, "function not found: missing")
	}
	if got, want := payload.Functions, []string{"a", "b"}; !equalStrings(got, want) {
		t.Errorf("functions = %v, want %v", got, want)
	}
}

func TestRouterInvokeNameCollision(t *testing.T) {
	dup1 := fn("dup")
	dup1.Dir = "/functions/dup1"
	dup2 := fn("dup")
	dup2.Dir = "/functions/dup2"

	fns := []discovery.Function{dup1, dup2}
	h := New(NewManager(newFakeBackend(), fns), fns)

	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/dup/invocations", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Functions []string `json:"functions"`
	}
	decodeJSON(t, rec, &payload)
	if got, want := payload.Functions, []string{"/functions/dup1", "/functions/dup2"}; !equalStrings(got, want) {
		t.Errorf("functions = %v, want %v", got, want)
	}
}

func TestRouterRouteCollision(t *testing.T) {
	a := fn("route-a")
	a.Route = "/shared"
	b := fn("route-b")
	b.Route = "/shared"

	fns := []discovery.Function{a, b}
	h := New(NewManager(newFakeBackend(), fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/shared/anything", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Functions []string `json:"functions"`
	}
	decodeJSON(t, rec, &payload)
	if got, want := payload.Functions, []string{"route-a", "route-b"}; !equalStrings(got, want) {
		t.Errorf("functions = %v, want %v", got, want)
	}
}

func TestRouterRouteHitReturns501(t *testing.T) {
	fns := []discovery.Function{fn("greet")}
	h := New(NewManager(newFakeBackend(), fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/greet/sub/path", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Message string `json:"message"`
	}
	decodeJSON(t, rec, &payload)
	want := "HTTP event mapping lands in M2 — invoke via POST /2015-03-31/functions/greet/invocations"
	if payload.Message != want {
		t.Errorf("message = %q, want %q", payload.Message, want)
	}
}

func TestRouterIndex(t *testing.T) {
	fns := []discovery.Function{fn("a"), fn("b")}
	h := New(NewManager(newFakeBackend(), fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Server    string `json:"server"`
		Functions []struct {
			Name    string `json:"name"`
			Route   string `json:"route"`
			Runtime string `json:"runtime"`
			Backend string `json:"backend"`
		} `json:"functions"`
	}
	decodeJSON(t, rec, &payload)

	if payload.Server != "lambdary" {
		t.Errorf("server = %q, want %q", payload.Server, "lambdary")
	}
	if len(payload.Functions) != 2 {
		t.Fatalf("functions count = %d, want 2", len(payload.Functions))
	}
}

func TestRouterUnknownPath404(t *testing.T) {
	fns := []discovery.Function{fn("a")}
	h := New(NewManager(newFakeBackend(), fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRouterSerializesInvokesPerFunction(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte("ok"), "", 30*time.Millisecond)
	}

	fns := []discovery.Function{fn("a")}
	h := New(NewManager(b, fns), fns)

	const n = 4
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/a/invocations", strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		}()
	}
	wg.Wait()

	inst := b.instance("a")
	if inst == nil {
		t.Fatal("backend was never started")
	}
	if got := inst.peakConcurrency(); got != 1 {
		t.Fatalf("peak concurrent invocations to a single function = %d, want 1 (serialized)", got)
	}
	if got := inst.invocationCount(); got != n {
		t.Fatalf("invocation count = %d, want %d", got, n)
	}
}

func TestRouterDifferentFunctionsInvokeConcurrently(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte("ok"), "", 50*time.Millisecond)
	}

	fns := []discovery.Function{fn("a"), fn("b")}
	h := New(NewManager(b, fns), fns)

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan time.Duration, 2)

	for _, name := range []string{"a", "b"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			<-start
			t0 := time.Now()
			req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/"+name+"/invocations", strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			results <- time.Since(t0)
		}(name)
	}
	close(start)
	wg.Wait()
	close(results)

	// Each individual invoke sleeps 50ms; if the two functions ran
	// concurrently rather than being serialized against each other, both
	// should complete well under their combined 100ms.
	for d := range results {
		if d >= 90*time.Millisecond {
			t.Errorf("invoke for a different function took %s, want well under the serial 100ms sum (functions should not block each other)", d)
		}
	}
}

func TestRouterInvokeTimeout(t *testing.T) {
	// Manifest.Timeout is whole seconds (AWS Lambda's own unit), so 1s is
	// the smallest manifest timeout expressible; sleep well past it.
	slow := fn("slow")
	slow.Manifest = &manifest.Manifest{Timeout: 1}

	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte("ok"), "", 2*time.Second)
	}

	fns := []discovery.Function{slow}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/slow/invocations", strings.NewReader("{}"))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body = %s", rec.Code, rec.Body.String())
	}
}

// equalStrings reports whether a and b contain the same strings in the
// same order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
