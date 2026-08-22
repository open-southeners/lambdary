package cli

import (
	"context"
	"encoding/json"
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

// TestE2ELayers is TestE2E's layers companion, per plans/layers.md's Unit
// F: it drives testdata/layers-demo through the exact same full stack
// (container backend, router.Manager, router.New) as TestE2E, proving
// `layers:` end to end rather than through internal/layers' or
// internal/backend/container's own unit/integration tests. The fixture's
// "layered" function has no local copy of the greeting module it imports —
// only its sibling "shared-layer" directory (a local-path layer entry,
// resolved relative to the function dir per manifest.ParseLayerRef) has
// it, so a 200 response containing the layer's message proves
// containerBackend.Start really staged and mounted /opt from the layer,
// not from the function's own code.
//
// Same Docker guard as TestE2E: skipped on -short or when no docker daemon
// answers `docker info`.
func TestE2ELayers(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test skipped: -short")
	}

	var runner backend.ExecRunner

	if _, err := runner.Run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("e2e test skipped: docker daemon unreachable: %v", err)
	}

	root, err := filepath.Abs("testdata/layers-demo")
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
	// shared-layer has no .lambda.yml and no recognizable project marker
	// file of its own, so discovery skips it as a function — only
	// "layered" is discovered here.
	if len(fns) != 1 {
		t.Fatalf("discovered %d functions, want 1 (layered): %+v", len(fns), fns)
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

	resp, err := client.Get(srv.URL + "/layered")
	if err != nil {
		t.Fatalf("GET %s/layered: %v", srv.URL, err)
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
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshalling response body %q: %v", body, err)
	}

	if want := "hello from the layer"; result.Message != want {
		t.Errorf("message = %q, want %q (the handler's own code has no greeting module — this can only come from the staged /opt layer)", result.Message, want)
	}
}
