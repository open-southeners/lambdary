package process

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// fakeRIERecord mirrors the JSON testdata/fakerie/fake_rie.py writes when
// $FAKE_RIE_RECORD is set: its own argv, cwd, and full environment, so
// TestProcessBackendStart can assert Start() assembled the right RIE argv,
// cwd, and env without needing the real RIE or a host language runtime.
type fakeRIERecord struct {
	Argv []string          `json:"argv"`
	Cwd  string            `json:"cwd"`
	Env  map[string]string `json:"env"`
}

// fakeRIEPath resolves testdata/fakerie/fake_rie.py, skipping the test if
// python3 isn't on PATH (the fake RIE's own interpreter, unrelated to
// whether the *function* under test is Node or Python).
func fakeRIEPath(t *testing.T) string {
	t.Helper()
	requireBin(t, "python3")

	abs, err := filepath.Abs(filepath.Join("testdata", "fakerie", "fake_rie.py"))
	if err != nil {
		t.Fatalf("resolving fake rie path: %v", err)
	}

	return abs
}

// writeTestScript writes an executable shell script with the given content
// into a fresh temp dir and returns its path — used as a riePath stand-in
// for tests that only care about Start's readiness-timeout cleanup path,
// not a real invoke round trip.
func writeTestScript(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "fake-rie")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("writing test script: %v", err)
	}

	return path
}

