// Package backend defines the execution-backend contract Lambdary's
// container and (later) process backends implement, plus the shared
// building blocks — an exec.Cmd-abstracting Runner, container CLI
// detection, and AWS runtime→image mapping — described in DESIGN.md's
// "Execution backends" section.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/open-southeners/lambdary/internal/discovery"
)

// Backend starts and supervises instances of a discovered function on one
// execution technology (container, and later process — see DESIGN.md's
// "backends" component table). Implementations live in subpackages, e.g.
// internal/backend/container.
type Backend interface {
	// Start brings up one running instance of fn and returns a handle to
	// it. The returned Instance is ready to receive invocations by the
	// time Start returns without error: callers do not need to wait for
	// readiness themselves.
	Start(ctx context.Context, fn discovery.Function) (Instance, error)
}

// ErrStartFailed is what errors.Is matches on any error Backend.Start
// returns because the function's own instance never came up — its container
// exited at once, or never answered on its invoke port — as opposed to a
// failure of Lambdary or the container engine itself (pulling an image,
// building, a missing bootstrap). Callers use it to tell "the function
// failed to start" apart from an error the function returned while running.
var ErrStartFailed = errors.New("function failed to start")

// StartFailedError is the concrete error behind ErrStartFailed: errors.Is
// reports true for ErrStartFailed, and errors.As recovers the function name
// and (when there is one) the underlying cause.
type StartFailedError struct {
	// Function is the function's name.
	Function string
	// Detail says what happened, e.g. "container exited with code 255:
	// exec /var/runtime/bootstrap: exec format error".
	Detail string
	// Cause is the underlying error, when the failure had one (nil for an
	// observed container exit).
	Cause error
}

// Error renders "function <name>: <detail>".
func (e *StartFailedError) Error() string {
	return fmt.Sprintf("function %s: %s", e.Function, e.Detail)
}

// Is reports whether target is ErrStartFailed.
func (e *StartFailedError) Is(target error) bool { return target == ErrStartFailed }

// Unwrap returns the underlying cause, if any.
func (e *StartFailedError) Unwrap() error { return e.Cause }

// BootstrapBuilder is implemented by a Backend that can compile a Go
// function (fn.Go set) into the bootstrap binary that backend would run,
// writing it to out. Publishing a version uses it to freeze the binary.
type BootstrapBuilder interface {
	BuildBootstrap(ctx context.Context, fn discovery.Function, out string) error
}

// Instance is one running copy of a function, backed by whatever the
// owning Backend started (a container, a subprocess, …). Per DESIGN.md's
// "The RIE processes one invocation at a time per instance" constraint,
// callers are expected to serialize invocations against a single Instance
// themselves (see internal/router's Manager) rather than Instance doing it.
type Instance interface {
	// InvokeURL is the AWS Invoke API endpoint for this instance —
	// `POST {InvokeURL()}` with a raw JSON event body, per DESIGN.md's
	// "Invoke API" — e.g.
	// "http://127.0.0.1:54321/2015-03-31/functions/function/invocations".
	InvokeURL() string
	// Stop tears the instance down (stop the container/process, release
	// its port). It is safe to call once; implementations should make
	// repeat calls a no-op rather than error.
	Stop(ctx context.Context) error
	// Logs streams the instance's combined stdout+stderr. Reads block for
	// more output until the instance stops, at which point the stream
	// reaches EOF.
	Logs() io.Reader
}
