package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
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

func TestRouterRoutePayload10EventMapping(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newV1EchoInstance(http.StatusOK)
	}

	legacy := fn("legacy")
	legacy.Manifest = &manifest.Manifest{URL: manifest.URL{Payload: "1.0"}}

	fns := []discovery.Function{legacy}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/legacy/sub?a=1&a=2", nil)
	req.Header.Set("X-Custom-Header", "hi")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var ev event.RequestV1
	decodeJSON(t, rec, &ev)

	if ev.Path != "/sub" {
		t.Errorf("path = %q, want %q (route prefix stripped)", ev.Path, "/sub")
	}
	if ev.HTTPMethod != http.MethodGet {
		t.Errorf("httpMethod = %q, want %q", ev.HTTPMethod, http.MethodGet)
	}
	if got := ev.MultiValueHeaders["X-Custom-Header"]; len(got) != 1 || got[0] != "hi" {
		t.Errorf("multiValueHeaders[X-Custom-Header] = %v, want [\"hi\"] (canonical casing preserved, not lowercased)", got)
	}
	if _, lowercased := ev.MultiValueHeaders["x-custom-header"]; lowercased {
		t.Errorf("multiValueHeaders unexpectedly has a lowercased x-custom-header entry, want canonical casing only")
	}
	if got, want := ev.PathParameters["proxy"], "sub"; got != want {
		t.Errorf("pathParameters.proxy = %q, want %q", got, want)
	}
	if ev.RequestContext.Identity.SourceIP == "" {
		t.Errorf("requestContext.identity.sourceIp is empty, want a value")
	}
}

func TestRouterRoutePayload10ShapedResponse(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		shaped := `{"statusCode":201,"headers":{"X-Extra":"yes"},"multiValueHeaders":{"X-Multi":["a","b"]},"body":"hello"}`
		return newFakeInstance(http.StatusOK, []byte(shaped), "application/json", 0)
	}

	legacy := fn("legacy")
	legacy.Manifest = &manifest.Manifest{URL: manifest.URL{Payload: "1.0"}}

	fns := []discovery.Function{legacy}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/legacy", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("X-Extra"), "yes"; got != want {
		t.Errorf("X-Extra = %q, want %q", got, want)
	}
	if got, want := rec.Header().Values("X-Multi"), []string{"a", "b"}; !equalStrings(got, want) {
		t.Errorf("X-Multi = %v, want %v (multiValueHeaders merged additively)", got, want)
	}
	if got, want := rec.Body.String(), "hello"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestRouterRoutePayload10StrictMalformedResponseIs502(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		// An unshaped return value: v2's ToHTTP would fall back to a bare
		// 200 JSON body, but v1's ToHTTPV1 requires a statusCode and has no
		// such fallback.
		return newEchoInstance(http.StatusOK)
	}

	legacy := fn("legacy")
	legacy.Manifest = &manifest.Manifest{URL: manifest.URL{Payload: "1.0"}}

	fns := []discovery.Function{legacy}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/legacy", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Message string `json:"message"`
	}
	decodeJSON(t, rec, &payload)
	if !strings.Contains(payload.Message, "payload 1.0") {
		t.Errorf("message = %q, want it to name payload 1.0", payload.Message)
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

func TestRouterRouteUnsupportedPayloadReturns501(t *testing.T) {
	future := fn("future")
	future.Manifest = &manifest.Manifest{URL: manifest.URL{Payload: "3.0"}}

	fns := []discovery.Function{future}
	h := New(NewManager(newFakeBackend(), fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/future", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Message string `json:"message"`
	}
	decodeJSON(t, rec, &payload)
	want := `payload format 3.0 is not supported — supported values are "2.0" and "1.0"`
	if payload.Message != want {
		t.Errorf("message = %q, want %q", payload.Message, want)
	}
}

// streamFrame builds a raw http-integration-response payload — prelude
// JSON, the eight-NUL delimiter, then body — mirroring what
// awslambda.streamifyResponse produces, for router tests exercising
// RESPONSE_STREAM handling. A router-local copy of event's own unexported
// test helper of the same shape, since event.frameDelimiter isn't
// exported for this package to reuse directly.
func streamFrame(prelude string, body []byte) []byte {
	payload := []byte(prelude)
	payload = append(payload, 0, 0, 0, 0, 0, 0, 0, 0)
	payload = append(payload, body...)

	return payload
}

func TestRouterRouteStreamingFramedPayloadParsed(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		framed := streamFrame(`{"statusCode":202,"headers":{"Content-Type":"text/plain","X-Stream":"yes"},"cookies":["a=1"]}`, []byte("chunk-1;chunk-2;"))
		return newFakeInstance(http.StatusOK, framed, "application/octet-stream", 0)
	}

	streaming := fn("streamfn")
	streaming.Manifest = &manifest.Manifest{URL: manifest.URL{InvokeMode: "RESPONSE_STREAM"}}

	fns := []discovery.Function{streaming}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/streamfn", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (the handler's own status, parsed from the frame's prelude); body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "text/plain"; got != want {
		t.Errorf("Content-Type = %q, want %q (from the prelude, not application/octet-stream's upstream default)", got, want)
	}
	if got, want := rec.Header().Get("X-Stream"), "yes"; got != want {
		t.Errorf("X-Stream = %q, want %q", got, want)
	}
	if got, want := len(rec.Result().Cookies()), 1; got != want {
		t.Fatalf("Set-Cookie count = %d, want %d", got, want)
	}
	if got, want := rec.Body.String(), "chunk-1;chunk-2;"; got != want {
		t.Errorf("body = %q, want %q (the frame's body, with the prelude and delimiter stripped)", got, want)
	}
}

