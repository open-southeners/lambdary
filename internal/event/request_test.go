package event

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// withFixedHooks swaps nowFunc/idFunc for fixed values for the duration of
// the calling test, restoring the originals via t.Cleanup — the mechanism
// FromHTTP's golden-event test (and any other test wanting a stable
// requestId/time) relies on.
func withFixedHooks(t *testing.T, now time.Time, id string) {
	t.Helper()

	origNow, origID := nowFunc, idFunc
	nowFunc = func() time.Time { return now }
	idFunc = func() string { return id }

	t.Cleanup(func() {
		nowFunc = origNow
		idFunc = origID
	})
}

func TestFromHTTPGoldenEvent(t *testing.T) {
	fixedTime := time.Date(2020, time.March, 12, 19, 3, 58, 0, time.UTC)
	withFixedHooks(t, fixedTime, "test-request-id")

	req := httptest.NewRequest(http.MethodPost, "/hello/users?x=1&y=2&y=3", strings.NewReader(`{"key":"value"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "test-agent/1.0")
	req.Header.Set("X-Custom", "val1")
	req.Header.Add("X-Multi", "a")
	req.Header.Add("X-Multi", "b")
	req.Header.Set("Cookie", "session=abc123; theme=dark")
	req.RemoteAddr = "203.0.113.5:54321"

	got, err := FromHTTP(req, "/hello")
	if err != nil {
		t.Fatalf("FromHTTP: %v", err)
	}

	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshaling result: %v", err)
	}

	golden := fmt.Sprintf(`{
		"version": "2.0",
		"routeKey": "$default",
		"rawPath": "/users",
		"rawQueryString": "x=1&y=2&y=3",
		"cookies": ["session=abc123", "theme=dark"],
		"headers": {
			"content-type": "application/json",
			"user-agent": "test-agent/1.0",
			"x-custom": "val1",
			"x-multi": "a,b"
		},
		"queryStringParameters": {"x": "1", "y": "2,3"},
		"requestContext": {
			"accountId": "anonymous",
			"apiId": "lambdary",
			"domainName": "localhost",
			"domainPrefix": "localhost",
			"http": {
				"method": "POST",
				"path": "/hello/users",
				"protocol": "HTTP/1.1",
				"sourceIp": "203.0.113.5",
				"userAgent": "test-agent/1.0"
			},
			"requestId": "test-request-id",
			"routeKey": "$default",
			"stage": "$default",
			"time": "12/Mar/2020:19:03:58 +0000",
			"timeEpoch": %d
		},
		"body": "{\"key\":\"value\"}",
		"isBase64Encoded": false
	}`, fixedTime.UnixMilli())

	assertJSONEqual(t, gotJSON, []byte(golden))
}

// assertJSONEqual compares got and want structurally (decoded into
// interface{}) rather than byte-for-byte, so formatting/whitespace and
// encoding/json's default HTML escaping (e.g. "&" -> "&") don't cause
// spurious golden-test failures over semantically identical JSON.
func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()

	var gotAny, wantAny any
	if err := json.Unmarshal(got, &gotAny); err != nil {
		t.Fatalf("unmarshaling got JSON %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantAny); err != nil {
		t.Fatalf("unmarshaling want JSON %s: %v", want, err)
	}

	if !reflect.DeepEqual(gotAny, wantAny) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestStripRoutePrefix(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		path   string
		want   string
	}{
		{"bare route path", "/hello", "/hello", "/"},
		{"nested path", "/hello", "/hello/users", "/users"},
		{"deeper nested path", "/hello", "/hello/users/1", "/users/1"},
		{"trailing slash on bare route", "/hello", "/hello/", "/"},
		{"root route, root request", "/", "/", "/"},
		{"root route, any path", "/", "/anything", "/anything"},
		{"empty prefix behaves like root", "", "/anything", "/anything"},
		{"non-matching similar prefix left alone", "/hello", "/helloworld", "/helloworld"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripRoutePrefix(tt.path, tt.prefix); got != tt.want {
				t.Errorf("stripRoutePrefix(%q, %q) = %q, want %q", tt.path, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestFromHTTPCookies(t *testing.T) {
	withFixedHooks(t, time.Now(), "id")

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	req.Header.Set("Cookie", "session=abc123; theme=dark; empty=")

	got, err := FromHTTP(req, "/hello")
	if err != nil {
		t.Fatalf("FromHTTP: %v", err)
	}

	want := []string{"session=abc123", "theme=dark", "empty="}
	if !reflect.DeepEqual(got.Cookies, want) {
		t.Errorf("Cookies = %v, want %v", got.Cookies, want)
	}

	if _, ok := got.Headers["cookie"]; ok {
		t.Errorf("Headers contains %q; the Cookie header must be excluded (represented via Cookies instead)", "cookie")
	}
}

func TestFromHTTPNoCookies(t *testing.T) {
	withFixedHooks(t, time.Now(), "id")

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)

	got, err := FromHTTP(req, "/hello")
	if err != nil {
		t.Fatalf("FromHTTP: %v", err)
	}

	if got.Cookies != nil {
		t.Errorf("Cookies = %v, want nil (omitted) when the request has no Cookie header", got.Cookies)
	}
}

func TestBuildHeadersMultiValueJoin(t *testing.T) {
	h := http.Header{}
	h.Add("X-Trace", "one")
	h.Add("X-Trace", "two")
	h.Add("X-Trace", "three")
	h.Set("Cookie", "a=b")

	got := buildHeaders(h)

	if want := "one,two,three"; got["x-trace"] != want {
		t.Errorf("headers[x-trace] = %q, want %q", got["x-trace"], want)
	}
	if _, ok := got["cookie"]; ok {
		t.Errorf("buildHeaders must exclude the Cookie header")
	}
}

func TestBuildQueryParamsMultiValueJoin(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/hello?tag=a&tag=b&tag=c&single=1", nil)

	got := buildQueryParams(req.URL)

	if want := "a,b,c"; got["tag"] != want {
		t.Errorf("queryStringParameters[tag] = %q, want %q", got["tag"], want)
	}
	if want := "1"; got["single"] != want {
		t.Errorf("queryStringParameters[single] = %q, want %q", got["single"], want)
	}
}

func TestBuildQueryParamsOmittedWhenEmpty(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)

	if got := buildQueryParams(req.URL); got != nil {
		t.Errorf("buildQueryParams = %v, want nil for a request with no query string", got)
	}
}

func TestIsBase64Body(t *testing.T) {
	invalidUTF8 := []byte{0xff, 0xfe, 0x00}
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

	tests := []struct {
		name        string
		body        []byte
		contentType string
		want        bool
	}{
		{"JSON body, JSON content type", []byte(`{"a":1}`), "application/json", false},
		{"plain text body, text content type", []byte("hello world"), "text/plain", false},
		{"PNG bytes, image content type", png, "image/png", true},
		{"invalid UTF-8, textual content type forces base64 anyway", invalidUTF8, "text/plain; charset=utf-8", true},
		{"invalid UTF-8, no content type", invalidUTF8, "", true},
		{"valid UTF-8, no content type", []byte("hello"), "", false},
		{"empty body, binary content type still false", []byte{}, "image/png", false},
		{"structured +json suffix is textual", []byte(`{"a":1}`), "application/vnd.api+json", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBase64Body(tt.body, tt.contentType); got != tt.want {
				t.Errorf("isBase64Body(%v, %q) = %v, want %v", tt.body, tt.contentType, got, tt.want)
			}
		})
	}
}

func TestFromHTTPBinaryBodyBase64Encoded(t *testing.T) {
	withFixedHooks(t, time.Now(), "id")

	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	req := httptest.NewRequest(http.MethodPost, "/hello", strings.NewReader(string(png)))
	req.Header.Set("Content-Type", "image/png")

	got, err := FromHTTP(req, "/hello")
	if err != nil {
		t.Fatalf("FromHTTP: %v", err)
	}

	if !got.IsBase64Encoded {
		t.Fatal("IsBase64Encoded = false, want true for binary PNG body")
	}
	if got.Body == string(png) {
		t.Error("Body looks like raw bytes, want base64 text")
	}
}

func TestFromHTTPEmptyBody(t *testing.T) {
	withFixedHooks(t, time.Now(), "id")

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)

	got, err := FromHTTP(req, "/hello")
	if err != nil {
		t.Fatalf("FromHTTP: %v", err)
	}

	if got.Body != "" {
		t.Errorf("Body = %q, want empty", got.Body)
	}
	if got.IsBase64Encoded {
		t.Error("IsBase64Encoded = true, want false for an empty body")
	}
}
