// Package backend defines the execution-backend contract Lambdary's
// container and (later) process backends implement, plus the shared
// building blocks — an exec.Cmd-abstracting Runner, container CLI
// detection, and AWS runtime→image mapping — described in DESIGN.md's
// "Execution backends" section.
package backend

import (
	"context"
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
