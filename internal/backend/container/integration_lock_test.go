package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/lockfile"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// recordingRunner wraps a real backend.ExecRunner, recording every Run
// call's argv while still executing it for real — the real-docker
// equivalent of fakeRunner's respond map, needed because
// TestContainerBackendIntegrationLockPinning has to both run real `docker`
// commands and assert the exact argv Start built (specifically, whether it
// used a plain tag or a "<repo>@sha256:..." digest ref).
type recordingRunner struct {
	backend.ExecRunner

	mu    sync.Mutex
	calls [][]string
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()

	return r.ExecRunner.Run(ctx, name, args...)
}

// callsFor returns the argv (including the subcommand) of every recorded
// call whose first arg matches subcommand, in invocation order — mirroring
// fakeRunner.callsFor for the unit tests.
func (r *recordingRunner) callsFor(subcommand string) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out [][]string
	for _, c := range r.calls {
		if len(c) > 1 && c[1] == subcommand {
			out = append(out, c[1:])
		}
	}

	return out
}

// TestContainerBackendIntegrationLockPinning exercises `.lambdary/lock`
// pinning end to end against the real docker CLI, per plans/m5-extras.md's
// Unit C: a first Start of testdata/python-hello (an unpinned tag) records
// a real sha256 digest into the lock file; a second Start, from a freshly
// loaded Lock (simulating a new `lambdary` process reading the committed
// lock), runs the container by "<repo>@sha256:..." instead of the tag.
// Gated identically to TestContainerBackendIntegration.
func TestContainerBackendIntegrationLockPinning(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped: -short")
	}

	var probe backend.ExecRunner

	if _, err := probe.Run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("integration test skipped: docker daemon unreachable: %v", err)
	}

	fnDir, err := filepath.Abs("testdata/python-hello")
	if err != nil {
		t.Fatalf("filepath.Abs() unexpected error: %v", err)
	}

	// configRoot stands in for the directory containing lambdary.yml — the
	// lock lives at configRoot/.lambdary/lock, per internal/lockfile.Load.
	configRoot := t.TempDir()

	const wantTag = "public.ecr.aws/lambda/python:3.13"

	fn := discovery.Function{
		Name:    "lambdary-container-it-lock-pin",
		Dir:     fnDir,
		Runtime: "python3.13",
		Handler: "lambda_function.handler",
		Backend: "container",
		Manifest: &manifest.Manifest{
			Timeout: 30,
		},
	}

	// --- First Start: unpinned tag, records a digest on success. ---

	lock, err := lockfile.Load(configRoot)
	if err != nil {
		t.Fatalf("lockfile.Load() unexpected error: %v", err)
	}

	runner1 := &recordingRunner{}
	b1 := &containerBackend{cli: "docker", runner: runner1, readyTimeout: 120 * time.Second, lock: lock}

	ctx1, cancel1 := context.WithTimeout(context.Background(), 150*time.Second)
	inst1, err := b1.Start(ctx1, fn)
	cancel1()
	if err != nil {
		t.Fatalf("first Start() unexpected error: %v", err)
	}

	firstRunCalls := runner1.callsFor("run")
	if len(firstRunCalls) != 1 {
		t.Fatalf("first Start(): run calls = %d, want 1", len(firstRunCalls))
	}
	if argv := firstRunCalls[0]; !containsArg(argv, wantTag) {
		t.Errorf("first Start(): run argv = %v, want it to use the plain unpinned tag %q", argv, wantTag)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = inst1.Stop(stopCtx)
	cancel()
	if err != nil {
		t.Fatalf("first Stop() unexpected error: %v", err)
	}

	waitForNoLabeledContainers(t, probe, fn.Name)

	digest, ok := lock.ImageDigest(wantTag)
	if !ok {
		t.Fatalf("lock has no digest recorded for %q after first Start()", wantTag)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("recorded digest = %q, want a sha256:... value", digest)
	}

	raw, err := os.ReadFile(filepath.Join(configRoot, ".lambdary", "lock"))
	if err != nil {
		t.Fatalf("reading .lambdary/lock: %v", err)
	}
	if got := string(raw); !strings.Contains(got, wantTag+": "+digest) {
		t.Errorf("lock file content = %q, want it to contain %q", got, wantTag+": "+digest)
	}

	// --- Second Start: a fresh process re-loading the same lock from disk
	// must run by digest, not by tag. ---

	reloaded, err := lockfile.Load(configRoot)
	if err != nil {
		t.Fatalf("lockfile.Load() (reload) unexpected error: %v", err)
	}

	runner2 := &recordingRunner{}
	b2 := &containerBackend{cli: "docker", runner: runner2, readyTimeout: 120 * time.Second, lock: reloaded}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Second)
	inst2, err := b2.Start(ctx2, fn)
	cancel2()
	if err != nil {
		t.Fatalf("second Start() unexpected error: %v", err)
	}

	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		inst2.Stop(stopCtx) //nolint:errcheck // best-effort cleanup
	})

	secondRunCalls := runner2.callsFor("run")
	if len(secondRunCalls) != 1 {
		t.Fatalf("second Start(): run calls = %d, want 1", len(secondRunCalls))
	}

	wantDigestRef := "public.ecr.aws/lambda/python@" + digest
	if argv := secondRunCalls[0]; !containsArg(argv, wantDigestRef) {
		t.Errorf("second Start(): run argv = %v, want it to use the digest ref %q", argv, wantDigestRef)
	}

	if calls := runner2.callsFor("inspect"); len(calls) != 0 {
		t.Errorf("second Start(): inspect calls = %d, want 0 (already pinned, nothing to resolve)", len(calls))
	}

	stopCtx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	err = inst2.Stop(stopCtx)
	cancel()
	if err != nil {
		t.Fatalf("second Stop() unexpected error: %v", err)
	}

	waitForNoLabeledContainers(t, probe, fn.Name)
}

// containsArg reports whether want appears anywhere in argv.
func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}

	return false
}
