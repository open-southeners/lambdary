package rie

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
)

// call records one Runner.Run invocation, for tests to assert both the
// exact argv issued and the sequence of commands (git before go before
// clone before build before verify).
type call struct {
	name string
	args []string
}

// fakeRunner is a small backend.Runner test double local to this package,
// per the container package's precedent of writing its own fake rather than
// sharing one across packages. respond, when set, is consulted for every
// Run call and can fail the test outright (e.g. when a chain step should
// never reach the runner, such as the env-override or cache-hit paths).
type fakeRunner struct {
	mu    sync.Mutex
	calls []call

	respond func(name string, args []string) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...)})
	f.mu.Unlock()

	if f.respond != nil {
		return f.respond(name, args)
	}

	return nil, nil
}

func (f *fakeRunner) Start(context.Context, string, ...string) (backend.Cmd, error) {
	return nil, errors.New("fakeRunner: Start not implemented — rie.Resolve never calls it")
}

func failOnAnyCall(t *testing.T) *fakeRunner {
	t.Helper()

	return &fakeRunner{
		respond: func(name string, args []string) ([]byte, error) {
			t.Fatalf("unexpected runner call: %s %s", name, strings.Join(args, " "))
			return nil, nil
		},
	}
}

func TestResolveEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-lambda-rie")

	if err := os.WriteFile(path, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatalf("os.WriteFile() unexpected error: %v", err)
	}

	t.Setenv(envRIEPath, path)

	got, err := Resolve(context.Background(), failOnAnyCall(t))
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}

	if got != path {
		t.Errorf("Resolve() = %q, want %q", got, path)
	}
}

func TestResolveEnvOverrideNotExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-lambda-rie")

	if err := os.WriteFile(path, []byte("not a binary"), 0o644); err != nil {
		t.Fatalf("os.WriteFile() unexpected error: %v", err)
	}

	t.Setenv(envRIEPath, path)

	_, err := Resolve(context.Background(), failOnAnyCall(t))
	if err == nil {
		t.Fatal("Resolve() error = nil, want non-executable error")
	}

	if !strings.Contains(err.Error(), envRIEPath) {
		t.Errorf("Resolve() error = %q, want it to mention %s", err, envRIEPath)
	}
}

func TestResolveEnvOverrideMissing(t *testing.T) {
	t.Setenv(envRIEPath, filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := Resolve(context.Background(), failOnAnyCall(t))
	if err == nil {
		t.Fatal("Resolve() error = nil, want a missing-file error")
	}

	if !strings.Contains(err.Error(), envRIEPath) {
		t.Errorf("Resolve() error = %q, want it to mention %s", err, envRIEPath)
	}
}

func TestResolveCacheHit(t *testing.T) {
	home := t.TempDir()
	t.Setenv(envHome, home)

	name := fmt.Sprintf("aws-lambda-rie-%s-%s-%s", Version, runtime.GOOS, runtime.GOARCH)
	cached := filepath.Join(home, "bin", name)

	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatalf("os.MkdirAll() unexpected error: %v", err)
	}

	if err := os.WriteFile(cached, []byte("cached binary"), 0o755); err != nil {
		t.Fatalf("os.WriteFile() unexpected error: %v", err)
	}

	got, err := Resolve(context.Background(), failOnAnyCall(t))
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}

	if got != cached {
		t.Errorf("Resolve() = %q, want %q", got, cached)
	}
}

// outPathPattern extracts the -o argument out.go build's -o <path>
// interpolates into the `sh -c` command build issues, so the fake can
// create that exact file itself instead of actually invoking git/go.
var outPathPattern = regexp.MustCompile(`-o ('[^']*'|\S+)`)

func TestResolveBuildsFromSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv(envHome, home)
	t.Setenv(envRIEPath, "")

	runner := &fakeRunner{}
	runner.respond = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "git" && len(args) > 0 && args[0] == "--version":
			return []byte("git version 2.43.0"), nil
		case name == "go" && len(args) > 0 && args[0] == "version":
			return []byte("go version go1.25.5 darwin/arm64"), nil
		case name == "git" && len(args) > 0 && args[0] == "clone":
			return nil, nil
		case name == "sh" && len(args) > 0 && args[0] == "-c":
			m := outPathPattern.FindStringSubmatch(args[1])
			if m == nil {
				t.Fatalf("build command %q has no -o output path", args[1])
			}

			outPath := strings.Trim(m[1], "'")

			if err := os.WriteFile(outPath, []byte("fake built rie binary"), 0o755); err != nil {
				t.Fatalf("fake build step: writing %s: %v", outPath, err)
			}

			return nil, nil
		default:
			t.Fatalf("unexpected runner call: %s %s", name, strings.Join(args, " "))
			return nil, nil
		}
	}

	got, err := Resolve(context.Background(), runner)
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}

	wantCache := filepath.Join(home, "bin", fmt.Sprintf("aws-lambda-rie-%s-%s-%s", Version, runtime.GOOS, runtime.GOARCH))
	if got != wantCache {
		t.Errorf("Resolve() = %q, want %q", got, wantCache)
	}

	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("os.Stat(%s) unexpected error: %v", got, err)
	}

	if info.Mode()&0o111 == 0 {
		t.Errorf("cached binary mode = %v, want executable", info.Mode())
	}

	content, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("os.ReadFile(%s) unexpected error: %v", got, err)
	}

	if string(content) != "fake built rie binary" {
		t.Errorf("cached binary content = %q, want the built binary's content", content)
	}

	// Assert the argv sequence: git --version, go version, git clone
	// (correct tag/URL), then sh -c build. The freshly built binary is
	// never itself run through the runner (see verifyBuiltFile's doc
	// comment on why a --help-style check is unsafe for RIE specifically).
	runner.mu.Lock()
	calls := append([]call(nil), runner.calls...)
	runner.mu.Unlock()

	if len(calls) != 4 {
		t.Fatalf("runner.calls = %d calls, want exactly 4: %+v", len(calls), calls)
	}

	if calls[0].name != "git" || len(calls[0].args) == 0 || calls[0].args[0] != "--version" {
		t.Errorf("calls[0] = %+v, want git --version", calls[0])
	}

	if calls[1].name != "go" || len(calls[1].args) == 0 || calls[1].args[0] != "version" {
		t.Errorf("calls[1] = %+v, want go version", calls[1])
	}

	clone := calls[2]
	if clone.name != "git" {
		t.Errorf("calls[2].name = %q, want git", clone.name)
	}

	wantCloneArgs := []string{"clone", "--depth", "1", "--branch", Version, repoURL}
	if len(clone.args) < len(wantCloneArgs) {
		t.Fatalf("calls[2].args = %v, want prefix %v", clone.args, wantCloneArgs)
	}

	for i, want := range wantCloneArgs {
		if clone.args[i] != want {
			t.Errorf("calls[2].args[%d] = %q, want %q", i, clone.args[i], want)
		}
	}

	build := calls[3]
	if build.name != "sh" || len(build.args) < 2 || build.args[0] != "-c" {
		t.Fatalf("calls[3] = %+v, want sh -c <build command>", build)
	}

	if !strings.Contains(build.args[1], "GOTOOLCHAIN=auto") {
		t.Errorf("build command %q missing GOTOOLCHAIN=auto", build.args[1])
	}

	if !strings.Contains(build.args[1], "go build -o") || !strings.Contains(build.args[1], "./cmd/aws-lambda-rie") {
		t.Errorf("build command %q missing expected go build invocation", build.args[1])
	}
}

func TestResolveBuildToolsMissingGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv(envHome, home)
	t.Setenv(envRIEPath, "")

	runner := &fakeRunner{
		respond: func(name string, args []string) ([]byte, error) {
			if name == "git" {
				return nil, fmt.Errorf("exec: git: %w", exec.ErrNotFound)
			}

			t.Fatalf("unexpected runner call: %s %s", name, strings.Join(args, " "))

			return nil, nil
		},
	}

	_, err := Resolve(context.Background(), runner)
	if err == nil {
		t.Fatal("Resolve() error = nil, want ErrBuildToolsMissing")
	}

	if !errors.Is(err, ErrBuildToolsMissing) {
		t.Errorf("Resolve() error = %v, want errors.Is ErrBuildToolsMissing", err)
	}

	if !strings.Contains(err.Error(), "git") {
		t.Errorf("Resolve() error = %q, want it to mention git", err)
	}
}