func TestProcessBackendStart(t *testing.T) {
	t.Run("happy path: writes shims, allocates ports, spawns with cwd/env, waits ready", func(t *testing.T) {
		riePath := fakeRIEPath(t)
		fnDir := t.TempDir()
		recordPath := filepath.Join(t.TempDir(), "record.json")

		b := &processBackend{riePath: riePath, home: t.TempDir(), readyTimeout: 5 * time.Second}

		fn := discovery.Function{
			Name:    "hello",
			Dir:     fnDir,
			Runtime: "nodejs22.x",
			Handler: "index.handler",
			Manifest: &manifest.Manifest{
				Timeout: 7,
				Environment: map[string]string{
					"FAKE_RIE_RECORD": recordPath,
					"TABLE_NAME":      "local-table",
				},
			},
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background()) //nolint:errcheck // best-effort cleanup; the test's own Stop() call below is what's asserted.

		if !strings.HasPrefix(inst.InvokeURL(), "http://127.0.0.1:") || !strings.HasSuffix(inst.InvokeURL(), "/2015-03-31/functions/function/invocations") {
			t.Errorf("InvokeURL() = %q, want the Invoke API shape", inst.InvokeURL())
		}

		resp, err := http.Get(inst.InvokeURL())
		if err != nil {
			t.Fatalf("GET InvokeURL() unexpected error: %v", err)
		}
		resp.Body.Close() //nolint:errcheck // draining a test response body.
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET InvokeURL() status = %d, want 200 (the fake RIE answers everything with 200)", resp.StatusCode)
		}

		data, err := os.ReadFile(recordPath)
		if err != nil {
			t.Fatalf("reading fake RIE's recorded argv/env/cwd: %v", err)
		}

		var rec fakeRIERecord
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("unmarshalling fake RIE record: %v", err)
		}

		wantCwd, err := filepath.EvalSymlinks(fnDir)
		if err != nil {
			t.Fatalf("resolving fnDir symlinks: %v", err)
		}
		gotCwd, err := filepath.EvalSymlinks(rec.Cwd)
		if err != nil {
			t.Fatalf("resolving recorded cwd symlinks: %v", err)
		}
		if gotCwd != wantCwd {
			t.Errorf("spawned cwd = %q, want the function directory %q", gotCwd, wantCwd)
		}

		if len(rec.Argv) < 3 {
			t.Fatalf("recorded argv = %v, want at least the trailing runtime command", rec.Argv)
		}
		trailing := rec.Argv[len(rec.Argv)-3:]
		if trailing[0] != "node" {
			t.Errorf("trailing runtime command = %v, want it to start with \"node\"", trailing)
		}
		if !strings.HasSuffix(trailing[1], nodeShimName) {
			t.Errorf("trailing runtime command = %v, want the second arg to be the node shim path", trailing)
		}
		if trailing[2] != "index.handler" {
			t.Errorf("trailing runtime command = %v, want the handler as the last arg", trailing)
		}

		if !contains(rec.Argv, "--runtime-interface-emulator-address") || !contains(rec.Argv, "--runtime-api-address") {
			t.Errorf("recorded argv = %v, want both RIE address flags", rec.Argv)
		}

		if rec.Env["AWS_LAMBDA_FUNCTION_NAME"] != "hello" {
			t.Errorf("env AWS_LAMBDA_FUNCTION_NAME = %q, want %q", rec.Env["AWS_LAMBDA_FUNCTION_NAME"], "hello")
		}
		if rec.Env["AWS_LAMBDA_FUNCTION_TIMEOUT"] != "7" {
			t.Errorf("env AWS_LAMBDA_FUNCTION_TIMEOUT = %q, want %q", rec.Env["AWS_LAMBDA_FUNCTION_TIMEOUT"], "7")
		}
		if rec.Env["TABLE_NAME"] != "local-table" {
			t.Errorf("env TABLE_NAME = %q, want %q", rec.Env["TABLE_NAME"], "local-table")
		}
		if rec.Env["PATH"] == "" {
			t.Error("env PATH is empty, want the host PATH to be inherited")
		}

		if err := inst.Stop(context.Background()); err != nil {
			t.Fatalf("Stop() unexpected error: %v", err)
		}

		if _, err := http.Get(inst.InvokeURL()); err == nil {
			t.Error("GET InvokeURL() after Stop() unexpectedly succeeded, want the process to be gone")
		}

		if err := inst.Stop(context.Background()); err != nil {
			t.Errorf("second Stop() unexpected error: %v", err)
		}
	})

	t.Run("unsupported runtime errors before ever spawning", func(t *testing.T) {
		b := &processBackend{riePath: "/should-not-be-invoked", home: t.TempDir()}
		fn := discovery.Function{Name: "hello", Dir: t.TempDir(), Runtime: "ruby3.3", Manifest: &manifest.Manifest{}}

		_, err := b.Start(context.Background(), fn)
		if !errors.Is(err, ErrRuntimeNotSupported) {
			t.Errorf("Start() error = %v, want wrapping ErrRuntimeNotSupported", err)
		}
	})

	t.Run("provided.* with no bootstrap errors before ever spawning", func(t *testing.T) {
		b := &processBackend{riePath: "/should-not-be-invoked", home: t.TempDir()}
		fn := discovery.Function{Name: "custom", Dir: t.TempDir(), Runtime: "provided.al2023", Manifest: &manifest.Manifest{}}

		_, err := b.Start(context.Background(), fn)
		if !errors.Is(err, ErrBootstrapMissing) {
			t.Errorf("Start() error = %v, want wrapping ErrBootstrapMissing", err)
		}
	})

	t.Run("readiness timeout stops the process and surfaces recent output", func(t *testing.T) {
		riePath := writeTestScript(t, "#!/bin/sh\necho boot failed >&2\nsleep 5\n")

		// Two full seconds, not milliseconds: this needs to comfortably
		// outlast the script's own fork/exec/echo latency even when the host
		// is busy running other packages' tests in parallel (go test's
		// normal mode) — the assertion below only cares that Start() didn't
		// wait anywhere near the real defaultReadyTimeout, not that it was
		// fast in absolute terms.
		b := &processBackend{riePath: riePath, home: t.TempDir(), readyTimeout: 2 * time.Second}
		fn := discovery.Function{Name: "slow", Dir: t.TempDir(), Runtime: "nodejs22.x", Handler: "index.handler", Manifest: &manifest.Manifest{}}

		start := time.Now()
		_, err := b.Start(context.Background(), fn)
		if err == nil {
			t.Fatal("Start() expected a readiness timeout error, got nil")
		}
		if elapsed := time.Since(start); elapsed > 6*time.Second {
			t.Errorf("Start() took %s, want it to respect the tiny readyTimeout", elapsed)
		}
		if !strings.Contains(err.Error(), "boot failed") {
			t.Errorf("Start() error = %v, want it to include the recent output tail", err)
		}
	})
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}
