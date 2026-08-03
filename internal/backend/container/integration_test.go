package container

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
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

	waitForNoLabeledContainers(t, runner, fn.Name)
}

// waitForNoLabeledContainers polls
// `docker ps -a --filter label=lambdary=1 --filter label=lambdary.function=<fnName>`
// every 250ms until it reports no containers or a 10s deadline expires,
// failing the test with the last output only in the latter case. A
// container run with --rm (as ours are) is removed asynchronously after
// `docker stop` returns, so an instant check right after Stop can still
// briefly see it — polling avoids that flake instead of asserting on a
// single point-in-time snapshot.
func waitForNoLabeledContainers(t *testing.T, runner backend.ExecRunner, fnName string) {
	t.Helper()

	const (
		pollInterval = 250 * time.Millisecond
		pollTimeout  = 10 * time.Second
	)

	deadline := time.Now().Add(pollTimeout)

	var last string
	for {
		out, err := runner.Run(context.Background(), "docker", "ps", "-a",
			"--filter", "label=lambdary=1",
			"--filter", "label=lambdary.function="+fnName,
			"--format", "{{.ID}}")
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

	t.Errorf("docker ps after Stop() = %q, want no containers left for label lambdary.function=%s", last, fnName)
}

// TestContainerBackendIntegrationDockerfileFunction exercises the real
// docker CLI for a Dockerfile-marker function end to end: build + start
// from testdata/dockerfile-echo, invoke, stop, no leftover containers —
// then mutate a private copy of the fixture's handler and Start again,
// proving the rebuild-on-every-Start behaviour (build.go's
// buildDockerfileImage doc comment) actually picks up the edit. Gated the
// same way as TestContainerBackendIntegration.
func TestContainerBackendIntegrationDockerfileFunction(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped: -short")
	}

	var runner backend.ExecRunner

	if _, err := runner.Run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("integration test skipped: docker daemon unreachable: %v", err)
	}

	// Work from a private copy so the test can mutate the handler without
	// touching the repo's testdata fixture.
	workDir := t.TempDir()
	copyDockerfileEchoFixture(t, workDir)

	fn := discovery.Function{
		Name:    "lambdary-container-it-dockerfile-echo",
		Dir:     workDir,
		Backend: "container",
		Manifest: &manifest.Manifest{
			Timeout: 30,
		},
	}

	tag := dockerBuildTag(fn)
	t.Cleanup(func() {
		// Best-effort: image removal failing (e.g. it was never built
		// because an earlier assertion failed first) shouldn't mask the
		// test's actual failure, but a passing run must not leave a
		// lambdary/*:local image behind on the host.
		runner.Run(context.Background(), "docker", "rmi", "-f", tag) //nolint:errcheck // cleanup best-effort
	})

	// The first build of this test on a host pulls public.ecr.aws/lambda/
	// python:3.13, which can take a while — give readiness up to 120s per
	// plans/m1-container-path.md Unit B's convention for the equivalent
	// pulled-image test, with a little headroom on the outer context.
	b := &containerBackend{cli: "docker", runner: runner, readyTimeout: 120 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	inst, err := b.Start(ctx, fn)
	cancel()
	if err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}

	assertDockerfileEcho(t, inst, false)

	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = inst.Stop(stopCtx)
	cancel()
	if err != nil {
		t.Fatalf("Stop() unexpected error: %v", err)
	}

	waitForNoLabeledContainers(t, runner, fn.Name)

	// Mutate the handler in place and Start again under the same fn.Name
	// (so it builds the same lambdary/<name>:local tag): Start rebuilds on
	// every call, so this proves the edit is picked up with no extra
	// reload machinery.
	mutated := "def handler(event, context):\n    return {\"echo\": event, \"mutated\": True}\n"
	if err := os.WriteFile(filepath.Join(workDir, "lambda_function.py"), []byte(mutated), 0o644); err != nil {
		t.Fatalf("writing mutated handler: %v", err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 150*time.Second)
	inst2, err := b.Start(ctx, fn)
	cancel()
	if err != nil {
		t.Fatalf("second Start() unexpected error: %v", err)
	}

	assertDockerfileEcho(t, inst2, true)

	stopCtx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	err = inst2.Stop(stopCtx)
	cancel()
	if err != nil {
		t.Fatalf("second Stop() unexpected error: %v", err)
	}

	waitForNoLabeledContainers(t, runner, fn.Name)
}

// copyDockerfileEchoFixture copies testdata/dockerfile-echo's files (flat,
// no subdirectories) into dir, so
// TestContainerBackendIntegrationDockerfileFunction can mutate its own
// private copy of the handler without ever touching the repo's testdata.
func copyDockerfileEchoFixture(t *testing.T, dir string) {
	t.Helper()

	srcDir, err := filepath.Abs("testdata/dockerfile-echo")
	if err != nil {
		t.Fatalf("filepath.Abs() unexpected error: %v", err)
	}

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%s) unexpected error: %v", srcDir, err)
	}

	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(srcDir, entry.Name()))
		if err != nil {
			t.Fatalf("reading fixture file %s: %v", entry.Name(), err)
		}

		if err := os.WriteFile(filepath.Join(dir, entry.Name()), data, 0o644); err != nil {
			t.Fatalf("writing fixture file %s: %v", entry.Name(), err)
		}
	}
}

// assertDockerfileEcho POSTs {"ping":"pong"} to inst's invoke URL and
// checks the response echoes it back, plus a "mutated" field matching
// wantMutated — the pre/post-edit marker
// TestContainerBackendIntegrationDockerfileFunction uses to prove a
// rebuilt image actually served the request.
func assertDockerfileEcho(t *testing.T, inst backend.Instance, wantMutated bool) {
	t.Helper()

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
		Echo    map[string]string `json:"echo"`
		Mutated bool              `json:"mutated"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshalling response body %q: %v", body, err)
	}

	if result.Echo["ping"] != "pong" {
		t.Errorf("response echo = %v, want {\"ping\": \"pong\"}", result.Echo)
	}
	if result.Mutated != wantMutated {
		t.Errorf("response mutated = %v, want %v (before/after the handler edit)", result.Mutated, wantMutated)
	}
}
