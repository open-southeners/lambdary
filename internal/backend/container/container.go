// Package container implements backend.Backend on top of a container CLI
// (docker/podman/finch/nerdctl), per DESIGN.md's "Container backend"
// section and plans/m1-container-path.md's Unit B. Every function gets its
// own container built from an AWS Lambda base image, with its code
// bind-mounted read-only at /var/task; the base image already runs the
// Runtime Interface Emulator as its entrypoint, so Start only needs to
// launch the container and wait for its invoke port to answer.
//
// Dockerfile-marker functions (see discovery.Function's doc comment) are
// built with `<cli> build` before every Start instead of being pulled from
// a fixed tag — see build.go. The supported target is Dockerfiles derived
// from AWS's own Lambda base images, per DESIGN.md's detection table: the
// RIE is already bundled in those images and listening on :8080 as the
// ENTRYPOINT/CMD, so Start runs the built image with no handler argument
// and lets the image decide how to invoke itself, exactly as it does for a
// pulled runtime image.
package container

import (
	"context"
	"fmt"
	"os"
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

// containerBackend is the backend.Backend implementation returned by New.
// readyTimeout is unexported and defaults to defaultReadyTimeout when zero;
// tests in this package construct containerBackend directly to override it
// with a short deadline instead of exercising the real 30s wait. lock is
// nil unless the backend was built with NewWithLock, in which case it's
// consulted/updated for registry-image digest pinning — see lock.go and
// Start's pinnable/usedDigest handling.
type containerBackend struct {
	cli          string
	runner       backend.Runner
	readyTimeout time.Duration
	lock         Lock
}

// New returns a backend.Backend that runs functions as containers via cli
// (e.g. "docker", "podman" — anything backend.DetectContainerCLI resolved),
// issuing every command through runner so callers can substitute a fake in
// tests. It has no image-digest lock (see NewWithLock); every image
// resolves by tag, exactly as before plans/m5-extras.md's Unit C.
func New(cli string, runner backend.Runner) backend.Backend {
	return &containerBackend{cli: cli, runner: runner}
}

// Start resolves fn's image (building it first for a Dockerfile-marker
// function — see build.go), launches it with the argv runArgs builds,
// resolves the ephemeral host port docker published :8080 on, and blocks
// until that port answers before returning — per backend.Backend.Start's
// contract that the returned Instance is immediately ready to invoke. On
// any failure after the container starts, Start stops it before returning
// so a failed Start never leaks a running container.
func (b *containerBackend) Start(ctx context.Context, fn discovery.Function) (backend.Instance, error) {
	absDir, err := filepath.Abs(fn.Dir)
	if err != nil {
		return nil, fmt.Errorf("container: %s: resolving function directory: %w", fn.Name, err)
	}

	fileEnv, err := loadEnvFile(fn, absDir)
	if err != nil {
		return nil, fmt.Errorf("container: %s: %w", fn.Name, err)
	}

	// Captured before the Dockerfile branch below can overwrite fn.Image
	// with a build tag: overridden is true only for a genuine
	// manifest local.image, which (like a build tag) is never pinned — see
	// the pinnable computation below.
	overridden := fn.Image != ""
	dockerfileFn := isDockerfileFunction(fn)

	if dockerfileFn {
		tag, err := b.buildDockerfileImage(ctx, fn, absDir)
		if err != nil {
			return nil, fmt.Errorf("container: %s: %w", fn.Name, err)
		}

		// The image's own ENTRYPOINT/CMD runs the function (the RIE plus
		// whatever handler the Dockerfile bakes in), so no handler
		// argument is appended to the run argv below.
		fn.Image = tag
		fn.Handler = ""
	}

	image, err := resolveImage(fn)
	if err != nil {
		return nil, fmt.Errorf("container: %s: %w", fn.Name, err)
	}

	// pinnable: only a registry image resolved from the manifest runtime
	// (backend.ImageFor) is a candidate for lock-based digest pinning — an
	// explicit local.image override or a freshly built lambdary/<name>:local
	// tag is already exactly what the user/Start chose, so a digest pin
	// adds nothing, per plans/m5-extras.md's Unit C.
	pinnable := b.lock != nil && !overridden && !dockerfileFn

	runImage := image
	usedDigest := false

	if pinnable {
		if digest, ok := b.lock.ImageDigest(image); ok {
			runImage = digestRef(image, digest)
			usedDigest = true
		}
	}

	// runArgs bind-mounts <absDir>/bootstrap to /var/runtime/bootstrap for a
	// provided.* function (see argv.go), and Docker/Podman silently create a
	// missing bind-mount *source* as an empty directory rather than erroring
	// — which would leave the container's RUNTIME_ENTRYPOINT pointing at a
	// directory instead of a bootstrap file and fail in a confusing way deep
	// inside the container. Catch the missing file here instead, with a
	// message that names the actual fix.
	if strings.HasPrefix(fn.Runtime, "provided.") {
		if _, err := os.Stat(filepath.Join(absDir, "bootstrap")); err != nil {
			return nil, fmt.Errorf("container: %s: provided.* runtime needs a bootstrap file in the function directory (or a function-owned Dockerfile): %w", fn.Name, err)
		}
	}

	args := runArgs(fn, runImage, absDir, fileEnv)

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

	// A successful start of a not-yet-pinned tag is the moment Start
	// resolves and records its digest, per plans/m5-extras.md's Unit C
	// ("after a successful start of an unlocked tag..."). Skipped entirely
	// when Start already ran by digest (usedDigest) — nothing new to
	// record.
	if pinnable && !usedDigest {
		b.recordDigest(ctx, image)
	}

	return &instance{cli: b.cli, runner: b.runner, id: id, port: port}, nil
}

// resolveImage picks the image Start runs fn from: the manifest's
// local.image override (fn.Image) when set, otherwise
// backend.ImageFor(fn.Runtime). Start sets fn.Image to the freshly built
// tag before calling resolveImage for a Dockerfile-marker function (see
// isDockerfileFunction and buildDockerfileImage in build.go), so by the
// time resolveImage runs there is always either an override, a build tag,
// or a resolvable runtime to fall back to.
func resolveImage(fn discovery.Function) (string, error) {
	if fn.Image != "" {
		return fn.Image, nil
	}

	return backend.ImageFor(fn.Runtime)
}

// loadEnvFile loads fn's local.env_file (manifest.Local.EnvFile), resolving
// a relative path against absDir (fn's directory, already made absolute) —
// per plans/m5-extras.md's Unit D. It returns a nil map and nil error when
// fn has no local.env_file configured at all. A configured path that can't
// be opened or parsed is a Start error naming the path: env_file was set
// explicitly, so silently running without it would hide a typo or a
// missing file rather than surfacing it.
func loadEnvFile(fn discovery.Function, absDir string) (map[string]string, error) {
	m := fn.Manifest
	if m == nil || m.Local.EnvFile == "" {
		return nil, nil
	}

	path := m.Local.EnvFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(absDir, path)
	}

	env, err := backend.ParseEnvFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading local.env_file: %w", err)
	}

	return env, nil
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
