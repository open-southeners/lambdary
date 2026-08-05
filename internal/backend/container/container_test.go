package container

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// listenLoopback starts an httptest server on an ephemeral 127.0.0.1 port,
// standing in for a container's :8080 that Start's HTTP-based readiness
// probe (see port.go's waitReady) needs to succeed against without a real
// container.
func listenLoopback(t *testing.T) (port string) {
	t.Helper()

	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("net.SplitHostPort() unexpected error: %v", err)
	}

	return port
}

func TestContainerBackendStart(t *testing.T) {
	t.Run("happy path: run, resolve port, wait ready, InvokeURL", func(t *testing.T) {
		hostPort := listenLoopback(t)

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "container123\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second}

		fn := discovery.Function{
			Name:    "hello",
			Dir:     t.TempDir(),
			Handler: "index.handler",
			Runtime: "nodejs22.x",
			Manifest: &manifest.Manifest{
				Timeout: 10,
			},
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		want := "http://127.0.0.1:" + hostPort + "/2015-03-31/functions/function/invocations"
		if got := inst.InvokeURL(); got != want {
			t.Errorf("InvokeURL() = %q, want %q", got, want)
		}

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		if !strings.Contains(strings.Join(runCalls[0], " "), "-e AWS_LAMBDA_FUNCTION_TIMEOUT=10") {
			t.Errorf("run argv = %v, want AWS_LAMBDA_FUNCTION_TIMEOUT=10 present", runCalls[0])
		}

		if calls := r.callsFor("stop"); len(calls) != 0 {
			t.Errorf("stop calls before Stop() = %d, want 0", len(calls))
		}
	})

	t.Run("provided.* runtime with no bootstrap file errors without running anything", func(t *testing.T) {
		r := &fakeRunner{}
		b := &containerBackend{cli: "docker", runner: r}

		fn := discovery.Function{
			Name:     "custom-runtime",
			Dir:      t.TempDir(),
			Runtime:  "provided.al2023",
			Manifest: &manifest.Manifest{},
		}

		_, err := b.Start(context.Background(), fn)
		if err == nil {
			t.Fatal("Start() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "bootstrap") {
			t.Errorf("Start() error = %v, want it to mention bootstrap", err)
		}
		if calls := r.callsFor("run"); len(calls) != 0 {
			t.Errorf("run calls = %d, want 0 (Start must not run a container for a missing bootstrap)", len(calls))
		}
	})

	t.Run("provided.* runtime with a bootstrap file mounts it and appends the placeholder CMD", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "bootstrap"), "#!/bin/sh\n")

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "container123\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:" + listenLoopback(t) + "\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second}

		fn := discovery.Function{
			Name:     "custom-runtime",
			Dir:      dir,
			Runtime:  "provided.al2023",
			Manifest: &manifest.Manifest{},
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		argv := runCalls[0]
		wantMount := "-v " + filepath.Join(dir, "bootstrap") + ":/var/runtime/bootstrap:ro"
		if !strings.Contains(strings.Join(argv, " "), wantMount) {
			t.Errorf("run argv = %v, want it to contain %q", argv, wantMount)
		}
		if argv[len(argv)-1] != "bootstrap" {
			t.Errorf("run argv last element = %q, want the placeholder CMD %q", argv[len(argv)-1], "bootstrap")
		}
	})

	t.Run("Dockerfile-marker function with no actual Dockerfile errors without running anything", func(t *testing.T) {
		r := &fakeRunner{}
		b := &containerBackend{cli: "docker", runner: r}

		fn := discovery.Function{Name: "docker-app", Dir: t.TempDir(), Backend: "container"}

		_, err := b.Start(context.Background(), fn)
		if err == nil {
			t.Fatal("Start() expected error, got nil")
		}
		if len(r.calls) != 0 {
			t.Errorf("calls = %v, want no CLI invocations when fn.Dir has no Dockerfile", r.calls)
		}
	})

	t.Run("readiness timeout stops the container and surfaces logs", func(t *testing.T) {
		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "container123\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:1\n", nil }, // nothing listens on :1
			"logs": func(args []string) (string, error) { return "boot error\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 150 * time.Millisecond}

		fn := discovery.Function{Name: "slow", Dir: t.TempDir(), Runtime: "nodejs22.x", Manifest: &manifest.Manifest{}}

		start := time.Now()
		_, err := b.Start(context.Background(), fn)
		if err == nil {
			t.Fatal("Start() expected a readiness timeout error, got nil")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("Start() took %s, want it to respect the tiny readyTimeout", elapsed)
		}
		if !strings.Contains(err.Error(), "boot error") {
			t.Errorf("Start() error = %v, want it to include the logs tail", err)
		}

		if calls := r.callsFor("stop"); len(calls) != 1 {
			t.Errorf("stop calls = %d, want 1 (Start must clean up a container that never became ready)", len(calls))
		}
	})

	t.Run("local.image override wins over ImageFor for the run image", func(t *testing.T) {
		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "container123\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:" + listenLoopback(t) + "\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second}

		fn := discovery.Function{
			Name:    "hello",
			Dir:     t.TempDir(),
			Runtime: "python3.13",
			Image:   "my-registry/custom:tag",
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		argv := runCalls[0]
		if argv[len(argv)-1] != "my-registry/custom:tag" {
			t.Errorf("run argv image = %q, want the local.image override", argv[len(argv)-1])
		}
	})

	t.Run("local.env_file is loaded and merged under manifest environment", func(t *testing.T) {
		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "container123\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:" + listenLoopback(t) + "\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second}

		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".env"), "FOO=from-file\nBAR=file-only\n")

		fn := discovery.Function{
			Name:    "hello",
			Dir:     dir,
			Runtime: "python3.13",
			Manifest: &manifest.Manifest{
				Environment: map[string]string{"FOO": "from-manifest"},
				Local:       manifest.Local{EnvFile: ".env"},
			},
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		argv := strings.Join(runCalls[0], " ")
		if !strings.Contains(argv, "-e FOO=from-manifest") {
			t.Errorf("run argv = %v, want FOO=from-manifest (manifest environment wins over env_file)", runCalls[0])
		}
		if !strings.Contains(argv, "-e BAR=file-only") {
			t.Errorf("run argv = %v, want BAR=file-only from env_file", runCalls[0])
		}
	})

	t.Run("missing configured local.env_file errors naming the path, without running anything", func(t *testing.T) {
		r := &fakeRunner{}
		b := &containerBackend{cli: "docker", runner: r}

		dir := t.TempDir()
		fn := discovery.Function{
			Name:    "hello",
			Dir:     dir,
			Runtime: "python3.13",
			Manifest: &manifest.Manifest{
				Local: manifest.Local{EnvFile: "missing.env"},
			},
		}

		_, err := b.Start(context.Background(), fn)
		if err == nil {
			t.Fatal("Start() expected error, got nil")
		}
		if !strings.Contains(err.Error(), filepath.Join(dir, "missing.env")) {
			t.Errorf("Start() error = %v, want it to name the resolved env_file path", err)
		}
		if len(r.calls) != 0 {
			t.Errorf("calls = %v, want no CLI invocations for a missing env_file", r.calls)
		}
	})
}

