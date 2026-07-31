package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// RIE flag names for the two addresses Start always passes together —
// verified against upstream v1.35's flag definitions
// (internal/lambda/rie/run.go: `long:"runtime-api-address"` and
// `long:"runtime-interface-emulator-address"`), matching the public flag
// plans/rie-darwin-spike.md already exercised. Both must always be set
// together: the RIE's internal Runtime API server defaults to port 9001
// when runtimeAPIAddressFlag is omitted, which can collide with whatever
// port emulatorAddressFlag happens to get — the spike's port-9001 pitfall.
const (
	emulatorAddressFlag   = "--runtime-interface-emulator-address"
	runtimeAPIAddressFlag = "--runtime-api-address"
)

// ErrBootstrapMissing indicates a provided.* (custom-runtime) function has
// no executable `bootstrap` file in its directory, per DESIGN.md's
// detection table note ("a `bootstrap` file is expected").
var ErrBootstrapMissing = errors.New("no executable bootstrap file found for a custom (provided.*) runtime")

// ErrRuntimeNotSupported indicates fn.Runtime isn't one the process backend
// can run yet — see plans/m3-process-path.md's Unit B scope (Node, Python,
// and custom runtimes only) and CURRENT_ISSUES.md for the deferred
// families.
var ErrRuntimeNotSupported = errors.New("runtime not supported by the process backend yet")

// runtimeCommand resolves the command Start spawns as the RIE's trailing
// argv — the process the RIE's internal Runtime API talks to. absDir is
// fn's directory (absolute); shimDir is where writeShims wrote the embedded
// Node/Python shims. Resolution order, per plans/m3-process-path.md Unit B:
//
//  1. local.command, if set: run it via `sh -c <command>`. cwd is always
//     the function directory regardless of which branch is taken (spawn
//     always sets it), so "in the function dir" needs no special-casing
//     here.
//  2. nodejs* runtimes: `node <shimDir>/bootstrap.mjs <handler>`.
//  3. python* runtimes: `python3 <shimDir>/bootstrap.py <handler>`.
//  4. provided.* runtimes: the function's own `./bootstrap`, which must
//     exist and be executable — ErrBootstrapMissing otherwise.
//  5. anything else: ErrRuntimeNotSupported, naming the runtime.
func runtimeCommand(fn discovery.Function, absDir, shimDir string) (name string, args []string, err error) {
	m := fn.Manifest
	if m == nil {
		m = &manifest.Manifest{}
	}

	if m.Local.Command != "" {
		return "sh", []string{"-c", m.Local.Command}, nil
	}

	switch {
	case strings.HasPrefix(fn.Runtime, "nodejs"):
		return "node", []string{filepath.Join(shimDir, nodeShimName), fn.Handler}, nil
	case strings.HasPrefix(fn.Runtime, "python"):
		return "python3", []string{filepath.Join(shimDir, pythonShimName), fn.Handler}, nil
	case strings.HasPrefix(fn.Runtime, "provided"):
		bootstrap := filepath.Join(absDir, "bootstrap")

		info, statErr := os.Stat(bootstrap)
		if statErr != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return "", nil, fmt.Errorf("%w: %s", ErrBootstrapMissing, bootstrap)
		}

		return "./bootstrap", nil, nil
	default:
		return "", nil, fmt.Errorf("%w: %q", ErrRuntimeNotSupported, fn.Runtime)
	}
}

// rieArgs builds the full argv Start passes to riePath: both address flags
// (invokePort for emulatorAddressFlag, the public Invoke API;  rapiPort for
// runtimeAPIAddressFlag, the RIE's internal Runtime API) followed by the
// runtime command and its own args as trailing positional args — the RIE
// takes the bootstrap command as trailing args per
// plans/rie-darwin-spike.md. A pure function so tests can assert the built
// argv directly without spawning anything.
func rieArgs(invokePort, rapiPort int, runtimeName string, runtimeArgs []string) []string {
	args := []string{
		emulatorAddressFlag, fmt.Sprintf("127.0.0.1:%d", invokePort),
		runtimeAPIAddressFlag, fmt.Sprintf("127.0.0.1:%d", rapiPort),
		runtimeName,
	}

	return append(args, runtimeArgs...)
}

// manifestEnv builds the env vars Start adds on top of the inherited
// os.Environ() for the spawned RIE (and, transitively, the runtime process
// it launches): the manifest's own environment block, then
// AWS_LAMBDA_FUNCTION_NAME, then AWS_LAMBDA_FUNCTION_TIMEOUT and
// AWS_LAMBDA_FUNCTION_MEMORY_SIZE when the manifest sets them — mirroring
// container/argv.go's runArgs env handling for the container backend. A
// pure function so tests can assert it without inheriting the real
// process's environment. The RIE sets AWS_LAMBDA_RUNTIME_API on its runtime
// child itself; this package's shims just read it (see shims/bootstrap.mjs
// and shims/bootstrap.py).
func manifestEnv(fn discovery.Function) []string {
	m := fn.Manifest
	if m == nil {
		m = &manifest.Manifest{}
	}

	env := make([]string, 0, len(m.Environment)+3)

	for _, k := range sortedKeys(m.Environment) {
		env = append(env, k+"="+m.Environment[k])
	}

	env = append(env, "AWS_LAMBDA_FUNCTION_NAME="+fn.Name)

	if m.Timeout > 0 {
		env = append(env, fmt.Sprintf("AWS_LAMBDA_FUNCTION_TIMEOUT=%d", m.Timeout))
	}

	if m.Memory > 0 {
		env = append(env, fmt.Sprintf("AWS_LAMBDA_FUNCTION_MEMORY_SIZE=%d", m.Memory))
	}

	return env
}

// sortedKeys returns m's keys in sorted order, so environment variables are
// assembled in a deterministic sequence regardless of Go's randomized map
// iteration — both for reproducible argv/env across runs and so unit tests
// can assert exact output.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
