package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/container"
	"github.com/open-southeners/lambdary/internal/backend/process"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/rie"
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
	// testdata/demo also holds node-hello (added for TestE2EProcessBackend,
	// below); it's discovered here too since both tests share the fixture
	// root, but this test never starts or asserts on it — only hello and
	// shaped, same as before.
	if len(fns) != 3 {
		t.Fatalf("discovered %d functions, want 3 (hello, shaped, node-hello): %+v", len(fns), fns)
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

// TestE2EProcessBackend is TestE2E's Docker-free companion: the same
// testdata/demo fixture root (hello, shaped — both python) plus node-hello
// (the process backend's Node shim's own fixture, added for this test), all
// driven through internal/backend/process against host-installed node and
// python3 instead of a container runtime, per plans/m3-process-path.md's
// Unit C. It proves the process backend end to end for both languages it
// supports (see internal/backend/process's package doc) — the Node shim in
// particular has no other end-to-end coverage, since internal/backend/
// process's own tests only exercise it against a fake RIE.
//
// process backend functions run on whatever runtime versions are installed
// on this host (this repo's dev host: node v24, python 3.14 — see
// plans/m3-process-path.md's "Environment facts"), not the pinned Lambda
// runtime versions the container backend's AWS base images provide. That's
// the process backend's documented fidelity trade-off (DESIGN.md), not a
// bug in this test.
//
// Skipped on -short, or when node or python3 aren't on PATH. The RIE binary
// itself is resolved via the real rie.Resolve/ExecRunner: a warm
// ~/.lambdary/bin cache (the common case once any lambdary command has run
// once on a host) makes this instant, but the deadline below is generous
// enough to cover a first-time source build too.
func TestE2EProcessBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test skipped: -short")
	}

	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("e2e test skipped: node not on PATH: %v", err)
	}

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("e2e test skipped: python3 not on PATH: %v", err)
	}

	var runner backend.ExecRunner

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
	if len(fns) != 3 {
		t.Fatalf("discovered %d functions, want 3 (hello, shaped, node-hello): %+v", len(fns), fns)
	}

	baseline := countRIEProcesses(t)

	// Generous: covers a cold ~/.lambdary/bin cache (a source build takes
	// ~30s on this host, per plans/m3-process-path.md) as well as the
	// common warm-cache case.
	resolveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	riePath, err := rie.Resolve(resolveCtx, runner)
	if err != nil {
		t.Fatalf("rie.Resolve() unexpected error: %v", err)
	}

	b := process.New(riePath, runner)
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

		waitForRIEProcessBaseline(t, baseline)
	})

	// No image pull here (see internal/backend/process's own readyTimeout,
	// ~15s), but the first invocation of a function still spawns and
	// readiness-checks a fresh RIE + runtime process, so this stays well
	// above that.
	client := &http.Client{Timeout: 60 * time.Second}

	t.Run("browser-style GET arrives as a v2 event (python via process backend)", func(t *testing.T) {
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

		var result struct {
			Echo struct {
				Version        string            `json:"version"`
				RawPath        string            `json:"rawPath"`
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
		if !containsString(ev.Cookies, "session=abc123") {
			t.Errorf("event cookies = %v, want to contain %q", ev.Cookies, "session=abc123")
		}
		if got := ev.Headers["x-demo-header"]; got != "hi" {
			t.Errorf("event headers[x-demo-header] = %q, want %q", got, "hi")
		}
		if ev.RequestContext.HTTP.Path != "/hello/users" {
			t.Errorf("event requestContext.http.path = %q, want %q", ev.RequestContext.HTTP.Path, "/hello/users")
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

	t.Run("node-hello echoes the event via the Node shim", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/node-hello?y=2")
		if err != nil {
			t.Fatalf("GET %s/node-hello: %v", srv.URL, err)
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
			Echo struct {
				QueryStringParameters map[string]string `json:"queryStringParameters"`
			} `json:"echo"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("unmarshalling response body %q: %v", body, err)
		}

		if got := result.Echo.QueryStringParameters["y"]; got != "2" {
			t.Errorf("echo.queryStringParameters[y] = %q, want %q (full body: %s)", got, "2", body)
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
}

// countRIEProcesses returns how many processes on the host currently match
// `pgrep -f aws-lambda-rie` — the process backend's RIE binary name — via a
// combined-output run so a "no processes found" (pgrep's normal exit 1) is
// treated as zero rather than a test failure. Used as
// TestE2EProcessBackend's pre-test baseline: some other, unrelated RIE
// process could plausibly already be running on a dev machine, so the
// cleanup check asserts a return to this count rather than assuming zero.
func countRIEProcesses(t *testing.T) int {
	t.Helper()

	out, err := exec.Command("pgrep", "-f", "aws-lambda-rie").CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return 0
		}

		t.Fatalf("pgrep -f aws-lambda-rie unexpected error: %v (%s)", err, out)
	}

	lines := strings.Fields(strings.TrimSpace(string(out)))

	return len(lines)
}

// waitForRIEProcessBaseline polls countRIEProcesses every 250ms until it
// returns to baseline or a 10s deadline expires, failing the test with the
// last observed count only in the latter case — the process-backend
// equivalent of waitForNoLabeledContainers, since Stop's SIGTERM (see
// internal/backend/process's package doc) isn't necessarily instant.
func waitForRIEProcessBaseline(t *testing.T, baseline int) {
	t.Helper()

	const (
		pollInterval = 250 * time.Millisecond
		pollTimeout  = 10 * time.Second
	)

	deadline := time.Now().Add(pollTimeout)

	var last int
	for {
		last = countRIEProcesses(t)
		if last <= baseline {
			return
		}

		if time.Now().After(deadline) {
			break
		}

		time.Sleep(pollInterval)
	}

	t.Errorf("aws-lambda-rie process count after StopAll() = %d, want back to baseline %d", last, baseline)
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
