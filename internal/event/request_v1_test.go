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

func TestFromHTTPV1GoldenEvent(t *testing.T) {
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

	got, err := FromHTTPV1(req, "/hello")
	if err != nil {
		t.Fatalf("FromHTTPV1: %v", err)
	}

	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshaling result: %v", err)
	}

	golden := fmt.Sprintf(`{
		"resource": "/{proxy+}",
		"path": "/users",
		"httpMethod": "POST",
		"headers": {
			"Content-Type": "application/json",
			"User-Agent": "test-agent/1.0",
			"X-Custom": "val1",
			"X-Multi": "b",
			"Cookie": "session=abc123; theme=dark"
		},
		"multiValueHeaders": {
			"Content-Type": ["application/json"],
			"User-Agent": ["test-agent/1.0"],
			"X-Custom": ["val1"],
			"X-Multi": ["a", "b"],
			"Cookie": ["session=abc123; theme=dark"]
		},
		"queryStringParameters": {"x": "1", "y": "3"},
		"multiValueQueryStringParameters": {"x": ["1"], "y": ["2", "3"]},
		"pathParameters": {"proxy": "users"},
		"stageVariables": null,
		"requestContext": {
			"accountId": "anonymous",
			"apiId": "lambdary",
			"stage": "$default",
			"requestId": "test-request-id",
			"identity": {
				"sourceIp": "203.0.113.5",
				"userAgent": "test-agent/1.0"
			},
			"httpMethod": "POST",
			"path": "/hello/users",
			"protocol": "HTTP/1.1",
			"requestTime": "12/Mar/2020:19:03:58 +0000",
			"requestTimeEpoch": %d
		},
		"body": "{\"key\":\"value\"}",
		"isBase64Encoded": false
	}`, fixedTime.UnixMilli())

	assertJSONEqual(t, gotJSON, []byte(golden))
}

