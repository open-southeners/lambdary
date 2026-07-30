package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/container"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/router"
)

// TestE2E exercises the full local stack against real Docker: resolve+
// discover the testdata/demo fixture root, wire up the container backend,
// router.Manager, and router.New exactly like `lambdary dev` does (see
// runDev in dev.go), and serve it on an httptest server. It's the
// end-to-end companion to internal/event and internal/router's unit tests —
// plans/m2-http-events.md's Unit C — covering the HTTP↔event mapping this
// milestone adds on top of M1's invoke passthrough.
//
// It only runs when a real docker daemon answers `docker info` AND -short
// is absent, same guard as
// internal/backend/container/integration_test.go: the point is to skip on
// hosts/CI without Docker, not to require it.
func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test skipped: -short")
	}

	var runner backend.ExecRunner

	if _, err := runner.Run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("e2e test skipped: docker daemon unreachable: %v", err)
	}

	root, err := filepath.Abs("testdata/demo")
	if err != nil {
		t.Fatalf("filepath.Abs() unexpected error: %v", err)
	}

	scanRoot, cfg, err := resolveRoot(root)
	if err != nil {
		t.Fatalf("resolveRoot() unexpected error: %v", err)
	}

	fns, err := discovery.Discover(scanRoot, cfg)
	if err != nil {
		t.Fatalf("discovery.Discover() unexpected error: %v", err)
	}
	if len(fns) != 2 {
		t.Fatalf("discovered %d functions, want 2 (hello, shaped): %+v", len(fns), fns)
	}

	cli, err := backend.DetectContainerCLI(context.Background(), runner)
	if err != nil {
		t.Fatalf("DetectContainerCLI() unexpected error: %v", err)
	}

	b := container.New(cli, runner)
	mgr := router.NewManager(b, fns)
	handler := router.New(mgr, fns)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := mgr.StopAll(stopCtx); err != nil {
			t.Errorf("cleanup StopAll() unexpected error: %v", err)
		}

		waitForNoLabeledContainers(t, runner, "docker ps after StopAll()")
	})

	// The first invocation of a fixture starts its container: the image
	// (public.ecr.aws/lambda/python:3.13) is already cached on this host so
	// starts are fast, but give the HTTP client generous headroom for a
	// cold pull elsewhere — the manifests' own timeout: 120 bounds the
	// router's server-side invoke deadline the same way.
	client := &http.Client{Timeout: 120 * time.Second}

	t.Run("browser-style GET arrives as a v2 event", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/hello/users?x=1", nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("Cookie", "session=abc123")
		req.Header.Set("X-Demo-Header", "hi")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", req.URL, err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading response body: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want %q", ct, "application/json")
		}

		var result struct {
			Echo struct {
				Version        string            `json:"version"`
				RawPath        string            `json:"rawPath"`
				RawQueryString string            `json:"rawQueryString"`
				Cookies        []string          `json:"cookies"`
				Headers        map[string]string `json:"headers"`
				RequestContext struct {
					HTTP struct {
						Method string `json:"method"`
						Path   string `json:"path"`
					} `json:"http"`
				} `json:"requestContext"`
			} `json:"echo"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("unmarshalling response body %q: %v", body, err)
		}

		ev := result.Echo
		if ev.Version != "2.0" {
			t.Errorf("event version = %q, want %q", ev.Version, "2.0")
		}
		if ev.RawPath != "/users" {
			t.Errorf("event rawPath = %q, want %q", ev.RawPath, "/users")
		}
		if ev.RawQueryString != "x=1" {
			t.Errorf("event rawQueryString = %q, want %q", ev.RawQueryString, "x=1")
		}
		if !containsString(ev.Cookies, "session=abc123") {
			t.Errorf("event cookies = %v, want to contain %q", ev.Cookies, "session=abc123")
		}
		if got := ev.Headers["x-demo-header"]; got != "hi" {
			t.Errorf("event headers[x-demo-header] = %q, want %q", got, "hi")
		}
		if ev.RequestContext.HTTP.Path != "/hello/users" {
			t.Errorf("event requestContext.http.path = %q, want %q", ev.RequestContext.HTTP.Path, "/hello/users")
		}
		if ev.RequestContext.HTTP.Method != http.MethodGet {
			t.Errorf("event requestContext.http.method = %q, want %q", ev.RequestContext.HTTP.Method, http.MethodGet)
		}
	})

	t.Run("shaped response honored", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/shaped")
		if err != nil {
			t.Fatalf("GET %s/shaped: %v", srv.URL, err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading response body: %v", err)
		}

		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", resp.StatusCode, body)
		}
		if got := resp.Header.Get("X-Demo"); got != "yes" {
			t.Errorf("X-Demo header = %q, want %q", got, "yes")
		}
		if got := resp.Header.Get("Content-Type"); got != "text/plain" {
			t.Errorf("Content-Type = %q, want %q", got, "text/plain")
		}

		var sawCookie bool
		for _, c := range resp.Cookies() {
			if c.Name == "session" && c.Value == "abc123" {
				sawCookie = true
			}
		}
		if !sawCookie {
			t.Errorf("Set-Cookie session=abc123 not found in %v", resp.Cookies())
		}
		if string(body) != "created" {
			t.Errorf("body = %q, want %q", body, "created")
		}
	})

	t.Run("invoke passthrough still works", func(t *testing.T) {
		resp, err := client.Post(srv.URL+"/2015-03-31/functions/hello/invocations", "application/json", strings.NewReader(`{"a":1}`))
		if err != nil {
			t.Fatalf("POST invocations: %v", err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading response body: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
		}

		var result struct {
			Echo map[string]float64 `json:"echo"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("unmarshalling response body %q: %v", body, err)
		}
		if result.Echo["a"] != 1 {
			t.Errorf("echo = %v, want {\"a\": 1}", result.Echo)
		}
	})

	t.Run("index lists both functions", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading response body: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
		}

		var result struct {
			Functions []struct {
				Name string `json:"name"`
			} `json:"functions"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("unmarshalling response body %q: %v", body, err)
		}

		names := make(map[string]bool, len(result.Functions))
		for _, fn := range result.Functions {
			names[fn.Name] = true
		}
		if !names["hello"] || !names["shaped"] {
			t.Errorf("index functions = %v, want to contain hello and shaped", result.Functions)
		}
	})
}

// containsString reports whether s appears in ss.
func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}

	return false
}

// waitForNoLabeledContainers polls `docker ps -a --filter label=lambdary=1`
// every 250ms until it reports no containers or a 10s deadline expires,
// failing the test with the last output only in the latter case. A
// container run with --rm (as ours are) is removed asynchronously after
// `docker stop` returns, so an instant check right after StopAll can still
// briefly see it — polling avoids that flake instead of asserting on a
// single point-in-time snapshot.
func waitForNoLabeledContainers(t *testing.T, runner backend.ExecRunner, msgPrefix string) {
	t.Helper()

	const (
		pollInterval = 250 * time.Millisecond
		pollTimeout  = 10 * time.Second
	)

	deadline := time.Now().Add(pollTimeout)

	var last string
	for {
		out, err := runner.Run(context.Background(), "docker", "ps", "-a",
			"--filter", "label=lambdary=1", "--format", "{{.ID}}")
		if err != nil {
			t.Fatalf("docker ps unexpected error: %v", err)
		}

		last = strings.TrimSpace(string(out))
		if last == "" {
			return
		}

		if time.Now().After(deadline) {
			break
		}

		time.Sleep(pollInterval)
	}

	t.Errorf("%s = %q, want no lambdary containers left", msgPrefix, last)
}
