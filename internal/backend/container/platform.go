package container

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
)

// execFormatError is the kernel message a container prints when its
// binaries are built for another CPU architecture than it runs on.
const execFormatError = "exec format error"

// imagePlatform runs `<cli> image inspect` on ref and returns its
// "<os>/<architecture>" (e.g. "linux/arm64"), reporting an error when the
// image is not present locally.
func imagePlatform(ctx context.Context, runner backend.Runner, cli, ref string) (string, error) {
	out, err := runner.Run(ctx, cli, "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", ref)
	if err != nil {
		return "", err
	}

	return parseImagePlatform(string(out))
}

// parseImagePlatform extracts "<os>/<arch>" from the last line of
// `image inspect --format {{.Os}}/{{.Architecture}}` output, rejecting
// anything not shaped that way (e.g. an image with no recorded platform).
func parseImagePlatform(s string) (string, error) {
	p := lastLine(s)

	os, arch, ok := strings.Cut(p, "/")
	if !ok || os == "" || arch == "" {
		return "", fmt.Errorf("unexpected image platform %q", p)
	}

	return p, nil
}

// platformMatches reports whether a local image's platform ("linux/arm64",
// optionally with a variant such as "linux/arm64/v8") satisfies the wanted
// `--platform` value.
func platformMatches(have, want string) bool {
	return have == want || strings.HasPrefix(have, want+"/")
}

// ensureImage makes sure the local copy of the registry image ref is the
// wanted platform's variant, pulling it with `--platform` when the image is
// absent or is a different platform. Without this a variant cached under
// the plain tag by an earlier run for another architecture would be reused
// as is, and every container would die with an exec format error.
func (b *containerBackend) ensureImage(ctx context.Context, ref, platform string) error {
	if have, err := imagePlatform(ctx, b.runner, b.cli, ref); err == nil && platformMatches(have, platform) {
		return nil
	}

	if out, err := b.runner.Run(ctx, b.cli, "pull", "--platform", platform, ref); err != nil {
		return fmt.Errorf("pulling %s for %s: %w%s", ref, platform, err, buildOutputTail(string(out)))
	}

	// A successful pull is trusted unless the image positively reports
	// another platform (e.g. a locked digest that names a single-platform
	// manifest of the wrong architecture).
	if have, err := imagePlatform(ctx, b.runner, b.cli, ref); err == nil && !platformMatches(have, platform) {
		return fmt.Errorf("image %s is %s after pulling for %s", ref, have, platform)
	}

	return nil
}

// startFailed turns a failure after the container was created into the
// error Start returns, wrapped as a backend.StartFailedError. It inspects
// the container first: when it has exited, the report is its exit code and
// its last log lines (`function ping: container exited with code 255:
// exec /var/runtime/bootstrap: exec format error`); otherwise (still
// running but never ready) cause is reported followed by the log tail. The
// container is stopped and removed before returning. An exec format error
// in the output adds a hint about the architecture mismatch.
func (b *containerBackend) startFailed(ctx context.Context, fn discovery.Function, id string, cause error) error {
	info, ierr := inspect(ctx, b.runner, b.cli, id)
	logs := recentLogs(ctx, b.runner, b.cli, id)

	stopContainer(ctx, b.runner, b.cli, id)

	var detail string

	switch {
	case ierr == nil && !info.State.Running && info.State.Status != "":
		detail = fmt.Sprintf("container exited with code %d", info.State.ExitCode)
		if logs != "" {
			detail += ": " + logs
		}
	default:
		detail = cause.Error()
		if logs != "" {
			detail += "\nrecent logs:\n" + logs
		}
	}

	if strings.Contains(detail, execFormatError) {
		detail += "\n" + archHint(fn)
	}

	return &backend.StartFailedError{Function: fn.Name, Detail: detail, Cause: cause}
}

// archHint explains an exec format error: fn's architecture is not one the
// container engine's host can run.
func archHint(fn discovery.Function) string {
	arch := archOf(fn)

	other := "arm64"
	if arch == "arm64" {
		other = "x86_64"
	}

	return fmt.Sprintf("function %s targets %s but this Docker host cannot run %s images (install qemu/binfmt or set architectures: [%s])", fn.Name, arch, arch, other)
}

// recentLogs returns the container's last logsTailLines log lines, trimmed,
// or "" when there are none or they cannot be read.
func recentLogs(ctx context.Context, runner backend.Runner, cli, id string) string {
	out, err := runner.Run(ctx, cli, "logs", "--tail", logsTailLines, id)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(out))
}

// containerGone reports whether container id has stopped running, so the
// readiness wait can give up at once instead of polling a dead container.
// Any failure to tell counts as "not gone".
func (b *containerBackend) containerGone(ctx context.Context, id string) bool {
	info, err := inspect(ctx, b.runner, b.cli, id)

	return err == nil && !info.State.Running && info.State.Status != ""
}
