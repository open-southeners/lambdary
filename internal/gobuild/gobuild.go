// Package gobuild compiles Go functions (discovery.Function.Go) into the
// `bootstrap` binary a provided.* runtime executes. Both execution backends
// call Build on every function start, so hot reload's restart-on-change
// rebuilds the binary with no watcher-side build step; Go's own build cache
// keeps an unchanged rebuild cheap.
package gobuild

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
)

// norpcTag is the build tag aws-lambda-go documents for provided.*
// runtimes: it drops the legacy go1.x RPC server from the binary. Harmless
// for functions that don't use aws-lambda-go.
const norpcTag = "lambda.norpc"

// outputTailLines is how many trailing lines of `go build` output a
// build-failure error carries, matching the container backend's
// Dockerfile build errors.
const outputTailLines = 20

// archGOARCH maps AWS Lambda architecture identifiers (manifest
// `architectures` entries) to GOARCH values.
var archGOARCH = map[string]string{
	"arm64":  "arm64",
	"x86_64": "amd64",
}

// Build compiles src's main package for goos/goarch into out, creating
// out's directory first. The build runs through runner as
// `env CGO_ENABLED=0 GOOS=… GOARCH=… go -C <module> build …`, so it needs a
// Go toolchain on the host's PATH. CGO is disabled because the binary has
// to run on Lambda's base image, not the machine that built it.
func Build(ctx context.Context, runner backend.Runner, src discovery.GoSource, goos, goarch, out string) error {
	// `go -C` resolves a relative -o against the module, while callers mean
	// their own working directory (the cache dir is relative when --root is).
	out, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("go build %s: %w", src.Package, err)
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("go build %s: %w", src.Package, err)
	}

	tags := append([]string{norpcTag}, src.Tags...)

	args := []string{
		"CGO_ENABLED=0", "GOOS=" + goos, "GOARCH=" + goarch,
		"go", "-C", src.ModuleDir, "build", "-tags", strings.Join(tags, ","),
	}
	if src.LDFlags != "" {
		args = append(args, "-ldflags", src.LDFlags)
	}
	args = append(args, "-o", out, src.Package)

	if output, err := runner.Run(ctx, "env", args...); err != nil {
		return fmt.Errorf("go build %s: %w%s", src.Package, err, outputTail(string(output)))
	}

	return nil
}

// OutputPath is where a function's built bootstrap lives for one target:
// under the project's `.lambdary` cache dir, never in the source tree, so
// writing it can't retrigger the hot-reload watcher. Separate per target
// because the container backend builds for linux and the process backend
// for the host.
func OutputPath(cacheDir, fnName, goos, goarch string) string {
	return filepath.Join(cacheDir, "build", fnName, goos+"_"+goarch, "bootstrap")
}

// ArchFor returns the GOARCH matching a manifest's first `architectures`
// entry, or the host's when none is set or it is not one Lambda supports.
func ArchFor(architectures []string) string {
	if len(architectures) > 0 {
		if goarch, ok := archGOARCH[architectures[0]]; ok {
			return goarch
		}
	}

	return runtime.GOARCH
}

// outputTail returns the last outputTailLines non-blank lines of `go
// build` output, formatted for appending to a build-failure error, or ""
// when there is none.
func outputTail(out string) string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}

	if len(lines) == 0 {
		return ""
	}

	if len(lines) > outputTailLines {
		lines = lines[len(lines)-outputTailLines:]
	}

	return "\nbuild output:\n" + strings.Join(lines, "\n")
}
