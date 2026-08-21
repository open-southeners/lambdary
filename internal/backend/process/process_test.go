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

// writeFile writes content to path, failing the test on any error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
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

	t.Run("local.env_file is loaded and merged under manifest environment", func(t *testing.T) {
		riePath := fakeRIEPath(t)
		fnDir := t.TempDir()
		recordPath := filepath.Join(t.TempDir(), "record.json")

		writeFile(t, filepath.Join(fnDir, ".env"), "FOO=from-file\nBAR=file-only\n")

		b := &processBackend{riePath: riePath, home: t.TempDir(), readyTimeout: 5 * time.Second}

		fn := discovery.Function{
			Name:    "hello",
			Dir:     fnDir,
			Runtime: "nodejs22.x",
			Handler: "index.handler",
			Manifest: &manifest.Manifest{
				Environment: map[string]string{
					"FAKE_RIE_RECORD": recordPath,
					"FOO":             "from-manifest",
				},
				Local: manifest.Local{EnvFile: ".env"},
			},
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background()) //nolint:errcheck // best-effort cleanup.

		resp, err := http.Get(inst.InvokeURL())
		if err != nil {
			t.Fatalf("GET InvokeURL() unexpected error: %v", err)
		}
		resp.Body.Close() //nolint:errcheck // draining a test response body.

		data, err := os.ReadFile(recordPath)
		if err != nil {
			t.Fatalf("reading fake RIE's recorded argv/env/cwd: %v", err)
		}

		var rec fakeRIERecord
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("unmarshalling fake RIE record: %v", err)
		}

		if rec.Env["FOO"] != "from-manifest" {
			t.Errorf("env FOO = %q, want %q (manifest environment wins over env_file)", rec.Env["FOO"], "from-manifest")
		}
		if rec.Env["BAR"] != "file-only" {
			t.Errorf("env BAR = %q, want %q (from env_file)", rec.Env["BAR"], "file-only")
		}
	})

	t.Run("missing configured local.env_file errors naming the path, without spawning anything", func(t *testing.T) {
		fnDir := t.TempDir()
		b := &processBackend{riePath: "/should-not-be-invoked", home: t.TempDir()}
		fn := discovery.Function{
			Name:    "hello",
			Dir:     fnDir,
			Runtime: "nodejs22.x",
			Handler: "index.handler",
			Manifest: &manifest.Manifest{
				Local: manifest.Local{EnvFile: "missing.env"},
			},
		}

		_, err := b.Start(context.Background(), fn)
		if err == nil {
			t.Fatal("Start() expected error, got nil")
		}
		if !strings.Contains(err.Error(), filepath.Join(fnDir, "missing.env")) {
			t.Errorf("Start() error = %v, want it to name the resolved env_file path", err)
		}
	})

	t.Run("unsupported runtime errors before ever spawning", func(t *testing.T) {
		b := &processBackend{riePath: "/should-not-be-invoked", home: t.TempDir()}
		fn := discovery.Function{Name: "hello", Dir: t.TempDir(), Runtime: "java21", Manifest: &manifest.Manifest{}}

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
		// Two full seconds, not milliseconds: this needs to comfortably
		// outlast the script's own fork/exec/echo latency even when the host
		// is busy running other packages' tests in parallel (go test's
		// normal mode) — the assertion below only cares that Start() didn't
		// wait anywhere near the real defaultReadyTimeout, not that it was
		// fast in absolute terms.
		const readyTimeout = 2 * time.Second

		// Retried up to maxAttempts times: under a genuinely saturated host
		// (e.g. every package's tests running in parallel at once, each
		// itself spawning processes), the OS can occasionally fail to
		// schedule this test's brand-new child process at all within the
		// whole readyTimeout window, so it never even reaches its first
		// "echo" line — confirmed by instrumenting Start's readiness-failure
		// path during triage: the failing run showed a fully-drained, empty
		// buffer (proc.stop returned in ~160µs, tailLen=0), not a
		// partially-written one, ruling out a snapshot-before-drain race
		// (see process.go's Start, which now stops — and so waits for the
		// output-copying goroutine to finish — before snapshotting the
		// tail). That's host scheduling noise, not a defect in Start, so a
		// bounded retry here is the right tool rather than loosening the
		// assertion or growing readyTimeout.
		const maxAttempts = 3

		var err error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			riePath := writeTestScript(t, "#!/bin/sh\necho boot failed >&2\nsleep 5\n")
			b := &processBackend{riePath: riePath, home: t.TempDir(), readyTimeout: readyTimeout}
			fn := discovery.Function{Name: "slow", Dir: t.TempDir(), Runtime: "nodejs22.x", Handler: "index.handler", Manifest: &manifest.Manifest{}}

			start := time.Now()
			_, err = b.Start(context.Background(), fn)
			if err == nil {
				t.Fatal("Start() expected a readiness timeout error, got nil")
			}
			if elapsed := time.Since(start); elapsed > 6*time.Second {
				t.Errorf("Start() took %s, want it to respect the tiny readyTimeout", elapsed)
			}

			if strings.Contains(err.Error(), "boot failed") {
				return
			}

			t.Logf("attempt %d/%d: Start() error = %v, missing the output tail (likely the child never got scheduled within readyTimeout under host load); retrying", attempt, maxAttempts, err)
		}

		t.Errorf("Start() error = %v, want it to include the recent output tail after %d attempts", err, maxAttempts)
	})

	t.Run("layers: a cache-miss ARN fails Start with context, before ever spawning", func(t *testing.T) {
		b := &processBackend{riePath: "/should-not-be-invoked", home: t.TempDir(), cacheDir: t.TempDir()}
		fn := discovery.Function{
			Name:    "hello",
			Dir:     t.TempDir(),
			Runtime: "nodejs22.x",
			Handler: "index.handler",
			Manifest: &manifest.Manifest{
				Layers: []string{"arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1"},
			},
		}

		_, err := b.Start(context.Background(), fn)
		if err == nil {
			t.Fatal("Start() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "hello") {
			t.Errorf("Start() error = %v, want it to name the function", err)
		}
		if !strings.Contains(err.Error(), "php-83") {
			t.Errorf("Start() error = %v, want it to name the uncached layer ARN", err)
		}
	})

	t.Run("layers: an invalid layers: entry fails Start naming the entry", func(t *testing.T) {
		b := &processBackend{riePath: "/should-not-be-invoked", home: t.TempDir(), cacheDir: t.TempDir()}
		fn := discovery.Function{
			Name:    "hello",
			Dir:     t.TempDir(),
			Runtime: "nodejs22.x",
			Handler: "index.handler",
			Manifest: &manifest.Manifest{
				Layers: []string{"arn:aws:lambda:eu-west-1:not-an-arn"},
			},
		}

		_, err := b.Start(context.Background(), fn)
		if err == nil {
			t.Fatal("Start() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "arn:aws:lambda:eu-west-1:not-an-arn") {
			t.Errorf("Start() error = %v, want it to name the bad layer entry", err)
		}
	})
}

func TestParseFunctionLayerRefs(t *testing.T) {
	t.Run("no manifest returns nil, no error", func(t *testing.T) {
		refs, err := parseFunctionLayerRefs(discovery.Function{Name: "hello"})
		if err != nil {
			t.Fatalf("parseFunctionLayerRefs() unexpected error: %v", err)
		}
		if refs != nil {
			t.Errorf("parseFunctionLayerRefs() = %v, want nil", refs)
		}
	})

	t.Run("no layers configured returns nil, no error", func(t *testing.T) {
		fn := discovery.Function{Name: "hello", Manifest: &manifest.Manifest{}}

		refs, err := parseFunctionLayerRefs(fn)
		if err != nil {
			t.Fatalf("parseFunctionLayerRefs() unexpected error: %v", err)
		}
		if refs != nil {
			t.Errorf("parseFunctionLayerRefs() = %v, want nil", refs)
		}
	})

	t.Run("a mix of an ARN and a local path parses both, in order", func(t *testing.T) {
		fn := discovery.Function{
			Name: "hello",
			Manifest: &manifest.Manifest{
				Layers: []string{"arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1", "../shared-layer"},
			},
		}

		refs, err := parseFunctionLayerRefs(fn)
		if err != nil {
			t.Fatalf("parseFunctionLayerRefs() unexpected error: %v", err)
		}
		if len(refs) != 2 {
			t.Fatalf("parseFunctionLayerRefs() = %v, want 2 refs", refs)
		}
		if refs[0].Kind != manifest.LayerRefKindARN {
			t.Errorf("refs[0].Kind = %q, want %q", refs[0].Kind, manifest.LayerRefKindARN)
		}
		if refs[1].Kind != manifest.LayerRefKindPath {
			t.Errorf("refs[1].Kind = %q, want %q", refs[1].Kind, manifest.LayerRefKindPath)
		}
	})

	t.Run("an invalid entry errors naming it", func(t *testing.T) {
		fn := discovery.Function{
			Name:     "hello",
			Manifest: &manifest.Manifest{Layers: []string{"arn:aws:lambda:eu-west-1:not-an-arn"}},
		}

		_, err := parseFunctionLayerRefs(fn)
		if err == nil {
			t.Fatal("parseFunctionLayerRefs() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "arn:aws:lambda:eu-west-1:not-an-arn") {
			t.Errorf("parseFunctionLayerRefs() error = %v, want it to name the bad entry", err)
		}
	})
}

func TestLoadEnvFile(t *testing.T) {
	t.Run("no local.env_file configured returns a nil map, no error", func(t *testing.T) {
		fn := discovery.Function{Name: "hello", Manifest: &manifest.Manifest{}}

		env, err := loadEnvFile(fn, t.TempDir())
		if err != nil {
			t.Fatalf("loadEnvFile() unexpected error: %v", err)
		}
		if env != nil {
			t.Errorf("loadEnvFile() = %v, want nil", env)
		}
	})

	t.Run("nil manifest behaves like an empty one", func(t *testing.T) {
		fn := discovery.Function{Name: "hello"}

		env, err := loadEnvFile(fn, t.TempDir())
		if err != nil {
			t.Fatalf("loadEnvFile() unexpected error: %v", err)
		}
		if env != nil {
			t.Errorf("loadEnvFile() = %v, want nil", env)
		}
	})

	t.Run("relative path resolves against absDir", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "config.env"), "FOO=bar\n")

		fn := discovery.Function{Name: "hello", Manifest: &manifest.Manifest{Local: manifest.Local{EnvFile: "config.env"}}}

		env, err := loadEnvFile(fn, dir)
		if err != nil {
			t.Fatalf("loadEnvFile() unexpected error: %v", err)
		}
		if env["FOO"] != "bar" {
			t.Errorf("loadEnvFile() = %v, want FOO=bar", env)
		}
	})

	t.Run("absolute path is used as-is", func(t *testing.T) {
		otherDir := t.TempDir()
		absPath := filepath.Join(otherDir, "outside.env")
		writeFile(t, absPath, "FOO=abs\n")

		fn := discovery.Function{Name: "hello", Manifest: &manifest.Manifest{Local: manifest.Local{EnvFile: absPath}}}

		env, err := loadEnvFile(fn, t.TempDir())
		if err != nil {
			t.Fatalf("loadEnvFile() unexpected error: %v", err)
		}
		if env["FOO"] != "abs" {
			t.Errorf("loadEnvFile() = %v, want FOO=abs", env)
		}
	})

	t.Run("missing configured file errors naming the resolved path", func(t *testing.T) {
		dir := t.TempDir()
		fn := discovery.Function{Name: "hello", Manifest: &manifest.Manifest{Local: manifest.Local{EnvFile: "nope.env"}}}

		_, err := loadEnvFile(fn, dir)
		if err == nil {
			t.Fatal("loadEnvFile() expected error, got nil")
		}
		if !strings.Contains(err.Error(), filepath.Join(dir, "nope.env")) {
			t.Errorf("loadEnvFile() error = %v, want it to name %s", err, filepath.Join(dir, "nope.env"))
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
