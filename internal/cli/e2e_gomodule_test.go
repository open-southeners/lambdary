package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/container"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/router"
)

// TestE2EGoModule drives testdata/go-module — one Go module holding two
// cmd/<name> functions listed in lambdary.yml's `functions:`, sharing an
// internal package — through the full stack (container backend,
// router.Manager, router.New). It proves that no bootstrap has to be built
// by hand, that build.go.ldflags reaches the binary, and that a restart (what
// hot reload does on a code change) rebuilds from the edited source.
//
// Same Docker guard as TestE2E, plus a `go` toolchain on PATH. The fixture
// is copied to a temp dir first, since the test edits its source.
func TestE2EGoModule(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test skipped: -short")
	}

	var runner backend.ExecRunner

	if _, err := runner.Run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("e2e test skipped: docker daemon unreachable: %v", err)
	}
	if _, err := runner.Run(context.Background(), "go", "version"); err != nil {
		t.Skipf("e2e test skipped: no go toolchain: %v", err)
	}

	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("testdata/go-module")); err != nil {
		t.Fatalf("copying fixture: %v", err)
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
		t.Fatalf("discovered %d functions, want 2 (echo, hello): %+v", len(fns), fns)
	}

	cli, err := backend.DetectContainerCLI(context.Background(), runner)
	if err != nil {
		t.Fatalf("DetectContainerCLI() unexpected error: %v", err)
	}

	b := container.New(cli, runner, filepath.Join(root, ".lambdary"))
	mgr := router.NewManager(b, fns)

	srv := httptest.NewServer(router.New(mgr, fns))
	defer srv.Close()

	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := mgr.StopAll(stopCtx); err != nil {
			t.Errorf("cleanup StopAll() unexpected error: %v", err)
		}

		waitForNoLabeledContainers(t, runner, "docker ps after StopAll()")
	})

	client := &http.Client{Timeout: 120 * time.Second}

	get := func(path string) string {
		t.Helper()

		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("GET %s: reading body: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200; body = %s", path, resp.StatusCode, body)
		}

		return string(body)
	}

	if got, want := get("/hello"), "hello says v1 (e2e)"; got != want {
		t.Errorf("GET /hello = %q, want %q", got, want)
	}
	if got, want := get("/say"), "echo says v1 (e2e)"; got != want {
		t.Errorf("GET /say = %q, want %q", got, want)
	}

	greet := filepath.Join(root, "internal", "greet", "greet.go")

	src, err := os.ReadFile(greet)
	if err != nil {
		t.Fatalf("reading %s: %v", greet, err)
	}
	if err := os.WriteFile(greet, []byte(strings.Replace(string(src), "says v1", "says v2", 1)), 0o644); err != nil {
		t.Fatalf("editing %s: %v", greet, err)
	}

	if err := mgr.Restart(context.Background(), "hello"); err != nil {
		t.Fatalf("Restart(hello) unexpected error: %v", err)
	}

	if got, want := get("/hello"), "hello says v2 (e2e)"; got != want {
		t.Errorf("GET /hello after edit + restart = %q, want %q (bootstrap not rebuilt)", got, want)
	}
}
