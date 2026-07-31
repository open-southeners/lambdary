package process

import (
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireBin skips the test if name isn't resolvable on PATH, matching
// internal/backend/exec_test.go's requireBin — this package needs its own
// copy since that one is unexported.
func requireBin(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available on PATH: %v", name, err)
	}

	return path
}

// TestSpawn drives spawn directly against real, near-universal Unix tools
// (sh, printenv) rather than a fake RIE binary or the real one — cheap and
// dependency-free, per plans/m3-process-path.md Unit B's note that a full
// RIE integration test is Unit C's e2e territory. It exists to prove the
// os/exec-based plumbing process.go's package doc describes (cwd, env,
// combined output capture, process-group stop) actually works, independent
// of anything RIE- or shim-specific.
func TestSpawn(t *testing.T) {
	sh := requireBin(t, "sh")

	t.Run("cwd is threaded through", func(t *testing.T) {
		dir := t.TempDir()

		p, err := spawn(sh, []string{"-c", "pwd -P"}, dir, nil)
		if err != nil {
			t.Fatalf("spawn() unexpected error: %v", err)
		}
		<-p.done

		out, err := io.ReadAll(p.out.NewReader())
		if err != nil {
			t.Fatalf("reading output unexpected error: %v", err)
		}

		got := strings.TrimSpace(string(out))
		want := resolveSymlinks(t, dir)
		if got != want {
			t.Errorf("pwd output = %q, want %q", got, want)
		}
	})

	t.Run("env is threaded through", func(t *testing.T) {
		p, err := spawn(sh, []string{"-c", "echo \"$LAMBDARY_TEST_VAR\""}, t.TempDir(), []string{"LAMBDARY_TEST_VAR=hello-process-backend"})
		if err != nil {
			t.Fatalf("spawn() unexpected error: %v", err)
		}
		<-p.done

		out, err := io.ReadAll(p.out.NewReader())
		if err != nil {
			t.Fatalf("reading output unexpected error: %v", err)
		}

		if got := strings.TrimSpace(string(out)); got != "hello-process-backend" {
			t.Errorf("env output = %q, want %q", got, "hello-process-backend")
		}
	})

	t.Run("stop terminates the process group, including a child process", func(t *testing.T) {
		// The inner `sleep 30 &` forks a grandchild in the same process
		// group; `wait` keeps the shell itself alive so stop has something
		// to signal. If stop only killed the immediate child (the shell)
		// and not the whole group, the sleep would keep running.
		p, err := spawn(sh, []string{"-c", "sleep 30 & wait"}, t.TempDir(), nil)
		if err != nil {
			t.Fatalf("spawn() unexpected error: %v", err)
		}

		start := time.Now()
		if err := p.stop(2 * time.Second); err != nil {
			t.Fatalf("stop() unexpected error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("stop() took %s, want SIGTERM to end the group promptly", elapsed)
		}

		select {
		case <-p.done:
		default:
			t.Error("stop() returned but the process's Wait goroutine hasn't finished")
		}
	})

	t.Run("stop is idempotent", func(t *testing.T) {
		p, err := spawn(sh, []string{"-c", "true"}, t.TempDir(), nil)
		if err != nil {
			t.Fatalf("spawn() unexpected error: %v", err)
		}
		<-p.done

		if err := p.stop(time.Second); err != nil {
			t.Errorf("first stop() unexpected error: %v", err)
		}
		if err := p.stop(time.Second); err != nil {
			t.Errorf("second stop() unexpected error: %v", err)
		}
	})

	t.Run("stop escalates to SIGKILL for a process that ignores SIGTERM", func(t *testing.T) {
		// trap '' TERM ignores SIGTERM; stop must fall back to SIGKILL
		// after its grace period rather than hanging forever.
		p, err := spawn(sh, []string{"-c", "trap '' TERM; sleep 30"}, t.TempDir(), nil)
		if err != nil {
			t.Fatalf("spawn() unexpected error: %v", err)
		}

		// Give the shell a moment to actually install the trap before
		// signalling — otherwise stop can race a SIGTERM in before "trap"
		// has run, which would kill the (still default-dispositioned)
		// process immediately and defeat the point of this test.
		time.Sleep(200 * time.Millisecond)

		start := time.Now()
		if err := p.stop(200 * time.Millisecond); err != nil {
			t.Fatalf("stop() unexpected error: %v", err)
		}
		elapsed := time.Since(start)

		if elapsed < 200*time.Millisecond {
			t.Errorf("stop() took %s, want it to wait out the grace period before escalating", elapsed)
		}
		if elapsed > 5*time.Second {
			t.Errorf("stop() took %s, want SIGKILL to end a SIGTERM-ignoring process promptly after grace", elapsed)
		}
	})
}

// resolveSymlinks resolves path (e.g. t.TempDir()'s result, which on macOS
// sits under a /var/folders symlink to /private/var/folders) to what a
// child process's `pwd -P` would report after actually chdir-ing there.
func resolveSymlinks(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks(%q) unexpected error: %v", path, err)
	}

	return resolved
}
