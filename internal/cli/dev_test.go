package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNewDevCmd verifies the dev command is properly configured.
func TestNewDevCmd(t *testing.T) {
	cmd := newDevCmd()
	if cmd.Use != "dev" {
		t.Errorf("expected Use 'dev', got %q", cmd.Use)
	}
	if cmd.RunE == nil {
		t.Errorf("expected RunE to be set")
	}
	if got := cmd.Flags().Lookup("no-reload").DefValue; got != "false" {
		t.Errorf("--no-reload default = %q, want %q (reload on by default)", got, "false")
	}
}

// TestAtomicHandlerServesCurrentlyStoredHandler covers the handler-swap
// indirection dev's structural reload relies on: requests hit whichever
// handler Store last set, and — critically — a swap mid-run redirects the
// *next* request without needing to restart the listener.
func TestAtomicHandlerServesCurrentlyStoredHandler(t *testing.T) {
	var h atomicHandler

	first := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first")) //nolint:errcheck // test helper.
	})
	second := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("second")) //nolint:errcheck // test helper.
	})

	h.Store(first)

	srv := httptest.NewServer(&h)
	defer srv.Close()

	if body := getBody(t, srv.URL); body != "first" {
		t.Fatalf("response before swap = %q, want %q", body, "first")
	}

	h.Store(second)

	if body := getBody(t, srv.URL); body != "second" {
		t.Fatalf("response after swap = %q, want %q", body, "second")
	}
}

// TestAtomicHandlerUnreadyReturns503 covers the (in dev's own use,
// unreachable — Store always runs before Serve starts accepting) fallback:
// a request before Store has ever been called gets a 503, not a panic.
func TestAtomicHandlerUnreadyReturns503(t *testing.T) {
	var h atomicHandler

	srv := httptest.NewServer(&h)
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:noctx // test helper.
	if err != nil {
		t.Fatalf("GET %s: %v", srv.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

// getBody GETs url and returns its response body as a string, failing the
// test on any error.
func getBody(t *testing.T, url string) string {
	t.Helper()

	resp, err := http.Get(url) //nolint:noctx // test helper.
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)

	return string(buf[:n])
}

// TestValidateBackend checks --backend flag validation.
func TestValidateBackend(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		wantErr bool
	}{
		{"auto", "auto", false},
		{"container", "container", false},
		{"empty", "", false},
		{"process", "process", false},
		{"unknown", "unknown", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBackend(tt.backend)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateBackend(%q) error = %v, wantErr %v", tt.backend, err, tt.wantErr)
			}
		})
	}
}