// writeFile writes content to path, failing the test on any error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
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
		if want := map[string]string{"FOO": "bar"}; env["FOO"] != want["FOO"] {
			t.Errorf("loadEnvFile() = %v, want %v", env, want)
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

func TestInstanceStop(t *testing.T) {
	r := &fakeRunner{respond: map[string]func(args []string) (string, error){
		"stop": func(args []string) (string, error) { return "", nil },
	}}

	inst := &instance{cli: "docker", runner: r, id: "container123", port: "0"}

	if err := inst.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() unexpected error: %v", err)
	}
	if err := inst.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() unexpected error: %v", err)
	}

	if calls := r.callsFor("stop"); len(calls) != 1 {
		t.Errorf("stop calls = %d, want 1 (second Stop() must be a no-op)", len(calls))
	}
}

func TestInstanceStopIgnoresAlreadyGone(t *testing.T) {
	r := &fakeRunner{respond: map[string]func(args []string) (string, error){
		"stop": func(args []string) (string, error) {
			return "Error: No such container: container123", errors.New("exit status 1")
		},
	}}

	inst := &instance{cli: "docker", runner: r, id: "container123", port: "0"}

	if err := inst.Stop(context.Background()); err != nil {
		t.Errorf("Stop() unexpected error for an already-gone container: %v", err)
	}
}

func TestInstanceLogs(t *testing.T) {
	r := &fakeRunner{respond: map[string]func(args []string) (string, error){
		"logs": func(args []string) (string, error) { return "hello from the function\n", nil },
	}}

	inst := &instance{cli: "docker", runner: r, id: "container123", port: "0"}

	out, err := io.ReadAll(inst.Logs())
	if err != nil {
		t.Fatalf("reading Logs() unexpected error: %v", err)
	}
	if got := string(out); got != "hello from the function\n" {
		t.Errorf("Logs() = %q, want %q", got, "hello from the function\n")
	}

	calls := r.callsFor("logs")
	if len(calls) != 1 || calls[0][1] != "-f" {
		t.Errorf("logs calls = %v, want a single `logs -f <id>`", calls)
	}
}
