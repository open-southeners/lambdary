package event

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// frame builds a raw http-integration-response payload — prelude JSON,
// frameDelimiter, then body — matching the shape awslambda.streamifyResponse
// produces. It always allocates a fresh slice, so tests can't accidentally
// alias (and corrupt) the package-level frameDelimiter through append.
func frame(prelude string, body []byte) []byte {
	payload := make([]byte, 0, len(prelude)+len(frameDelimiter)+len(body))
	payload = append(payload, prelude...)
	payload = append(payload, frameDelimiter...)
	payload = append(payload, body...)

	return payload
}

func TestSplitFrame(t *testing.T) {
	tests := []struct {
		name       string
		payload    []byte
		wantOK     bool
		wantPrefix []byte // prelude, when wantOK
		wantBody   []byte // body, when wantOK
	}{
		{
			name:       "prelude and body",
			payload:    frame(`{"statusCode":200}`, []byte("chunk-1;chunk-2;")),
			wantOK:     true,
			wantPrefix: []byte(`{"statusCode":200}`),
			wantBody:   []byte("chunk-1;chunk-2;"),
		},
		{
			name:       "prelude and empty body",
			payload:    frame(`{"statusCode":204}`, nil),
			wantOK:     true,
			wantPrefix: []byte(`{"statusCode":204}`),
			wantBody:   []byte{},
		},
		{
			name:       "body containing NUL bytes",
			payload:    frame(`{"statusCode":200}`, []byte{'a', 0x00, 'b', 0x00, 'c'}),
			wantOK:     true,
			wantPrefix: []byte(`{"statusCode":200}`),
			wantBody:   []byte{'a', 0x00, 'b', 0x00, 'c'},
		},
		{
			name:    "no delimiter",
			payload: []byte(`{"statusCode":200}chunk-1;chunk-2;`),
			wantOK:  false,
		},
		{
			name:    "prelude is not JSON",
			payload: frame("not json at all", []byte("chunk")),
			wantOK:  false,
		},
		{
			name:    "prelude without statusCode",
			payload: frame(`{"headers":{"X-Probe":"1"}}`, []byte("chunk")),
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prelude, body, ok := SplitFrame(tt.payload)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				if prelude != nil || body != nil {
					t.Errorf("prelude/body = %q/%q, want nil/nil when ok=false", prelude, body)
				}
				return
			}

			if !bytes.Equal(prelude, tt.wantPrefix) {
				t.Errorf("prelude = %q, want %q", prelude, tt.wantPrefix)
			}
			if !bytes.Equal(body, tt.wantBody) {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestToHTTPStreamNotFramed(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{"no delimiter", []byte(`{"statusCode":200}chunk`)},
		{"prelude is not JSON", frame("not json", []byte("chunk"))},
		{"prelude without statusCode", frame(`{"headers":{}}`, nil)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := ToHTTPStream(rec, tt.payload)
			if !errors.Is(err, ErrNotFramed) {
				t.Fatalf("ToHTTPStream error = %v, want %v", err, ErrNotFramed)
			}

			// ErrNotFramed's contract is that nothing was written to w —
			// the caller falls back to event.ToHTTP on the same
			// ResponseWriter, which would be unsafe if a status or body
			// had already gone out.
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d (default, meaning WriteHeader was never called)", rec.Code, http.StatusOK)
			}
			if len(rec.Header()) != 0 {
				t.Errorf("headers = %v, want none set", rec.Header())
			}
			if rec.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", rec.Body.String())
			}
		})
	}
}

func TestToHTTPStreamFramedPayload(t *testing.T) {
	payload := frame(
		`{"statusCode":201,"headers":{"X-Probe":"yes","Content-Type":"text/plain"},"cookies":["a=1","b=2"]}`,
		[]byte("chunk-1;chunk-2;"),
	)

	rec := httptest.NewRecorder()
	if err := ToHTTPStream(rec, payload); err != nil {
		t.Fatalf("ToHTTPStream: %v", err)
	}

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if got := rec.Header().Get("X-Probe"); got != "yes" {
		t.Errorf("X-Probe header = %q, want %q", got, "yes")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want %q (handler-supplied, not defaulted)", got, "text/plain")
	}
	if got := rec.Body.String(); got != "chunk-1;chunk-2;" {
		t.Errorf("body = %q, want %q", got, "chunk-1;chunk-2;")
	}

	gotCookies := rec.Header().Values("Set-Cookie")
	wantCookies := []string{"a=1", "b=2"}
	if len(gotCookies) != len(wantCookies) {
		t.Fatalf("Set-Cookie count = %d, want %d (got %v)", len(gotCookies), len(wantCookies), gotCookies)
	}
	for i, v := range wantCookies {
		if gotCookies[i] != v {
			t.Errorf("Set-Cookie[%d] = %q, want %q", i, gotCookies[i], v)
		}
	}
}

func TestToHTTPStreamEmptyBody(t *testing.T) {
	payload := frame(`{"statusCode":204}`, nil)

	rec := httptest.NewRecorder()
	if err := ToHTTPStream(rec, payload); err != nil {
		t.Fatalf("ToHTTPStream: %v", err)
	}

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Body.String(); got != "" {
		t.Errorf("body = %q, want empty", got)
	}
}

func TestToHTTPStreamMissingContentTypeDefaultsToOctetStream(t *testing.T) {
	payload := frame(`{"statusCode":200}`, []byte("raw bytes"))

	rec := httptest.NewRecorder()
	if err := ToHTTPStream(rec, payload); err != nil {
		t.Fatalf("ToHTTPStream: %v", err)
	}

	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want default %q (not application/json — a streamed body is arbitrary bytes)", got, "application/octet-stream")
	}
}

func TestToHTTPStreamHeaderValueContainsDelimiterAsText(t *testing.T) {
	// The delimiter is only meaningful as raw bytes splitting prelude from
	// body; a header *value* that happens to contain the same eight
	// characters as literal text (as opposed to actual NUL bytes) is just
	// an ordinary string and must round-trip untouched.
	delimiterAsText := "00000000"
	payload := frame(`{"statusCode":200,"headers":{"X-Marker":"`+delimiterAsText+`"}}`, []byte("body"))

	rec := httptest.NewRecorder()
	if err := ToHTTPStream(rec, payload); err != nil {
		t.Fatalf("ToHTTPStream: %v", err)
	}

	if got := rec.Header().Get("X-Marker"); got != delimiterAsText {
		t.Errorf("X-Marker header = %q, want %q", got, delimiterAsText)
	}
	if got := rec.Body.String(); got != "body" {
		t.Errorf("body = %q, want %q", got, "body")
	}
}

func TestToHTTPUnchangedByRefactor(t *testing.T) {
	// writeHeadersStatusBody replaced writeShaped's inline header/cookie/
	// body logic; this proves ToHTTP (which response_test.go already
	// covers in depth) still defaults Content-Type to "application/json"
	// and still writes cookies and bodies the same way after that
	// refactor, not just that stream.go's own default works.
	payload := []byte(`{"statusCode":200,"cookies":["a=1"],"body":"hi"}`)

	rec := httptest.NewRecorder()
	if err := ToHTTP(rec, payload); err != nil {
		t.Fatalf("ToHTTP: %v", err)
	}

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want default %q", got, "application/json")
	}
	if got := rec.Header().Get("Set-Cookie"); got != "a=1" {
		t.Errorf("Set-Cookie = %q, want %q", got, "a=1")
	}
	if got := rec.Body.String(); got != "hi" {
		t.Errorf("body = %q, want %q", got, "hi")
	}
}
