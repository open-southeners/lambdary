package container

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/open-southeners/lambdary/internal/backend"
)

// logsTailLines is how many trailing log lines logsTail fetches to attach
// to a readiness-timeout error, per plans/m1-container-path.md Unit B
// ("include recent logs tail if cheaply available").
const logsTailLines = "20"

// instance is the backend.Instance a containerBackend.Start returns: one
// running container, identified by its docker ID, with its :8080 published
// on port.
type instance struct {
	cli    string
	runner backend.Runner
	id     string
	port   string

	mu      sync.Mutex
	stopped bool
}

// InvokeURL implements backend.Instance, per DESIGN.md's Invoke API
// endpoint shape.
func (i *instance) InvokeURL() string {
	return fmt.Sprintf("http://127.0.0.1:%s/2015-03-31/functions/function/invocations", i.port)
}

// Stop implements backend.Instance by running `<cli> stop <id>`. The
// container was started with --rm, so a successful stop also removes it.
// Repeat calls are a no-op, per backend.Instance.Stop's contract; a stop
// racing the container already having exited or been removed is treated as
// success rather than surfaced as an error, since the end state — no
// running container — is what the caller wanted either way.
func (i *instance) Stop(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.stopped {
		return nil
	}
	i.stopped = true

	if err := stopContainer(ctx, i.runner, i.cli, i.id); err != nil {
		return fmt.Errorf("container: stop %s: %w", i.id, err)
	}

	return nil
}

// Logs implements backend.Instance by streaming `<cli> logs -f <id>`,
// combined stdout+stderr. backend.Instance.Logs takes no context, so Logs
// uses context.Background(): the stream ends on its own once the container
// exits or is removed (Stop's --rm cleanup). If the logs command itself
// can't be started, the returned io.Reader yields that error on its first
// Read instead of Logs returning one, since the interface has no error
// return.
func (i *instance) Logs() io.Reader {
	cmd, err := i.runner.Start(context.Background(), i.cli, "logs", "-f", i.id)
	if err != nil {
		return &errReader{err: fmt.Errorf("container: logs %s: %w", i.id, err)}
	}

	return cmd.Output()
}

// stopContainer runs `<cli> stop <id>`, treating "no such container" as
// success — the container is already gone, which is the state Stop wants.
// Shared between Start's cleanup-on-failure paths (which have no instance
// yet) and instance.Stop.
func stopContainer(ctx context.Context, runner backend.Runner, cli, id string) error {
	out, err := runner.Run(ctx, cli, "stop", id)
	if err != nil && !strings.Contains(strings.ToLower(string(out)), "no such container") {
		return err
	}

	return nil
}

// logsTail fetches the container's last logsTailLines lines for inclusion
// in a readiness-timeout error, best-effort: any failure fetching them
// yields an empty string rather than masking the original error.
func logsTail(ctx context.Context, runner backend.Runner, cli, id string) string {
	out, err := runner.Run(ctx, cli, "logs", "--tail", logsTailLines, id)
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return ""
	}

	return "\nrecent logs:\n" + string(out)
}

// errReader is an io.Reader that always fails with err, used by Logs when
// starting the log stream itself errors.
type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }
