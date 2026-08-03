package event

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestToHTTPV1ShapedFullResponse(t *testing.T) {
	payload := []byte(`{
		"statusCode": 201,
		"headers": {"X-Custom": "yes", "Content-Type": "text/plain"},
		"body": "created",
		"isBase64Encoded": false
	}`)

	rec := httptest.NewRecorder()
	if err := ToHTTPV1(rec, payload); err != nil {
		t.Fatalf("ToHTTPV1: %v", err)
	}

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if got := rec.Header().Get("X-Custom"); got != "yes" {
		t.Errorf("X-Custom header = %q, want %q", got, "yes")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want %q (handler-supplied, not defaulted)", got, "text/plain")
	}
	if got := rec.Body.String(); got != "created" {
		t.Errorf("body = %q, want %q", got, "created")
	}
}

func TestToHTTPV1ShapedMinimalDefaults(t *testing.T) {
	payload := []byte(`{"statusCode": 204}`)

	rec := httptest.NewRecorder()
	if err := ToHTTPV1(rec, payload); err != nil {
		t.Fatalf("ToHTTPV1: %v", err)
	}

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want default %q", got, "application/json")
	}
	if got := rec.Body.String(); got != "" {
		t.Errorf("body = %q, want empty", got)
	}
}

// TestToHTTPV1MultiValueHeadersMergeOrder verifies the plan's merge rule:
// headers[k] is applied first, then every multiValueHeaders[k] value is
// appended after it — additive, no dedup of an exact key/value pair
// appearing in both (see ToHTTPV1's doc comment for the AWS-docs nuance
// this deliberately simplifies away).
func TestToHTTPV1MultiValueHeadersMergeOrder(t *testing.T) {
	payload := []byte(`{
		"statusCode": 200,
		"headers": {"X-Trace": "first"},
		"multiValueHeaders": {"X-Trace": ["second", "third"]}
	}`)

	rec := httptest.NewRecorder()
	if err := ToHTTPV1(rec, payload); err != nil {
		t.Fatalf("ToHTTPV1: %v", err)
	}

	got := rec.Header().Values("X-Trace")
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("X-Trace values = %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("X-Trace[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestToHTTPV1MultiValueHeadersOnly(t *testing.T) {
	payload := []byte(`{
		"statusCode": 200,
		"multiValueHeaders": {"Set-Cookie": ["a=1", "b=2"]}
	}`)

	rec := httptest.NewRecorder()
	if err := ToHTTPV1(rec, payload); err != nil {
		t.Fatalf("ToHTTPV1: %v", err)
	}

	got := rec.Header().Values("Set-Cookie")
	want := []string{"a=1", "b=2"}
	if len(got) != len(want) {
		t.Fatalf("Set-Cookie values = %v, want %v (cookies are plain headers in v1, no special-casing)", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("Set-Cookie[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestToHTTPV1ShapedBase64Body(t *testing.T) {
	raw := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	encoded := base64.StdEncoding.EncodeToString(raw)

	payload := []byte(`{"statusCode": 200, "body": "` + encoded + `", "isBase64Encoded": true}`)

	rec := httptest.NewRecorder()
	if err := ToHTTPV1(rec, payload); err != nil {
		t.Fatalf("ToHTTPV1: %v", err)
	}

	if got := rec.Body.Bytes(); string(got) != string(raw) {
		t.Errorf("body = %v, want decoded bytes %v", got, raw)
	}
}

// TestToHTTPV1Malformed is the response-side table for v1's stricter
// contract: no bare-JSON-to-200 fallback the way ToHTTP's v2 has — every
// case here must error so the router can turn it into a 502, mirroring
// real API Gateway's own "malformed Lambda proxy response" behavior.
func TestToHTTPV1Malformed(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{"missing statusCode", `{"body": "hi"}`},
		{"non-integer statusCode", `{"statusCode": "200"}`},
		{"non-object top level (array)", `[1,2,3]`},
		{"non-object top level (string)", `"hello"`},
		{"invalid JSON", `not json at all`},
		{"bad base64 body", `{"statusCode": 200, "body": "not-valid-base64!!!", "isBase64Encoded": true}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := ToHTTPV1(rec, []byte(tt.payload))
			if err == nil {
				t.Fatalf("ToHTTPV1(%s) returned nil error, want an error the caller can turn into a 502", tt.payload)
			}
		})
	}
}

func TestToHTTPV1DefaultContentType(t *testing.T) {
	payload := []byte(`{"statusCode": 200, "body": "{}"}`)

	rec := httptest.NewRecorder()
	if err := ToHTTPV1(rec, payload); err != nil {
		t.Fatalf("ToHTTPV1: %v", err)
	}

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want default %q", got, "application/json")
	}
}
