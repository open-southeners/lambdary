// Package container implements backend.Backend on top of a container CLI
// (docker/podman/finch/nerdctl), per DESIGN.md's "Container backend"
// section and plans/m1-container-path.md's Unit B. Every function gets its
// own container built from an AWS Lambda base image, with its code
// bind-mounted read-only at /var/task; the base image already runs the
// Runtime Interface Emulator as its entrypoint, so Start only needs to
// launch the container and wait for its invoke port to answer.
package container

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
)

// defaultReadyTimeout is how long Start waits for a freshly started
// container's invoke port to answer before giving up, per
// plans/m1-container-path.md Unit B ("~30s, covers image pull on the run
// itself"). A zero containerBackend.readyTimeout falls back to this.
const defaultReadyTimeout = 30 * time.Second

// ErrDockerfileNotSupported is returned by Start for Dockerfile-marker
// functions: discovery.Function values with no Runtime, no Image override,
// and Backend "container" (see discovery.Function's doc comment — that
// combination only arises from a bare Dockerfile marker with no manifest
// override). Building the Dockerfile is deferred past M1 to keep the
// container path scoped to AWS's own base images — see
// plans/m1-container-path.md's Deferred section and CURRENT_ISSUES.md.
var ErrDockerfileNotSupported = errors.New("Dockerfile-based functions are not yet supported")

// containerBackend is the backend.Backend implementation returned by New.
// readyTimeout is unexported and defaults to defaultReadyTimeout when zero;
// tests in this package construct containerBackend directly to override it
// with a short deadline instead of exercising the real 30s wait.
type containerBackend struct {
	cli          string
	runner       backend.Runner
	readyTimeout time.Duration
}

// New returns a backend.Backend that runs functions as containers via cli
// (e.g. "docker", "podman" — anything backend.DetectContainerCLI resolved),
// issuing every command through runner so callers can substitute a fake in
// tests.
func New(cli string, runner backend.Runner) backend.Backend {
	return &containerBackend{cli: cli, runner: runner}
}

// Start resolves fn's image, launches it with the argv runArgs builds,
// resolves the ephemeral host port docker published :8080 on, and blocks
// until that port answers before returning — per backend.Backend.Start's
// contract that the returned Instance is immediately ready to invoke. On
// any failure after the container starts, Start stops it before returning
// so a failed Start never leaks a running container.
func (b *containerBackend) Start(ctx context.Context, fn discovery.Function) (backend.Instance, error) {
	image, err := resolveImage(fn)
	if err != nil {
		return nil, fmt.Errorf("container: %s: %w", fn.Name, err)
	}

	absDir, err := filepath.Abs(fn.Dir)
	if err != nil {
		return nil, fmt.Errorf("container: %s: resolving function directory: %w", fn.Name, err)
	}

	args := runArgs(fn, image, absDir)

	out, err := b.runner.Run(ctx, b.cli, args...)
	if err != nil {
		return nil, fmt.Errorf("container: %s: start: %w", fn.Name, err)
	}

	id := lastLine(string(out))
	if id == "" {
		return nil, fmt.Errorf("container: %s: start: no container ID in output %q", fn.Name, out)
	}

	port, err := resolvePort(ctx, b.runner, b.cli, id)
	if err != nil {
		stopContainer(ctx, b.runner, b.cli, id)

		return nil, fmt.Errorf("container: %s: %w", fn.Name, err)
	}

	timeout := b.readyTimeout
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}

	if err := waitReady(ctx, port, timeout); err != nil {
		tail := logsTail(ctx, b.runner, b.cli, id)
		stopContainer(ctx, b.runner, b.cli, id)

		return nil, fmt.Errorf("container: %s: %w%s", fn.Name, err, tail)
	}

	return &instance{cli: b.cli, runner: b.runner, id: id, port: port}, nil
}

// resolveImage picks the image Start runs fn from: the manifest's
// local.image override (fn.Image) when set, otherwise
// backend.ImageFor(fn.Runtime). Dockerfile-marker functions — no runtime,
// no image override, backend resolved to "container" by discovery purely
// from the marker — have no AWS base image to fall back to, so they error
// with ErrDockerfileNotSupported instead of reaching ImageFor("").
func resolveImage(fn discovery.Function) (string, error) {
	if fn.Runtime == "" && fn.Image == "" && fn.Backend == "container" {
		return "", ErrDockerfileNotSupported
	}

	if fn.Image != "" {
		return fn.Image, nil
	}

	return backend.ImageFor(fn.Runtime)
}

// lastLine returns the last non-blank line of s, trimmed. `docker run -d`
// normally prints just the container ID, but some CLI configurations (e.g.
// a build warning banner) can print extra lines first, so the ID — always
// the final line — is what callers should trust.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}

	return strings.TrimSpace(lines[len(lines)-1])
}
