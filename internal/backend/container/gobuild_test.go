package container

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/gobuild"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// buildCalls returns the argv of every `env ...` call the fake runner saw,
// in order — the gobuild command — plus the index of each within calls.
func buildCalls(f *fakeRunner) (argvs [][]string, indexes []int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i, c := range f.calls {
		if c.name == "env" {
			argvs = append(argvs, c.args)
			indexes = append(indexes, i)
		}
	}

	return argvs, indexes
}

// outputFlag returns the value following "-o" in argv.
func outputFlag(argv []string) string {
	for i, a := range argv {
		if a == "-o" && i+1 < len(argv) {
			return argv[i+1]
		}
	}

	return ""
}

// goBuildResponder scripts the fake `env ... go build` call: it writes a
// stand-in bootstrap at the -o path (as a real build would) and returns
// buildErr, if non-nil.
func goBuildResponder(buildErr error) func(args []string) (string, error) {
	return func(args []string) (string, error) {
		if buildErr != nil {
			return "# example.com/m\nmain.go:3:1: syntax error\n", buildErr
		}

		out := outputFlag(args)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return "", err
		}

		return "", os.WriteFile(out, []byte("#!/bin/sh\n"), 0o755)
	}
}

func TestContainerBackendStartGoFunction(t *testing.T) {
	newFn := func(t *testing.T, arch []string) discovery.Function {
		t.Helper()

		return discovery.Function{
			Name:    "ping",
			Dir:     t.TempDir(),
			Runtime: "provided.al2023",
			Handler: "bootstrap",
			Manifest: &manifest.Manifest{
				Architectures: arch,
			},
			Go: &discovery.GoSource{ModuleDir: "/src/mod", Package: "./cmd/ping", Tags: []string{"netgo"}},
		}
	}

	t.Run("builds for linux then mounts the built bootstrap", func(t *testing.T) {
		hostPort := listenLoopback(t)
		cacheDir := t.TempDir()

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"CGO_ENABLED=0": goBuildResponder(nil),
			"run":           func(args []string) (string, error) { return "container123\n", nil },
			"port":          func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop":          func(args []string) (string, error) { return "", nil },
		}}
		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second, cacheDir: cacheDir}

		inst, err := b.Start(t.Context(), newFn(t, []string{"arm64"}))
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(t.Context())

		wantOut := gobuild.OutputPath(cacheDir, "ping", "linux", "arm64")

		builds, buildIdx := buildCalls(r)
		if len(builds) != 1 {
			t.Fatalf("build calls = %d, want 1", len(builds))
		}
		joined := strings.Join(builds[0], " ")
		for _, want := range []string{"GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0", "go -C /src/mod build", "lambda.norpc,netgo", "-o " + wantOut, "./cmd/ping"} {
			if !strings.Contains(joined, want) {
				t.Errorf("build argv = %q, want it to contain %q", joined, want)
			}
		}

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		if wantMount := wantOut + ":/var/runtime/bootstrap"; !strings.Contains(strings.Join(runCalls[0], " "), wantMount) {
			t.Errorf("run argv = %v, want it to contain mount %q", runCalls[0], wantMount)
		}

		// The build must precede the container run.
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, c := range r.calls {
			if len(c.args) > 0 && c.args[0] == "run" && i < buildIdx[0] {
				t.Errorf("run issued at call %d before the build at %d", i, buildIdx[0])
			}
		}
	})

	t.Run("x86_64 architecture builds for amd64", func(t *testing.T) {
		hostPort := listenLoopback(t)
		cacheDir := t.TempDir()

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"CGO_ENABLED=0": goBuildResponder(nil),
			"run":           func(args []string) (string, error) { return "container123\n", nil },
			"port":          func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop":          func(args []string) (string, error) { return "", nil },
		}}
		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second, cacheDir: cacheDir}

		inst, err := b.Start(t.Context(), newFn(t, []string{"x86_64"}))
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(t.Context())

		builds, _ := buildCalls(r)
		if len(builds) != 1 || !strings.Contains(strings.Join(builds[0], " "), "GOARCH=amd64") {
			t.Fatalf("build argv = %v, want GOARCH=amd64", builds)
		}
		if got, want := outputFlag(builds[0]), gobuild.OutputPath(cacheDir, "ping", "linux", "amd64"); got != want {
			t.Errorf("-o = %q, want %q", got, want)
		}
	})

	t.Run("build failure errors and issues no run", func(t *testing.T) {
		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"CGO_ENABLED=0": goBuildResponder(errors.New("exit status 1")),
		}}
		b := &containerBackend{cli: "docker", runner: r, readyTimeout: time.Second, cacheDir: t.TempDir()}

		_, err := b.Start(t.Context(), newFn(t, []string{"arm64"}))
		if err == nil {
			t.Fatal("Start() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "syntax error") {
			t.Errorf("Start() error = %v, want it to carry the build output", err)
		}
		if calls := r.callsFor("run"); len(calls) != 0 {
			t.Errorf("run calls = %d, want 0 after a failed build", len(calls))
		}
	})

	t.Run("non-Go function issues no build", func(t *testing.T) {
		hostPort := listenLoopback(t)

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "container123\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}
		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second, cacheDir: t.TempDir()}

		fn := discovery.Function{Name: "hello", Dir: t.TempDir(), Runtime: "nodejs22.x", Handler: "index.handler", Manifest: &manifest.Manifest{}}
		inst, err := b.Start(t.Context(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(t.Context())

		if builds, _ := buildCalls(r); len(builds) != 0 {
			t.Errorf("build calls = %v, want none", builds)
		}
	})
}
