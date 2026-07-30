package container

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// TestContainerBackendIntegration exercises the real docker CLI end to end:
// start a python3.13 hello function from testdata, invoke it over HTTP,
// stop it, and confirm no container is left behind. It only runs when
// go test's -short flag is absent AND a real docker daemon answers — per
// plans/m1-container-path.md Unit B, skipped rather than failed when either
// condition doesn't hold, since the point is to guard CI/hosts without
// Docker, not to require it.
func TestContainerBackendIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped: -short")
	}

	var runner backend.ExecRunner

	if _, err := runner.Run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("integration test skipped: docker daemon unreachable: %v", err)
	}

	dir, err := filepath.Abs("testdata/python-hello")
	if err != nil {
		t.Fatalf("filepath.Abs() unexpected error: %v", err)
	}

	fn := discovery.Function{
		Name:    "lambdary-container-it-python-hello",
		Dir:     dir,
		Runtime: "python3.13",
		Handler: "lambda_function.handler",
		Backend: "container",
		Manifest: &manifest.Manifest{
			Timeout: 30,
		},
	}

	// The first run of this test on a host pulls public.ecr.aws/lambda/
	// python:3.13, which can take a while — give readiness up to 120s per
	// the plan, with a little headroom on the outer context.
	b := &containerBackend{cli: "docker", runner: runner, readyTimeout: 120 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	inst, err := b.Start(ctx, fn)
	if err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}

	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}

		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := inst.Stop(stopCtx); err != nil {
			t.Errorf("cleanup Stop() unexpected error: %v", err)
		}
	})

	event, err := json.Marshal(map[string]string{"ping": "pong"})
	if err != nil {
		t.Fatalf("json.Marshal() unexpected error: %v", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Post(inst.InvokeURL(), "application/json", bytes.NewReader(event))
	if err != nil {
		t.Fatalf("POST %s unexpected error: %v", inst.InvokeURL(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, body = %s", inst.InvokeURL(), resp.StatusCode, body)
	}

	var result struct {
		Echo map[string]string `json:"echo"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshalling response body %q: %v", body, err)
	}

	if result.Echo["ping"] != "pong" {
		t.Errorf("response echo = %v, want {\"ping\": \"pong\"}", result.Echo)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := inst.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() unexpected error: %v", err)
	}
	stopped = true

	out, err := runner.Run(context.Background(), "docker", "ps", "-a",
		"--filter", "label=lambdary=1",
		"--filter", "label=lambdary.function="+fn.Name,
		"--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("docker ps unexpected error: %v", err)
	}

	if got := strings.TrimSpace(string(out)); got != "" {
		t.Errorf("docker ps after Stop() = %q, want no containers left for label lambdary.function=%s", got, fn.Name)
	}
}
