// Package rie resolves a local, runnable copy of AWS's Lambda Runtime
// Interface Emulator (the binary the process backend, internal/backend/
// process, spawns per function), per DESIGN.md's "Process backend" section
// and plans/m3-process-path.md's Unit A. AWS only ships Linux binaries, so
// on other platforms (this repo's own dev host: darwin/arm64, per
// plans/rie-darwin-spike.md) Resolve builds the pinned upstream tag from
// source and caches the result — go:embed-ing a prebuilt binary into
// Lambdary releases needs a release pipeline that doesn't exist yet
// (CURRENT_ISSUES.md).
package rie

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/open-southeners/lambdary/internal/backend"
)

// Version is the upstream aws-lambda-runtime-interface-emulator tag Resolve
// builds and caches. Bumping it changes the cache filename (see cachePath),
// so a version bump never serves a stale binary out of an old cache.
const Version = "v1.35"

// repoURL is the upstream repository Resolve shallow-clones at Version when
// building from source.
const repoURL = "https://github.com/aws/aws-lambda-runtime-interface-emulator"

// Environment variables consulted by Resolve, per plans/m3-process-path.md's
// "RIE acquisition chain".
const (
	// envRIEPath, when set, is used verbatim as the RIE binary path — the
	// power-user/CI escape hatch that skips the cache and source-build
	// steps entirely.
	envRIEPath = "LAMBDARY_RIE_PATH"
	// envHome overrides Lambdary's cache root (default ~/.lambdary), the
	// same env var the process backend's shim cache respects — kept here
	// too so tests can point Resolve at a temp directory.
	envHome = "LAMBDARY_HOME"
)

// ErrBuildToolsMissing is wrapped into the error Resolve returns when
// building from source requires git or go and one of them isn't on PATH.
// errors.Is(err, ErrBuildToolsMissing) lets callers distinguish this case
// from other build failures.
var ErrBuildToolsMissing = errors.New("rie: build tools missing")

// Resolve returns the path to a runnable aws-lambda-rie binary, acquiring
// one via the chain documented in plans/m3-process-path.md:
//
//  1. $LAMBDARY_RIE_PATH, if set, is used as-is (after verifying it exists
//     and is executable).
//  2. The per-version, per-platform cache under $LAMBDARY_HOME/bin (default
//     ~/.lambdary/bin), if already populated.
//  3. A shallow clone of the pinned upstream tag, built from source with
//     `go build` and cached for next time.
//
// External commands the build step needs (git, go) go through runner so
// callers can substitute a fake in tests, per internal/backend's Runner
// convention — the freshly built binary itself is never executed as part
// of Resolve (see verifyBuiltFile). Resolve never shells out for step 1 or
// a cache hit (step 2), so tests can assert those paths issue zero runner
// calls.
func Resolve(ctx context.Context, runner backend.Runner) (string, error) {
	if override := os.Getenv(envRIEPath); override != "" {
		return resolveOverride(override)
	}

	cache, err := cachePath()
	if err != nil {
		return "", err
	}

	if info, err := os.Stat(cache); err == nil && !info.IsDir() {
		return cache, nil
	}

	return build(ctx, runner, cache)
}

// resolveOverride implements acquisition step 1: $LAMBDARY_RIE_PATH must
// name an existing, executable, non-directory file — anything else is an
// error naming the env var, since a bad override should fail loudly rather
// than silently fall through to the cache/build steps.
func resolveOverride(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("rie: $%s=%s: %w", envRIEPath, path, err)
	}

	if info.IsDir() {
		return "", fmt.Errorf("rie: $%s=%s is a directory, not an executable binary", envRIEPath, path)
	}

	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("rie: $%s=%s is not executable", envRIEPath, path)
	}

	return path, nil
}

// cachePath returns the path Resolve reads/writes for the current platform
// and pinned Version: $LAMBDARY_HOME/bin/aws-lambda-rie-<Version>-<GOOS>-
// <GOARCH>. Encoding version/os/arch into the filename means a Version bump
// or running the same $LAMBDARY_HOME on a different platform never collides
// with (or serves) a stale/foreign binary.
func cachePath() (string, error) {
	home, err := lambdaryHome()
	if err != nil {
		return "", err
	}

	name := fmt.Sprintf("aws-lambda-rie-%s-%s-%s", Version, runtime.GOOS, runtime.GOARCH)

	return filepath.Join(home, "bin", name), nil
}

// lambdaryHome returns $LAMBDARY_HOME if set, otherwise ~/.lambdary.
func lambdaryHome() (string, error) {
	if home := os.Getenv(envHome); home != "" {
		return home, nil
	}

	dir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("rie: resolving user home directory: %w", err)
	}

	return filepath.Join(dir, ".lambdary"), nil
}

