package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/event"
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

func TestRouterRouteEventMapping(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newEchoInstance(http.StatusOK)
	}

	fns := []discovery.Function{fn("hello")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/hello/users?x=1&x=2", nil)
	req.Header.Add("X-Custom", "a")
	req.Header.Add("X-Custom", "b")
	req.AddCookie(&http.Cookie{Name: "session", Value: "abc"})
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var ev event.RequestV2
	decodeJSON(t, rec, &ev)

	if ev.Version != "2.0" {
		t.Errorf("version = %q, want %q", ev.Version, "2.0")
	}
	if ev.RawPath != "/users" {
		t.Errorf("rawPath = %q, want %q (route prefix stripped)", ev.RawPath, "/users")
	}
	if ev.RawQueryString != "x=1&x=2" {
		t.Errorf("rawQueryString = %q, want %q (verbatim)", ev.RawQueryString, "x=1&x=2")
	}
	if want := []string{"session=abc", "theme=dark"}; !equalStrings(ev.Cookies, want) {
		t.Errorf("cookies = %v, want %v", ev.Cookies, want)
	}
	if got, want := ev.Headers["x-custom"], "a,b"; got != want {
		t.Errorf("headers[x-custom] = %q, want %q (joined)", got, want)
	}
	if got, want := ev.QueryStringParameters["x"], "1,2"; got != want {
		t.Errorf("queryStringParameters[x] = %q, want %q (joined)", got, want)
	}
	if got, want := ev.RequestContext.HTTP.Path, "/hello/users"; got != want {
		t.Errorf("requestContext.http.path = %q, want %q (unstripped)", got, want)
	}
	if got, want := ev.RequestContext.HTTP.Method, http.MethodGet; got != want {
		t.Errorf("requestContext.http.method = %q, want %q", got, want)
	}
}

func TestRouterRouteShapedResponse(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		shaped := `{"statusCode":201,"headers":{"X-Extra":"yes"},"cookies":["a=1","b=2"],"body":"aGVsbG8=","isBase64Encoded":true}`
		return newFakeInstance(http.StatusOK, []byte(shaped), "application/json", 0)
	}

	fns := []discovery.Function{fn("shaped")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/shaped", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("X-Extra"), "yes"; got != want {
		t.Errorf("X-Extra = %q, want %q", got, want)
	}
	if got, want := rec.Result().Cookies(), 2; len(got) != want {
		t.Fatalf("Set-Cookie count = %d, want %d", len(got), want)
	}
	if got, want := rec.Body.String(), "hello"; got != want {
		t.Errorf("body = %q, want %q (base64-decoded)", got, want)
	}
}

func TestRouterRouteBinaryRequestRoundTrips(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newEchoInstance(http.StatusOK)
	}

	fns := []discovery.Function{fn("img")}
	h := New(NewManager(b, fns), fns)

	binary := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x01, 0xFF}
	req := httptest.NewRequest(http.MethodPost, "/img", bytes.NewReader(binary))
	req.Header.Set("Content-Type", "image/png")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var ev event.RequestV2
	decodeJSON(t, rec, &ev)

	if !ev.IsBase64Encoded {
		t.Fatal("isBase64Encoded = false, want true for binary content")
	}
	decoded, err := base64.StdEncoding.DecodeString(ev.Body)
	if err != nil {
		t.Fatalf("decoding event body: %v", err)
	}
	if !bytes.Equal(decoded, binary) {
		t.Errorf("body round-trip = %x, want %x", decoded, binary)
	}
}

func TestRouterRoutePayload10Returns501(t *testing.T) {
	legacy := fn("legacy")
	legacy.Manifest = &manifest.Manifest{URL: manifest.URL{Payload: "1.0"}}

	fns := []discovery.Function{legacy}
	h := New(NewManager(newFakeBackend(), fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/legacy", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Message string `json:"message"`
	}
	decodeJSON(t, rec, &payload)
	want := "payload format 1.0 is not yet supported — see CURRENT_ISSUES.md"
	if payload.Message != want {
		t.Errorf("message = %q, want %q", payload.Message, want)
	}
}

func TestRouterRoutePayload20Works(t *testing.T) {
	modern := fn("modern")
	modern.Manifest = &manifest.Manifest{URL: manifest.URL{Payload: "2.0"}}

	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newEchoInstance(http.StatusOK)
	}

	fns := []discovery.Function{modern}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/modern", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRouterRouteFunctionErrorEnvelope(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte(`{"errorType":"ValueError","errorMessage":"boom"}`), "application/json", 0)
	}

	fns := []discovery.Function{fn("boom")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Message string `json:"message"`
		Error   struct {
			ErrorType    string `json:"errorType"`
			ErrorMessage string `json:"errorMessage"`
		} `json:"error"`
	}
	decodeJSON(t, rec, &payload)
	if payload.Message != "function error" {
		t.Errorf("message = %q, want %q", payload.Message, "function error")
	}
	if payload.Error.ErrorType != "ValueError" || payload.Error.ErrorMessage != "boom" {
		t.Errorf("error = %+v, want envelope carried through", payload.Error)
	}
}

func TestRouterRouteNon2xxIsFunctionError(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusInternalServerError, []byte(`{"message":"internal RIE error"}`), "application/json", 0)
	}

	fns := []discovery.Function{fn("fail")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/fail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRouterRouteBadShapedResponseIs502(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		bad := `{"statusCode":200,"body":"not-valid-base64!!","isBase64Encoded":true}`
		return newFakeInstance(http.StatusOK, []byte(bad), "application/json", 0)
	}

	fns := []discovery.Function{fn("badbody")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/badbody", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRouterRouteDeepPathStripsPrefix(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newEchoInstance(http.StatusOK)
	}

	fns := []discovery.Function{fn("hello")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/hello/a/b/c", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var ev event.RequestV2
	decodeJSON(t, rec, &ev)
	if ev.RawPath != "/a/b/c" {
		t.Errorf("rawPath = %q, want %q", ev.RawPath, "/a/b/c")
	}
}

func TestRouterRouteBarePathIsRoot(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newEchoInstance(http.StatusOK)
	}

	fns := []discovery.Function{fn("hello")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var ev event.RequestV2
	decodeJSON(t, rec, &ev)
	if ev.RawPath != "/" {
		t.Errorf("rawPath = %q, want %q (bare route)", ev.RawPath, "/")
	}
}

func TestRouterRouteTimeout(t *testing.T) {
	slow := fn("slow")
	slow.Manifest = &manifest.Manifest{Timeout: 1}

	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte("ok"), "", 2*time.Second)
	}

	fns := []discovery.Function{slow}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/slow", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body = %s", rec.Code, rec.Body.String())
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
