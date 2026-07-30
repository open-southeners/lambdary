package container

import (
	"context"
	"io"
	"strings"
	"sync"

	"github.com/open-southeners/lambdary/internal/backend"
)

// call records one Runner.Run or Runner.Start invocation, for tests to
// assert both the exact argv built and the sequence of commands issued
// (e.g. that a readiness failure stops the container).
type call struct {
	name string
	args []string
}

// fakeRunner is a small backend.Runner test double local to this package —
// per plans/m1-container-path.md Unit B's note that the backend package's
// own fake is test-local, container writes its own. respond, when set,
// dispatches by the invoked subcommand (args[0], e.g. "run"/"port"/
// "stop"/"logs"); commands with no matching entry return ("", nil).
type fakeRunner struct {
	mu    sync.Mutex
	calls []call

	respond map[string]func(args []string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.record(name, args)

	if len(args) > 0 && f.respond != nil {
		if fn, ok := f.respond[args[0]]; ok {
			out, err := fn(args)
			return []byte(out), err
		}
	}

	return nil, nil
}

func (f *fakeRunner) Start(_ context.Context, name string, args ...string) (backend.Cmd, error) {
	f.record(name, args)

	var out string
	if len(args) > 0 && f.respond != nil {
		if fn, ok := f.respond[args[0]]; ok {
			var err error
			out, err = fn(args)
			if err != nil {
				return nil, err
			}
		}
	}

	return &fakeCmd{r: strings.NewReader(out)}, nil
}

func (f *fakeRunner) record(name string, args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...)})
}

// callsFor returns the argv (including the subcommand) of every call whose
// first arg matches subcommand, in invocation order.
func (f *fakeRunner) callsFor(subcommand string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out [][]string
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == subcommand {
			out = append(out, c.args)
		}
	}

	return out
}

// fakeCmd is a backend.Cmd test double backed by a fixed string.
type fakeCmd struct {
	r io.Reader
}

func (c *fakeCmd) Output() io.Reader { return c.r }

func (c *fakeCmd) Wait() error { return nil }