// build implements acquisition step 3: shallow-clone repoURL at Version
// into a temp dir, build the emulator binary with `go build`, verify the
// output file landed (see verifyBuiltFile), then install it into cache. The
// temp clone is always removed, success or failure.
func build(ctx context.Context, runner backend.Runner, cache string) (string, error) {
	if err := checkBuildTool(ctx, runner, "git", "--version"); err != nil {
		return "", err
	}

	if err := checkBuildTool(ctx, runner, "go", "version"); err != nil {
		return "", err
	}

	// Printed unconditionally (not just on a TTY) so a dev watching plain
	// stderr output during the first `lambdary dev`/`invoke` run sees why
	// nothing has happened yet for ~30s, per plans/m3-process-path.md.
	fmt.Fprintf(os.Stderr, "building AWS Lambda runtime emulator %s (first run, ~30s)...\n", Version)

	tmpDir, err := os.MkdirTemp("", "lambdary-rie-build-*")
	if err != nil {
		return "", fmt.Errorf("rie: creating build temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup of a temp dir

	cloneDir := filepath.Join(tmpDir, "src")

	if _, err := runner.Run(ctx, "git", "clone", "--depth", "1", "--branch", Version, repoURL, cloneDir); err != nil {
		return "", fmt.Errorf("rie: cloning %s at %s: %w", repoURL, Version, err)
	}

	outPath := filepath.Join(tmpDir, "aws-lambda-rie")

	// The Runner interface (internal/backend/exec.go) has no cwd/env
	// parameters, so the working-directory change and GOTOOLCHAIN pin are
	// folded into a shell command instead of extending that interface just
	// for this one caller.
	buildCmd := fmt.Sprintf("cd %s && GOTOOLCHAIN=auto go build -o %s ./cmd/aws-lambda-rie", shellQuote(cloneDir), shellQuote(outPath))

	if _, err := runner.Run(ctx, "sh", "-c", buildCmd); err != nil {
		return "", fmt.Errorf("rie: building %s from source: %w", Version, err)
	}

	if err := verifyBuiltFile(outPath); err != nil {
		return "", err
	}

	if err := installBinary(outPath, cache); err != nil {
		return "", err
	}

	return cache, nil
}

// checkBuildTool runs `<name> <args...>` (a cheap version/availability
// probe) through runner and turns a "not found on PATH" failure into
// ErrBuildToolsMissing, naming both the missing tool and the
// $LAMBDARY_RIE_PATH escape hatch. Any other failure (the tool exists but
// errored) is returned as-is, since that's not a "missing tools" situation.
func checkBuildTool(ctx context.Context, runner backend.Runner, name string, args ...string) error {
	_, err := runner.Run(ctx, name, args...)
	if err == nil {
		return nil
	}

	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%w: %s not found on PATH — install %s, or set $%s to a prebuilt aws-lambda-rie binary", ErrBuildToolsMissing, name, name, envRIEPath)
	}

	return fmt.Errorf("rie: checking %s availability: %w", name, err)
}

// verifyBuiltFile checks that the build step actually produced a non-empty
// regular file at path. This is a no-run check by design: RIE has no
// --help/-h flag — any trailing args are taken as the bootstrap command to
// launch, so invoking the freshly built binary here (e.g. "<path> --help")
// would boot its full sandbox instead of printing usage, hanging forever
// with the internal Runtime API port free, or panicking on a bind error
// with it taken (see plans/rie-darwin-spike.md's port-9001 note). `go
// build` exiting 0 already means a working binary for this target; real
// runtime validation happens at first use, via the process backend's own
// readiness check.
func verifyBuiltFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("rie: built binary %s missing after build: %w", path, err)
	}

	if info.IsDir() {
		return fmt.Errorf("rie: built binary %s is a directory, not a binary", path)
	}

	if info.Size() == 0 {
		return fmt.Errorf("rie: built binary %s is empty", path)
	}

	return nil
}

// installBinary moves src (the freshly built binary) to dst (the cache
// path), creating dst's parent directory and ensuring dst is executable.
// os.Rename is tried first; since src (a temp dir, usually under the OS
// temp filesystem) and dst (under $LAMBDARY_HOME) can be on different
// filesystems, a cross-device rename falls back to a copy-then-remove.
func installBinary(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("rie: creating cache directory: %w", err)
	}

	if err := os.Rename(src, dst); err == nil {
		return chmodExecutable(dst)
	}

	if err := copyFile(src, dst); err != nil {
		return err
	}

	os.Remove(src) //nolint:errcheck // best-effort cleanup; the temp dir removal in build() also covers this

	return chmodExecutable(dst)
}

// copyFile copies src to dst, overwriting dst if it exists.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("rie: opening built binary: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("rie: creating cached binary: %w", err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // already failing; original error takes priority

		return fmt.Errorf("rie: copying built binary into cache: %w", err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("rie: copying built binary into cache: %w", err)
	}

	return nil
}

func chmodExecutable(path string) error {
	if err := os.Chmod(path, 0o755); err != nil {
		return fmt.Errorf("rie: making cached binary executable: %w", err)
	}

	return nil
}

// shellQuote wraps s in single quotes for safe interpolation into the `sh
// -c` command build issues, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