func TestBuildProxyPathParameters(t *testing.T) {
	tests := []struct {
		name string
		path string
		want map[string]string
	}{
		{"root of route", "/", nil},
		{"single segment", "/users", map[string]string{"proxy": "users"}},
		{"nested segments", "/users/1", map[string]string{"proxy": "users/1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildProxyPathParameters(tt.path)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildProxyPathParameters(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestFromHTTPV1PathParametersNilAtRouteRoot(t *testing.T) {
	withFixedHooks(t, time.Now(), "id")

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)

	got, err := FromHTTPV1(req, "/hello")
	if err != nil {
		t.Fatalf("FromHTTPV1: %v", err)
	}

	if got.Path != "/" {
		t.Fatalf("Path = %q, want %q", got.Path, "/")
	}
	if got.PathParameters != nil {
		t.Errorf("PathParameters = %v, want nil for a bare hit on the route root", got.PathParameters)
	}
}

// TestBuildHeadersV1LastValueWins documents and verifies the single-value
// headers map's behavior against AWS's own documented example: a header
// sent multiple times is represented in the single-value map by its LAST
// occurrence — not comma-joined like buildHeaders' v2 convention — while
// multiValueHeaders keeps every value. See
// https://docs.aws.amazon.com/apigateway/latest/developerguide/set-up-lambda-proxy-integrations.html#api-gateway-simple-proxy-for-lambda-input-format
// ("header2": "value1","header2": "value2" -> headers.header2 == "value2",
// multiValueHeaders.header2 == ["value1", "value2"]).
func TestBuildHeadersV1LastValueWins(t *testing.T) {
	h := http.Header{}
	h.Add("X-Trace", "one")
	h.Add("X-Trace", "two")
	h.Add("X-Trace", "three")
	h.Set("Cookie", "a=b")

	single := buildHeadersV1(h)
	if want := "three"; single["X-Trace"] != want {
		t.Errorf("headers[X-Trace] = %q, want %q (last value)", single["X-Trace"], want)
	}
	if want := "a=b"; single["Cookie"] != want {
		t.Errorf("headers[Cookie] = %q, want %q (v1 keeps Cookie in headers)", single["Cookie"], want)
	}

	multi := buildMultiValueHeadersV1(h)
	want := []string{"one", "two", "three"}
	if !reflect.DeepEqual(multi["X-Trace"], want) {
		t.Errorf("multiValueHeaders[X-Trace] = %v, want %v", multi["X-Trace"], want)
	}
}

// TestBuildQueryParamsV1LastValueWins is buildHeadersV1's query-string
// equivalent: same AWS-documented last-value rule for the single-value map,
// full array in the multi-value map.
func TestBuildQueryParamsV1LastValueWins(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/hello?tag=a&tag=b&tag=c&single=1", nil)
	query := req.URL.Query()

	single := buildQueryParamsV1(query)
	if want := "c"; single["tag"] != want {
		t.Errorf("queryStringParameters[tag] = %q, want %q (last value)", single["tag"], want)
	}
	if want := "1"; single["single"] != want {
		t.Errorf("queryStringParameters[single] = %q, want %q", single["single"], want)
	}

	multi := buildMultiValueQueryParamsV1(query)
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(multi["tag"], want) {
		t.Errorf("multiValueQueryStringParameters[tag] = %v, want %v", multi["tag"], want)
	}
}

func TestBuildQueryParamsV1OmittedWhenEmpty(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	query := req.URL.Query()

	if got := buildQueryParamsV1(query); got != nil {
		t.Errorf("buildQueryParamsV1 = %v, want nil for a request with no query string", got)
	}
	if got := buildMultiValueQueryParamsV1(query); got != nil {
		t.Errorf("buildMultiValueQueryParamsV1 = %v, want nil for a request with no query string", got)
	}
}

func TestFromHTTPV1PrefixStripping(t *testing.T) {
	withFixedHooks(t, time.Now(), "id")

	tests := []struct {
		name       string
		prefix     string
		reqPath    string
		wantPath   string
		wantParams map[string]string
	}{
		{"bare route path", "/hello", "/hello", "/", nil},
		{"nested path", "/hello", "/hello/users/1", "/users/1", map[string]string{"proxy": "users/1"}},
		{"root route", "/", "/anything", "/anything", map[string]string{"proxy": "anything"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.reqPath, nil)

			got, err := FromHTTPV1(req, tt.prefix)
			if err != nil {
				t.Fatalf("FromHTTPV1: %v", err)
			}

			if got.Path != tt.wantPath {
				t.Errorf("Path = %q, want %q", got.Path, tt.wantPath)
			}
			if !reflect.DeepEqual(got.PathParameters, tt.wantParams) {
				t.Errorf("PathParameters = %v, want %v", got.PathParameters, tt.wantParams)
			}
		})
	}
}

func TestFromHTTPV1EmptyBodyIsNull(t *testing.T) {
	withFixedHooks(t, time.Now(), "id")

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)

	got, err := FromHTTPV1(req, "/hello")
	if err != nil {
		t.Fatalf("FromHTTPV1: %v", err)
	}

	if got.Body != nil {
		t.Errorf("Body = %v, want nil (marshals to JSON null) for an empty request body", got.Body)
	}
	if got.IsBase64Encoded {
		t.Error("IsBase64Encoded = true, want false for an empty body")
	}

	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshaling result: %v", err)
	}
	if !strings.Contains(string(gotJSON), `"body":null`) {
		t.Errorf("marshaled JSON = %s, want it to contain \"body\":null", gotJSON)
	}
}

func TestFromHTTPV1BinaryBodyBase64Encoded(t *testing.T) {
	withFixedHooks(t, time.Now(), "id")

	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	req := httptest.NewRequest(http.MethodPost, "/hello", strings.NewReader(string(png)))
	req.Header.Set("Content-Type", "image/png")

	got, err := FromHTTPV1(req, "/hello")
	if err != nil {
		t.Fatalf("FromHTTPV1: %v", err)
	}

	if !got.IsBase64Encoded {
		t.Fatal("IsBase64Encoded = false, want true for binary PNG body")
	}
	if got.Body == nil {
		t.Fatal("Body = nil, want the base64-encoded PNG bytes")
	}
	if *got.Body == string(png) {
		t.Error("Body looks like raw bytes, want base64 text")
	}
}
