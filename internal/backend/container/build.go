package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-southeners/lambdary/internal/discovery"
)

// buildTailLines is how many trailing lines of `<cli> build` output
// buildDockerfileImage attaches to a build-failure error, matching
// instance.go's logsTailLines so a build failure and a readiness-timeout
// failure surface diagnostics the same way.
const buildTailLines = 20

// isDockerfileFunction reports whether fn is a Dockerfile-marker function:
// per discovery.Function's doc comment, a backend resolved to "container"
// with no Runtime and no manifest local.image override only arises from a
// bare Dockerfile marker with no override, so Start must build an image
// from fn.Dir before there is anything to run.
func isDockerfileFunction(fn discovery.Function) bool {
	return fn.Runtime == "" && fn.Image == "" && fn.Backend == "container"
}

// dockerBuildTag returns the local image tag Start builds and runs a
// Dockerfile-marker function under: lambdary/<name>:local. The lambdary/
// namespace keeps it distinct from any pulled or registry image, and makes
// locally built images easy to spot (and clean up) with `<cli> images`.
func dockerBuildTag(fn discovery.Function) string {
	return "lambdary/" + fn.Name + ":local"
}

// buildDockerfileImage builds fnDir's Dockerfile into dockerBuildTag(fn) by
// running `<cli> build -t <tag> <fnDir>` through b.runner, returning the
// tag on success. It fails fast, without invoking the CLI, when fnDir has
// no Dockerfile — discovery only ever sets up the marker combination
// isDockerfileFunction checks for when a Dockerfile is actually present,
// so this is a defensive check against a Dockerfile removed after
// discovery ran (or a hand-built discovery.Function, as in tests) rather
// than a path Start expects to hit in normal operation.
//
// Start calls buildDockerfileImage on every (re)start of a Dockerfile-
// marker function, not just the first — there's no separate
// "has this changed" check. Docker's own build cache makes an unchanged
// Dockerfile/context effectively free to rebuild, and that's exactly what
// lets hot reload (Manager.Restart -> the next Ensure -> Start) pick up
// Dockerfile and code edits with none of Lambdary's own file-watching
// machinery involved: the rebuild simply happens as a side effect of
// restarting the function.
func (b *containerBackend) buildDockerfileImage(ctx context.Context, fn discovery.Function, fnDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(fnDir, "Dockerfile")); err != nil {
		return "", fmt.Errorf("no Dockerfile in %s: %w", fnDir, err)
	}

	tag := dockerBuildTag(fn)

	out, err := b.runner.Run(ctx, b.cli, "build", "-t", tag, fnDir)
	if err != nil {
		return "", fmt.Errorf("building %s: %w%s", tag, err, buildOutputTail(string(out)))
	}

	return tag, nil
}

// buildOutputTail returns the last buildTailLines non-blank lines of a
// `<cli> build` output, formatted for appending to a build-failure error,
// or "" when out is blank.
func buildOutputTail(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}

	lines := strings.Split(out, "\n")
	if len(lines) > buildTailLines {
		lines = lines[len(lines)-buildTailLines:]
	}

	return "\nbuild output:\n" + strings.Join(lines, "\n")
}