func TestRouterRouteStreamingUnframedPayloadFallsBackToBuffered(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newEchoInstance(http.StatusOK)
	}

	streaming := fn("streamfn")
	streaming.Manifest = &manifest.Manifest{URL: manifest.URL{InvokeMode: "RESPONSE_STREAM"}}

	fns := []discovery.Function{streaming}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/streamfn/users?x=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (event.ErrNotFramed falls back to the ordinary buffered path); body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q (identical to today's non-streaming output)", got, want)
	}

	var ev event.RequestV2
	decodeJSON(t, rec, &ev)
	if ev.RawPath != "/users" {
		t.Errorf("rawPath = %q, want %q (route prefix stripped, same as any other v2 route)", ev.RawPath, "/users")
	}
}

func TestRouterRouteBufferedDeclaredFramedPayloadServedVerbatim(t *testing.T) {
	framed := streamFrame(`{"statusCode":202,"headers":{"X-Stream":"yes"}}`, []byte("chunk-1;chunk-2;"))

	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, framed, "application/octet-stream", 0)
	}

	buffered := fn("buffered")
	buffered.Manifest = &manifest.Manifest{URL: manifest.URL{InvokeMode: "BUFFERED"}}

	fns := []discovery.Function{buffered}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/buffered", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// A function that stays BUFFERED never reaches event.ToHTTPStream at
	// all, so a frame it happens to return is handled exactly as it was
	// before invoke mode existed: shapedStatusCode fails to parse the
	// whole payload as JSON (the trailing NULs and body aren't valid
	// JSON), so ToHTTP's writeUnshaped path wins — 200, application/json,
	// the raw frame written verbatim. Proves BUFFERED users see no
	// behaviour change from this feature landing.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (today's unshaped fallback, unchanged); body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := rec.Body.Bytes(), framed; !bytes.Equal(got, want) {
		t.Errorf("body = %q, want the raw frame served verbatim: %q", got, want)
	}
}

func TestRouterRouteStreamingFunctionErrorStillEnvelope(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte(`{"errorType":"ValueError","errorMessage":"boom"}`), "application/json", 0)
	}

	streaming := fn("streamfn")
	streaming.Manifest = &manifest.Manifest{URL: manifest.URL{InvokeMode: "RESPONSE_STREAM"}}

	fns := []discovery.Function{streaming}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/streamfn", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// isFunctionError must run before invoke mode is even consulted: an
	// unhandled exception is an ordinary JSON error envelope, never a
	// frame, so the 502 envelope must keep working exactly as it does for
	// a BUFFERED function.
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

func TestRouterRouteStreamingMalformedFrameIs502(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		// SplitFrame's own gate only checks for a top-level integer
		// statusCode; a non-object "headers" value passes that gate but
		// fails ToHTTPStream's own decode into event.Prelude, taking the
		// same "malformed function response" 502 path a bad shaped
		// event.ToHTTP response does.
		framed := streamFrame(`{"statusCode":200,"headers":[1,2,3]}`, []byte("chunk"))
		return newFakeInstance(http.StatusOK, framed, "application/octet-stream", 0)
	}

	streaming := fn("streamfn")
	streaming.Manifest = &manifest.Manifest{URL: manifest.URL{InvokeMode: "RESPONSE_STREAM"}}

	fns := []discovery.Function{streaming}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodGet, "/streamfn", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
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
	if got := b.startCount("boom"); got != 1 {
		t.Errorf("backend.Start called %d times, want 1 (an ordinary handler error envelope must not evict the instance)", got)
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
	if got := b.startCount("slow"); got != 1 {
		t.Errorf("backend.Start called %d times, want 1 (a slow function is not a dead one; a timeout must not evict)", got)
	}
}

func TestRouterInvokeRuntimeExitErrorEvictsInstance(t *testing.T) {
	envelope := []byte(`{"errorType":"Runtime.ExitError","errorMessage":"RequestId: abc Error: Runtime exited with error: exit status 1"}`)

	b := newFakeBackend()
	starts := 0
	b.newInstance = func(string) *fakeInstance {
		starts++
		if starts == 1 {
			return newFakeInstance(http.StatusOK, envelope, "application/json", 0)
		}
		return newFakeInstance(http.StatusOK, []byte(`{"ok":true}`), "application/json", 0)
	}

	fns := []discovery.Function{fn("crashy")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/crashy/invocations", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Runtime.ExitError is relayed verbatim, not turned into a router error); body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Body.Bytes(), envelope; !bytes.Equal(got, want) {
		t.Errorf("body = %s, want %s (relayed unchanged)", got, want)
	}

	// A subsequent invoke must not reuse the dead instance: Discard should
	// have evicted it, so this Ensure cold-starts a fresh one.
	req2 := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/crashy/invocations", strings.NewReader("{}"))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("second invoke status = %d, want 200; body = %s", rec2.Code, rec2.Body.String())
	}
	if got, want := rec2.Body.String(), `{"ok":true}`; got != want {
		t.Errorf("second invoke body = %q, want %q (fresh instance)", got, want)
	}
	if got := b.startCount("crashy"); got != 2 {
		t.Fatalf("backend.Start called %d times, want 2 (Runtime.ExitError must evict the dead instance)", got)
	}
}