func TestResolveBuildToolsMissingGo(t *testing.T) {
	home := t.TempDir()
	t.Setenv(envHome, home)
	t.Setenv(envRIEPath, "")

	runner := &fakeRunner{
		respond: func(name string, args []string) ([]byte, error) {
			switch name {
			case "git":
				return []byte("git version 2.43.0"), nil
			case "go":
				return nil, fmt.Errorf("exec: go: %w", exec.ErrNotFound)
			default:
				t.Fatalf("unexpected runner call: %s %s", name, strings.Join(args, " "))
				return nil, nil
			}
		},
	}

	_, err := Resolve(context.Background(), runner)
	if err == nil {
		t.Fatal("Resolve() error = nil, want ErrBuildToolsMissing")
	}

	if !errors.Is(err, ErrBuildToolsMissing) {
		t.Errorf("Resolve() error = %v, want errors.Is ErrBuildToolsMissing", err)
	}

	if !strings.Contains(err.Error(), "go") {
		t.Errorf("Resolve() error = %q, want it to mention go", err)
	}
}

// TestResolveIntegration exercises the real acquisition chain end to end:
// a real shallow clone + `go build` of the pinned upstream tag into a temp
// $LAMBDARY_HOME, then a second Resolve call that must hit the cache
// instead of rebuilding. Skipped on -short or when git/go aren't on PATH,
// per plans/m3-process-path.md — this host has both plus network, so it's
// kept rather than only unit-tested.
func TestResolveIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped: -short")
	}

	var runner backend.ExecRunner

	if _, err := runner.Run(context.Background(), "git", "--version"); err != nil {
		t.Skipf("integration test skipped: git unavailable: %v", err)
	}

	if _, err := runner.Run(context.Background(), "go", "version"); err != nil {
		t.Skipf("integration test skipped: go unavailable: %v", err)
	}

	home := t.TempDir()
	t.Setenv(envHome, home)
	t.Setenv(envRIEPath, "")

	// The first build clones over the network and runs `go build`
	// (potentially fetching a pinned toolchain via GOTOOLCHAIN=auto, per
	// plans/rie-darwin-spike.md) — ~30s observed on this host, generous
	// headroom here for a slower network/machine.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	start := time.Now()

	path, err := Resolve(ctx, runner)
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}

	buildElapsed := time.Since(start)
	t.Logf("first Resolve() (build from source): %s", buildElapsed)

	wantCache := filepath.Join(home, "bin", fmt.Sprintf("aws-lambda-rie-%s-%s-%s", Version, runtime.GOOS, runtime.GOARCH))
	if path != wantCache {
		t.Errorf("Resolve() = %q, want %q", path, wantCache)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat(%s) unexpected error: %v", path, err)
	}

	if info.Mode()&0o111 == 0 {
		t.Errorf("built binary mode = %v, want executable", info.Mode())
	}

	if info.Size() == 0 {
		t.Errorf("built binary %s is empty", path)
	}

	firstModTime := info.ModTime()

	// Deliberately not run here: RIE has no --help/-h flag, so invoking it
	// with one boots its full sandbox instead of printing usage — either
	// hanging forever (internal Runtime API port free) or panicking on a
	// bind error (port taken), per plans/rie-darwin-spike.md's port-9001
	// note. A real, bounded run-through-invoke check belongs to the process
	// backend's own readiness check (Unit B/C), not here.

	// Second Resolve must hit the cache, not rebuild: assert the cached
	// file's mtime is unchanged rather than timing the call, since timing
	// thresholds are inherently flaky across CI/dev hardware.
	start = time.Now()

	path2, err := Resolve(ctx, runner)
	if err != nil {
		t.Fatalf("second Resolve() unexpected error: %v", err)
	}

	cacheElapsed := time.Since(start)
	t.Logf("second Resolve() (cache hit): %s", cacheElapsed)

	if path2 != path {
		t.Errorf("second Resolve() = %q, want %q (same cached path)", path2, path)
	}

	info2, err := os.Stat(path2)
	if err != nil {
		t.Fatalf("os.Stat(%s) unexpected error: %v", path2, err)
	}

	if !info2.ModTime().Equal(firstModTime) {
		t.Errorf("cached binary mtime changed from %v to %v — Resolve rebuilt instead of hitting the cache", firstModTime, info2.ModTime())
	}
}
