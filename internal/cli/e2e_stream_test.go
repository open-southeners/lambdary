package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/container"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/router"
)

// TestE2EStream is TestE2E's response-streaming companion, per
// plans/response-streaming.md's Unit B: it drives testdata/stream-demo
// through the exact same full stack (container backend, router.Manager,
// router.New) as TestE2E, proving url.invoke_mode: RESPONSE_STREAM end to
// end against a real nodejs22.x RIE rather than through internal/event's or
// internal/router's own unit tests. The fixture's "streamfn" handler is
// wrapped in awslambda.streamifyResponse and sets a non-default status
// code, its own Content-Type, and a custom header via
// awslambda.HttpResponseStream.from() before writing its body in chunks —
// a 200 response with the default application/json Content-Type and no
// custom header would mean the frame leaked into the body unparsed
// (plans/response-streaming.md's "What was measured" bug this unit fixes).
//
// Same Docker guard as TestE2E: skipped on -short or when no docker daemon
// answers `docker info`. Uses its own testdata/stream-demo fixture root,
// not testdata/demo — TestE2E hard-asserts len(fns) == 4 against
// testdata/demo, so adding a function there would break it.
func TestE2EStream(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test skipped: -short")
	}

	var runner backend.ExecRunner

	if _, err := runner.Run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("e2e test skipped: docker daemon unreachable: %v", err)
	}

	root, err := filepath.Abs("testdata/stream-demo")
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
	if len(fns) != 1 {
		t.Fatalf("discovered %d functions, want 1 (streamfn): %+v", len(fns), fns)
	}

	cli, err := backend.DetectContainerCLI(context.Background(), runner)
	if err != nil {
		t.Fatalf("DetectContainerCLI() unexpected error: %v", err)
	}

	b := container.New(cli, runner, t.TempDir())
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

	// Same generous headroom as TestE2E: a cold image pull is possible on
	// hosts other than this repo's own dev host.
	client := &http.Client{Timeout: 120 * time.Second}

	resp, err := client.Get(srv.URL + "/streamfn")
	if err != nil {
		t.Fatalf("GET %s/streamfn: %v", srv.URL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}

	// The handler's own status, Content-Type, and header, not RIE's
	// buffered-invoke defaults (200, application/octet-stream, no custom
	// header) — proves the frame's prelude was actually parsed.
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (the handler's own status, from the frame prelude); body = %s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("Content-Type"), "text/plain"; got != want {
		t.Errorf("Content-Type = %q, want %q (the handler's own, not a router default)", got, want)
	}
	if got, want := resp.Header.Get("X-Stream-Probe"), "yes"; got != want {
		t.Errorf("X-Stream-Probe = %q, want %q", got, want)
	}

	// The body must be exactly what the handler streamed — no NUL bytes
	// (the frame delimiter) and no prelude JSON leaking through, which is
	// exactly what happened before this unit (see the plan's "What was
	// measured" section).
	if want := []byte("chunk-1;chunk-2;"); !bytes.Equal(body, want) {
		t.Errorf("body = %q, want %q", body, want)
	}
	if bytes.ContainsRune(body, 0x00) {
		t.Errorf("body = %q, contains a NUL byte — the frame delimiter leaked through unparsed", body)
	}
	if bytes.Contains(body, []byte("statusCode")) {
		t.Errorf("body = %q, contains the prelude JSON — the frame leaked through unparsed", body)
	}
}
