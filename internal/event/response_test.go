package event

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestToHTTPShapedFullResponse(t *testing.T) {
	payload := []byte(`{
		"statusCode": 201,
		"headers": {"X-Custom": "yes", "Content-Type": "text/plain"},
		"cookies": ["a=1", "b=2"],
		"body": "created",
		"isBase64Encoded": false
	}`)

	rec := httptest.NewRecorder()
	if err := ToHTTP(rec, payload); err != nil {
		t.Fatalf("ToHTTP: %v", err)
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

func TestToHTTPShapedMinimalDefaults(t *testing.T) {
	payload := []byte(`{"statusCode": 204}`)

	rec := httptest.NewRecorder()
	if err := ToHTTP(rec, payload); err != nil {
		t.Fatalf("ToHTTP: %v", err)
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
	if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("Set-Cookie = %v, want none", got)
	}
}

func TestToHTTPUnshapedObject(t *testing.T) {
	payload := []byte(`{"message": "hi"}`)

	rec := httptest.NewRecorder()
	if err := ToHTTP(rec, payload); err != nil {
		t.Fatalf("ToHTTP: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	if got := rec.Body.String(); got != string(payload) {
		t.Errorf("body = %q, want payload written verbatim %q", got, payload)
	}
}

func TestToHTTPUnshapedObjectNonIntegerStatusCode(t *testing.T) {
	// A "statusCode" field that isn't a literal JSON integer (a string,
	// here) doesn't qualify as a shaped response — it's just an ordinary
	// object return value that happens to have a field with that name.
	payload := []byte(`{"statusCode": "200", "note": "not shaped"}`)

	rec := httptest.NewRecorder()
	if err := ToHTTP(rec, payload); err != nil {
		t.Fatalf("ToHTTP: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (unshaped)", rec.Code)
	}
	if got := rec.Body.String(); got != string(payload) {
		t.Errorf("body = %q, want payload written verbatim %q", got, payload)
	}
}

func TestToHTTPUnshapedArray(t *testing.T) {
	payload := []byte(`[1,2,3]`)

	rec := httptest.NewRecorder()
	if err := ToHTTP(rec, payload); err != nil {
		t.Fatalf("ToHTTP: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	if got := rec.Body.String(); got != string(payload) {
		t.Errorf("body = %q, want payload written verbatim %q", got, payload)
	}
}

func TestToHTTPUnshapedBareString(t *testing.T) {
	payload := []byte(`"hello"`)

	rec := httptest.NewRecorder()
	if err := ToHTTP(rec, payload); err != nil {
		t.Fatalf("ToHTTP: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `"hello"` {
		t.Errorf("body = %q, want %q", got, `"hello"`)
	}
}

func TestToHTTPUnshapedInvalidJSON(t *testing.T) {
	payload := []byte(`not json at all`)

	rec := httptest.NewRecorder()
	if err := ToHTTP(rec, payload); err != nil {
		t.Fatalf("ToHTTP: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	if got := rec.Body.String(); got != string(payload) {
		t.Errorf("body = %q, want payload written verbatim %q", got, payload)
	}
}

func TestToHTTPShapedBase64Body(t *testing.T) {
	raw := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	encoded := base64.StdEncoding.EncodeToString(raw)

	payload := []byte(`{"statusCode": 200, "body": "` + encoded + `", "isBase64Encoded": true}`)

	rec := httptest.NewRecorder()
	if err := ToHTTP(rec, payload); err != nil {
		t.Fatalf("ToHTTP: %v", err)
	}

	if got := rec.Body.Bytes(); string(got) != string(raw) {
		t.Errorf("body = %v, want decoded bytes %v", got, raw)
	}
}

func TestToHTTPShapedBadBase64Errors(t *testing.T) {
	payload := []byte(`{"statusCode": 200, "body": "not-valid-base64!!!", "isBase64Encoded": true}`)

	rec := httptest.NewRecorder()
	err := ToHTTP(rec, payload)
	if err == nil {
		t.Fatal("ToHTTP returned nil error for a malformed base64 body, want an error the caller can turn into a 502")
	}
}

func TestToHTTPShapedMultipleSetCookies(t *testing.T) {
	payload := []byte(`{"statusCode": 200, "cookies": ["a=1", "b=2", "c=3"]}`)

	rec := httptest.NewRecorder()
	if err := ToHTTP(rec, payload); err != nil {
		t.Fatalf("ToHTTP: %v", err)
	}

	got := rec.Header().Values("Set-Cookie")
	want := []string{"a=1", "b=2", "c=3"}
	if len(got) != len(want) {
		t.Fatalf("Set-Cookie count = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("Set-Cookie[%d] = %q, want %q", i, got[i], v)
		}
	}
}
