package process

import (
	"context"
	"fmt"
	"io"
)

// instance is the backend.Instance a processBackend.Start returns: one
// spawned RIE process, with its Invoke API listening on port.
type instance struct {
	proc *rieProcess
	port int
}

// InvokeURL implements backend.Instance, per DESIGN.md's Invoke API
// endpoint shape.
func (i *instance) InvokeURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/2015-03-31/functions/function/invocations", i.port)
}

// Stop implements backend.Instance by terminating the RIE's process group
// (see spawn.go's rieProcess.stop). Repeat calls are a no-op, per
// backend.Instance.Stop's contract; ctx is unused since signalling a local
// process group is not itself a blocking or cancellable operation — only
// the grace-period wait before escalating to SIGKILL takes meaningful time,
// and that grace period is fixed (stopGrace), not caller-tunable.
func (i *instance) Stop(_ context.Context) error {
	if err := i.proc.stop(stopGrace); err != nil {
		return fmt.Errorf("process: stop: %w", err)
	}

	return nil
}

// Logs implements backend.Instance by streaming the RIE's combined
// stdout+stderr (which, per the spike report, already carries the runtime
// process's own output — the RIE does not redirect its child's streams
// elsewhere). Reads block for more output until the process stops, at
// which point the stream reaches EOF, per backend.Instance.Logs' contract.
func (i *instance) Logs() io.Reader {
	return i.proc.out.NewReader()
}
