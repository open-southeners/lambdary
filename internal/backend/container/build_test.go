package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
)

// writeDockerfile creates a minimal Dockerfile in dir, standing in for a
// function's actual Dockerfile-marker file — build.go only checks for its
// presence before invoking `<cli> build`; it never inspects the contents.
func writeDockerfile(t *testing.T, dir string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("writing Dockerfile: %v", err)
	}
}

func TestDockerBuildTag(t *testing.T) {
	got := dockerBuildTag(discovery.Function{Name: "docker-app"})
	if want := "lambdary/docker-app:local"; got != want {
		t.Errorf("dockerBuildTag() = %q, want %q", got, want)
	}
}

func TestContainerBackendStartDockerfileFunction(t *testing.T) {
	t.Run("builds then runs the built tag, no handler argument", func(t *testing.T) {
		dir := t.TempDir()
		writeDockerfile(t, dir)

		hostPort := listenLoopback(t)

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"build": func(args []string) (string, error) { return "Successfully built abc123\n", nil },
			"run":   func(args []string) (string, error) { return "container123\n", nil },
			"port":  func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop":  func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second}

		fn := discovery.Function{
			Name: "docker-app",
			Dir:  dir,
			// A stray handler (e.g. inherited from a project-wide default)
			// must never reach the run argv for a Dockerfile function.
			Handler: "should-be-ignored.handler",
			Backend: "container",
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		buildCalls := r.callsFor("build")
		if len(buildCalls) != 1 {
			t.Fatalf("build calls = %d, want 1", len(buildCalls))
		}
		wantBuild := []string{"build", "-t", "lambdary/docker-app:local", dir}
		if !reflect.DeepEqual(buildCalls[0], wantBuild) {
			t.Errorf("build argv = %v, want %v", buildCalls[0], wantBuild)
		}

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		argv := runCalls[0]
		if argv[len(argv)-1] != "lambdary/docker-app:local" {
			t.Errorf("run argv = %v, want it to end with the built tag and no handler argument", argv)
		}

		buildIdx, runIdx := -1, -1
		for i, c := range r.calls {
			if len(c.args) == 0 {
				continue
			}
			switch c.args[0] {
			case "build":
				buildIdx = i
			case "run":
				if runIdx == -1 {
					runIdx = i
				}
			}
		}
		if buildIdx == -1 || runIdx == -1 || buildIdx > runIdx {
			t.Errorf("call order = %v, want build to precede run", r.calls)
		}
	})

	t.Run("build failure surfaces the output tail and never runs anything", func(t *testing.T) {
		dir := t.TempDir()
		writeDockerfile(t, dir)

		lines := make([]string, 30)
		for i := range lines {
			lines[i] = fmt.Sprintf("build log line %d", i+1)
		}
		buildOutput := strings.Join(lines, "\n") + "\n"

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"build": func(args []string) (string, error) { return buildOutput, errors.New("exit status 1") },
		}}

		b := &containerBackend{cli: "docker", runner: r}

		fn := discovery.Function{Name: "docker-app", Dir: dir, Backend: "container"}

		_, err := b.Start(context.Background(), fn)
		if err == nil {
			t.Fatal("Start() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "build log line 30") {
			t.Errorf("Start() error = %v, want it to include the tail of the build output", err)
		}
		if strings.Contains(err.Error(), "build log line 1\n") {
			t.Errorf("Start() error = %v, want only the tail (~20 lines), not the full output", err)
		}
		if calls := r.callsFor("run"); len(calls) != 0 {
			t.Errorf("run calls = %d, want 0 after a build failure", len(calls))
		}
	})

	t.Run("local.image override skips the build even with a Dockerfile present", func(t *testing.T) {
		dir := t.TempDir()
		writeDockerfile(t, dir)

		hostPort := listenLoopback(t)
		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "container123\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second}

		fn := discovery.Function{
			Name:    "docker-app",
			Dir:     dir,
			Backend: "container",
			Image:   "my-registry/custom:tag",
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		if calls := r.callsFor("build"); len(calls) != 0 {
			t.Errorf("build calls = %d, want 0 when local.image overrides", len(calls))
		}

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		if argv := runCalls[0]; argv[len(argv)-1] != "my-registry/custom:tag" {
			t.Errorf("run argv = %v, want the override image, not a built tag", argv)
		}
	})

	t.Run("runtime function never triggers a build, even with a Dockerfile alongside it", func(t *testing.T) {
		dir := t.TempDir()
		writeDockerfile(t, dir)

		hostPort := listenLoopback(t)
		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "container123\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second}

		fn := discovery.Function{
			Name:    "hello",
			Dir:     dir,
			Runtime: "python3.13",
			Handler: "lambda_function.handler",
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		if calls := r.callsFor("build"); len(calls) != 0 {
			t.Errorf("build calls = %d, want 0 for a runtime function", len(calls))
		}

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		if argv := runCalls[0]; argv[len(argv)-1] != "lambda_function.handler" {
			t.Errorf("run argv = %v, want the handler argument preserved for a runtime function", argv)
		}
	})
}
