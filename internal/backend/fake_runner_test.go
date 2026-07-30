package backend

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
)

// fakeRunner is a Runner test double keyed by command name. It lets probe
// and other backend-level tests script per-CLI responses without
// invoking a real container CLI.
type fakeRunner struct {
	// run maps a command name (e.g. "docker") to the outcome its Run call
	// should produce. Names absent from run behave as if the binary isn't
	// on PATH: Run returns an *exec.Error wrapping exec.ErrNotFound, same
	// as the real ExecRunner would.
	run map[string]fakeResult

	// calls records every command name Run was invoked with, in order,
	// so tests can assert probe order.
	calls []string
}

// fakeResult scripts one fakeRunner.Run response.
type fakeResult struct {
	output []byte
	err    error
}

func (f *fakeRunner) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	f.calls = append(f.calls, name)

	res, ok := f.run[name]
	if !ok {
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	}

	return res.output, res.err
}

func (f *fakeRunner) Start(_ context.Context, name string, args ...string) (Cmd, error) {
	f.calls = append(f.calls, name)

	res, ok := f.run[name]
	if !ok {
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	}

	if res.err != nil {
		return nil, res.err
	}

	return &fakeCmd{r: strings.NewReader(string(res.output))}, nil
}

// fakeCmd is a Cmd test double backed by a fixed byte slice.
type fakeCmd struct {
	r io.Reader
}

func (c *fakeCmd) Output() io.Reader { return c.r }

func (c *fakeCmd) Wait() error { return nil }

// errDaemonDown is a stand-in for the non-zero-exit error a real CLI
// produces when its daemon is unreachable (an *exec.ExitError in
// practice) — any non-ErrNotFound error exercises the same code path.
var errDaemonDown = errors.New("exit status 1")
