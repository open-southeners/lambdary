package backend

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runner abstracts process execution (os/exec) behind an interface so
// backends and probes can be unit-tested without invoking a real container
// CLI. Per plans/m1-container-path.md's conventions, every exec call in
// this codebase goes through a Runner.
type Runner interface {
	// Run executes name with args, waits for it to complete, and returns
	// its combined stdout+stderr output. Used for short-lived commands —
	// `docker info`, `docker run -d`, `docker port`, `docker stop`.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)

	// Start launches name with args and returns immediately with a handle
	// to its combined stdout+stderr stream, for long-lived commands —
	// `docker logs -f`. The returned Cmd's Wait must eventually be called
	// to release the underlying process's resources.
	Start(ctx context.Context, name string, args ...string) (Cmd, error)
}

// Cmd is a handle to a process started by Runner.Start.
type Cmd interface {
	// Output streams the command's combined stdout+stderr. It reaches EOF
	// once the command exits.
	Output() io.Reader
	// Wait blocks until the command exits and returns its result. It is
	// safe to call at most once.
	Wait() error
}

// ExecRunner is the real Runner, backed by os/exec.
type ExecRunner struct{}

// Run implements Runner using exec.CommandContext and CombinedOutput.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("exec: %s: %w", commandString(name, args), err)
	}

	return out, nil
}

// Start implements Runner using exec.CommandContext, piping the child's
// combined stdout+stderr to the returned Cmd's Output.
func (ExecRunner) Start(ctx context.Context, name string, args ...string) (Cmd, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec: %s: %w", commandString(name, args), err)
	}

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.CloseWithError(err) //nolint:errcheck // CloseWithError(nil) closes cleanly; the error, if any, is also returned via done.
		done <- err
	}()

	return &execCmd{out: pr, done: done}, nil
}

// execCmd is the real Cmd, backed by a running exec.Cmd whose output is
// piped through an io.Pipe so it can be read while the process is still
// running.
type execCmd struct {
	out  io.Reader
	done chan error
}

func (c *execCmd) Output() io.Reader { return c.out }

func (c *execCmd) Wait() error { return <-c.done }

// commandString renders name and args for error messages, matching how
// they'd be typed on a shell (no quoting — output is diagnostic, not
// re-executable).
func commandString(name string, args []string) string {
	if len(args) == 0 {
		return name
	}

	return name + " " + strings.Join(args, " ")
}
