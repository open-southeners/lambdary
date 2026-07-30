package backend

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// requireBin skips the test if name isn't resolvable on PATH, so the smoke
// tests degrade gracefully on hosts missing one of these near-universal
// Unix tools instead of failing outright.
func requireBin(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available on PATH: %v", name, err)
	}

	return path
}

func TestExecRunnerRun(t *testing.T) {
	t.Run("true exits 0 with no error", func(t *testing.T) {
		requireBin(t, "true")

		var r ExecRunner
		if _, err := r.Run(context.Background(), "true"); err != nil {
			t.Errorf("Run(true) unexpected error: %v", err)
		}
	})

	t.Run("echo returns combined output", func(t *testing.T) {
		echo := requireBin(t, "echo")

		var r ExecRunner
		out, err := r.Run(context.Background(), echo, "hello")
		if err != nil {
			t.Fatalf("Run(echo) unexpected error: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "hello" {
			t.Errorf("Run(echo hello) output = %q, want %q", got, "hello")
		}
	})

	t.Run("false exits non-zero with an error", func(t *testing.T) {
		requireBin(t, "false")

		var r ExecRunner
		if _, err := r.Run(context.Background(), "false"); err == nil {
			t.Error("Run(false) expected error, got nil")
		}
	})

	t.Run("missing binary wraps exec.ErrNotFound", func(t *testing.T) {
		var r ExecRunner
		_, err := r.Run(context.Background(), "lambdary-definitely-not-a-real-binary")
		if err == nil {
			t.Fatal("Run() expected error, got nil")
		}
		if !errors.Is(err, exec.ErrNotFound) {
			t.Errorf("Run() error = %v, want wrapping exec.ErrNotFound", err)
		}
	})
}

func TestExecRunnerStart(t *testing.T) {
	echo := requireBin(t, "echo")

	var r ExecRunner
	cmd, err := r.Start(context.Background(), echo, "streamed")
	if err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}

	out, err := io.ReadAll(cmd.Output())
	if err != nil {
		t.Fatalf("reading Output() unexpected error: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "streamed" {
		t.Errorf("Start(echo streamed) output = %q, want %q", got, "streamed")
	}

	if err := cmd.Wait(); err != nil {
		t.Errorf("Wait() unexpected error: %v", err)
	}
}
