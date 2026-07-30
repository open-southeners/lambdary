package backend

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// containerCLIs lists the container CLIs DetectContainerCLI probes, in
// priority order, per DESIGN.md's "Selection (backend: auto)" step 1.
var containerCLIs = []string{"docker", "podman", "finch", "nerdctl"}

// Errors returned by DetectContainerCLI. They are wrapped with %w so
// callers can match on them with errors.Is regardless of the remedy text.
var (
	// ErrNoContainerCLI indicates none of containerCLIs has a binary on
	// PATH at all.
	ErrNoContainerCLI = errors.New("no container CLI found")
	// ErrDaemonUnreachable indicates at least one container CLI binary
	// exists, but none of their daemons answered.
	ErrDaemonUnreachable = errors.New("container CLI found but daemon unreachable")
)

// DetectContainerCLI probes containerCLIs in order and returns the name of
// the first one whose daemon actually answers. A CLI only "counts" when
// `<cli> info` exits 0 — checked instead of `<cli> version`, since a
// stopped daemon still makes `docker version` print client-only info and
// exit non-zero, which `info` treats consistently as unreachable. Every
// candidate is tried (not just the first binary found) so, for example, a
// stopped Docker Desktop with Podman also installed and running still
// resolves to podman.
//
// The returned error distinguishes two remedies: no CLI binary anywhere
// (ErrNoContainerCLI, install one) versus a CLI present but its daemon down
// (ErrDaemonUnreachable, start it) — see DESIGN.md's "Selection" step 3
// ("fail with a message that names both remedies").
func DetectContainerCLI(ctx context.Context, runner Runner) (string, error) {
	var found []string

	for _, cli := range containerCLIs {
		_, err := runner.Run(ctx, cli, "info")
		if err == nil {
			return cli, nil
		}

		if errors.Is(err, exec.ErrNotFound) {
			continue
		}

		found = append(found, cli)
	}

	if len(found) == 0 {
		return "", fmt.Errorf("%w: install Docker (https://docs.docker.com/get-docker/) or another container runtime (podman, finch, nerdctl)", ErrNoContainerCLI)
	}

	first := found[0]

	return "", fmt.Errorf("%w: %s found but daemon unreachable — start Docker Desktop (or the equivalent for %s)", ErrDaemonUnreachable, first, first)
}