func TestRouterInvokeTransportFailureRetriesTransparently(t *testing.T) {
	b := newFakeBackend()
	starts := 0
	b.newInstance = func(string) *fakeInstance {
		starts++
		if starts == 1 {
			// A closed httptest.Server: nothing is listening at its
			// InvokeURL, so doInvoke's http.Client.Do fails at the
			// transport level, before ever reaching a runtime.
			dead := newFakeInstance(http.StatusOK, []byte("unreachable"), "", 0)
			if err := dead.Stop(context.Background()); err != nil {
				t.Fatalf("closing dead instance's server: %v", err)
			}
			return dead
		}
		return newFakeInstance(http.StatusOK, []byte(`{"ok":true}`), "application/json", 0)
	}

	fns := []discovery.Function{fn("flaky")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/flaky/invocations", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a connection failure against a dead instance retries transparently against a fresh one); body = %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Body.String(), `{"ok":true}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got := b.startCount("flaky"); got != 2 {
		t.Fatalf("backend.Start called %d times, want 2 (initial start + one retry cold start)", got)
	}
}

func TestRouterInvokeTransportFailureBothAttemptsFailIs502(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		dead := newFakeInstance(http.StatusOK, []byte("unreachable"), "", 0)
		if err := dead.Stop(context.Background()); err != nil {
			t.Fatalf("closing dead instance's server: %v", err)
		}
		return dead
	}

	fns := []discovery.Function{fn("dead")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/dead/invocations", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	if got := b.startCount("dead"); got != 2 {
		t.Fatalf("backend.Start called %d times, want 2 (exactly one retry, never a second)", got)
	}
}

// midBodyDisconnectBackend is a minimal backend.Backend test double, local
// to TestRouterInvokeResponseBodyFailureDoesNotRetryOrEvict: it hands back
// a midBodyDisconnectInstance and counts Start calls, so the test can
// assert no retry (and no eviction) happened for a response-body read
// failure — as opposed to fakeBackend/fakeInstance, which have no way to
// make http.Client.Do itself succeed while the subsequent body read fails.
type midBodyDisconnectBackend struct {
	mu     sync.Mutex
	starts int
}

func (b *midBodyDisconnectBackend) Start(context.Context, discovery.Function) (backend.Instance, error) {
	b.mu.Lock()
	b.starts++
	b.mu.Unlock()

	return newMidBodyDisconnectInstance(), nil
}

// startCount reports how many times Start was called.
func (b *midBodyDisconnectBackend) startCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts
}

// midBodyDisconnectInstance is a backend.Instance test double whose server
// accepts the invoke request, hijacks the connection to write response
// headers claiming a body far longer than what it actually sends, then
// closes the connection — simulating a runtime that crashes after it
// already started answering. This makes http.DefaultClient.Do itself
// succeed (headers parse fine) while the subsequent io.ReadAll of the
// response body fails with an unexpected EOF, exercising doInvoke's second
// failure phase (see doInvoke's doc comment) as distinct from a connection
// that never got a response at all.
type midBodyDisconnectInstance struct {
	srv *httptest.Server
}

func newMidBodyDisconnectInstance() *midBodyDisconnectInstance {
	inst := &midBodyDisconnectInstance{}
	inst.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("test server ResponseWriter does not support hijacking")
		}

		conn, _, err := hj.Hijack()
		if err != nil {
			panic(err)
		}
		defer conn.Close()

		// Content-Length promises far more than the body actually
		// delivered before the connection closes.
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: application/json\r\n\r\n{\"partial\":")) //nolint:errcheck // test double, best-effort write.
	}))

	return inst
}

func (i *midBodyDisconnectInstance) InvokeURL() string { return i.srv.URL }

func (i *midBodyDisconnectInstance) Stop(context.Context) error {
	i.srv.Close()
	return nil
}

func (i *midBodyDisconnectInstance) Logs() io.Reader { return strings.NewReader("") }

func TestRouterInvokeResponseBodyFailureDoesNotRetryOrEvict(t *testing.T) {
	b := &midBodyDisconnectBackend{}

	fns := []discovery.Function{fn("flaky-body")}
	h := New(NewManager(b, fns), fns)

	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/flaky-body/invocations", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (a response-body read failure must not retry); body = %s", rec.Code, rec.Body.String())
	}
	if got := b.startCount(); got != 1 {
		t.Fatalf("backend.Start called %d times, want 1 (a response-body read failure must not evict/retry — the runtime may already have executed)", got)
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
