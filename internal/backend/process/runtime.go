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
// families. Since plans/process-ruby-and-container-fallback.md's Unit B,
// internal/cli catches this ahead of time via Supports and falls back to
// the container backend instead of letting an invoke fail outright, so in
// practice this error now only surfaces when that fallback itself has
// nowhere to go (no usable container runtime either).
var ErrRuntimeNotSupported = errors.New("runtime not supported by the process backend yet")

// nodeRuntimePrefix, pythonRuntimePrefix, and rubyRuntimePrefix are the
// fn.Runtime prefixes runtimeCommand's switch and Supports both recognize
// as having an embedded shim; providedRuntimePrefix is recognized too, but
// runs the function's own ./bootstrap instead of a shim (see
// runtimeCommand's provided.* branch). Named consts instead of inline
// string literals so processRuntimePrefixes below and runtimeCommand's
// switch cases can't drift apart — see processRuntimePrefixes' comment.
const (
	nodeRuntimePrefix     = "nodejs"
	pythonRuntimePrefix   = "python"
	rubyRuntimePrefix     = "ruby"
	providedRuntimePrefix = "provided"
)

// processRuntimePrefixes lists every fn.Runtime prefix the process backend
// can run: it's the single source of truth Supports iterates, built from
// the same named consts runtimeCommand's switch cases use below, so the two
// can never recognize a different set of runtimes — the drift
// plans/process-ruby-and-container-fallback.md's Unit B calls out as the
// reason Supports must not hand-maintain its own copy of this list.
var processRuntimePrefixes = []string{nodeRuntimePrefix, pythonRuntimePrefix, rubyRuntimePrefix, providedRuntimePrefix}

// Supports reports whether the process backend can run fn: true when the
// function's manifest sets local.command (runtimeCommand's local.command
// branch handles it regardless of fn.Runtime), or fn.Runtime has one of
// processRuntimePrefixes. It exists so internal/cli can decide, *before*
// calling Start, whether to fall back to the container backend instead of
// only discovering ErrRuntimeNotSupported after a spawn attempt fails — see
// plans/process-ruby-and-container-fallback.md's Unit B ("container
// fallback for unsupported runtimes"). Note that provided.* counts as
// supported here even when the function's own ./bootstrap file is missing
// or non-executable *and it has no layers configured*: that's
// ErrBootstrapMissing, a configuration mistake the container backend would
// hit too (it needs that same bootstrap file), not a missing-shim gap — so
// it must stay a hard error rather than silently triggering a container
// fallback.
//
// Carve-out (plans/layers.md Unit D): a provided.* function that *does*
// configure layers: and has no local ./bootstrap is different — its
// bootstrap is expected to come from a layer, which is Amazon-Linux
// content the host can't exec (see RequiresLayerBootstrap). Supports
// reports false for that case so internal/cli's existing
// needsContainerFallback gate routes it to the container backend, the same
// path java21/dotnet8 already take, instead of hitting ErrBootstrapMissing
// at spawn time for something that would actually work in a container.
// This is the one case where Supports does an os.Stat (via
// hasExecutableBootstrap) rather than deciding from fn.Runtime alone.
func Supports(fn discovery.Function) bool {
	m := fn.Manifest
	if m != nil && m.Local.Command != "" {
		return true
	}

	if RequiresLayerBootstrap(fn) {
		return false
	}

	for _, prefix := range processRuntimePrefixes {
		if strings.HasPrefix(fn.Runtime, prefix) {
			return true
		}
	}

	return false
}

// RequiresLayerBootstrap reports whether fn is exactly the provided.* +
// layers + no local ./bootstrap carve-out Supports' doc comment describes:
// a custom runtime whose bootstrap is expected to come from a layer
// (Bref's win — see plans/layers.md Unit D and Unit C's container-side
// bootstrap search order) rather than from the function's own directory.
// Such a bootstrap is Amazon-Linux layer content, built for the container
// backend's base image, not something Supports can let the process
// backend attempt to exec on the host. internal/cli's containerFallbackNotice
// uses this too, to give this case its own accurate wording instead of the
// generic "no process-backend shim" message — nodejs/python/ruby runtimes
// truly have no host-runnable equivalent, but a provided.* function with
// layers does, once it lands in a container.
func RequiresLayerBootstrap(fn discovery.Function) bool {
	return strings.HasPrefix(fn.Runtime, providedRuntimePrefix) && hasLayers(fn.Manifest) && !hasExecutableBootstrap(fn.Dir)
}

// hasLayers reports whether m configures any layers: entries. A nil
// manifest (mirroring every other m == nil check in this file) has none.
func hasLayers(m *manifest.Manifest) bool {
	return m != nil && len(m.Layers) > 0
}

// hasExecutableBootstrap reports whether fnDir contains an executable
// `bootstrap` file, using the same shape check runtimeCommand's provided.*
// branch already applies at spawn time (not a directory, at least one
// executable bit set). Shared so Supports' layers carve-out and
// runtimeCommand's ErrBootstrapMissing check can never recognize a
// different "has a local bootstrap" answer for the same function.
func hasExecutableBootstrap(fnDir string) bool {
	info, err := os.Stat(filepath.Join(fnDir, "bootstrap"))

	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// runtimeCommand resolves the command Start spawns as the RIE's trailing
// argv — the process the RIE's internal Runtime API talks to. absDir is
// fn's directory (absolute); shimDir is where writeShims wrote the embedded
// Node/Python/Ruby shims. Resolution order, per plans/m3-process-path.md
// Unit B and plans/process-ruby-and-container-fallback.md Unit A:
//
//  1. local.command, if set: run it via `sh -c <command>`. cwd is always
//     the function directory regardless of which branch is taken (spawn
//     always sets it), so "in the function dir" needs no special-casing
//     here.
//  2. nodejs* runtimes: `node <shimDir>/bootstrap.mjs <handler>`.
//  3. python* runtimes: `python3 <shimDir>/bootstrap.py <handler>`.
//  4. ruby* runtimes: `ruby <shimDir>/bootstrap.rb <handler>`.
//  5. provided.* runtimes: the function's own `./bootstrap`, which must
//     exist and be executable — ErrBootstrapMissing otherwise.
//  6. anything else: ErrRuntimeNotSupported, naming the runtime — Supports
//     above reports false for exactly this case, so internal/cli's
//     per-function resolver and standalone invoke path never actually reach
//     this branch in practice; it stays as a safety net.
func runtimeCommand(fn discovery.Function, absDir, shimDir string) (name string, args []string, err error) {
	m := fn.Manifest
	if m == nil {
		m = &manifest.Manifest{}
	}

	if m.Local.Command != "" {
		return "sh", []string{"-c", m.Local.Command}, nil
	}

	switch {
	case strings.HasPrefix(fn.Runtime, nodeRuntimePrefix):
		return "node", []string{filepath.Join(shimDir, nodeShimName), fn.Handler}, nil
	case strings.HasPrefix(fn.Runtime, pythonRuntimePrefix):
		return "python3", []string{filepath.Join(shimDir, pythonShimName), fn.Handler}, nil
	case strings.HasPrefix(fn.Runtime, rubyRuntimePrefix):
		return "ruby", []string{filepath.Join(shimDir, rubyShimName), fn.Handler}, nil
	case strings.HasPrefix(fn.Runtime, providedRuntimePrefix):
		if !hasExecutableBootstrap(absDir) {
			return "", nil, fmt.Errorf("%w: %s", ErrBootstrapMissing, filepath.Join(absDir, "bootstrap"))
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
// it launches): fileEnv (the function's already-loaded local.env_file, or
// nil when it has none) merged under the manifest's own environment block
// (manifest wins on conflict — see envMerge), then AWS_LAMBDA_FUNCTION_NAME,
// then AWS_LAMBDA_FUNCTION_TIMEOUT and AWS_LAMBDA_FUNCTION_MEMORY_SIZE when
// the manifest sets them — mirroring container/argv.go's runArgs env
// handling for the container backend. A pure function so tests can assert
// it without inheriting the real process's environment. The RIE sets
// AWS_LAMBDA_RUNTIME_API on its runtime child itself; this package's shims
// just read it (see shims/bootstrap.mjs and shims/bootstrap.py).
func manifestEnv(fn discovery.Function, fileEnv map[string]string) []string {
	m := fn.Manifest
	if m == nil {
		m = &manifest.Manifest{}
	}

	merged := envMerge(fileEnv, m.Environment)

	env := make([]string, 0, len(merged)+3)

	for _, k := range sortedKeys(merged) {
		env = append(env, k+"="+merged[k])
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

// envMerge combines fileEnv (local.env_file's parsed content) and
// manifestEnv (the manifest's own `environment` block) into the environment
// the manifestEnv function above passes to the RIE, with manifestEnv's keys
// winning on conflict — per plans/m5-extras.md's Unit D ("merge UNDER
// manifest environment"), env_file exists to supply defaults or secrets the
// manifest itself doesn't already set, not to override it. Mirrors
// container/argv.go's envMerge for the container backend.
func envMerge(fileEnv, manifestEnv map[string]string) map[string]string {
	if len(fileEnv) == 0 {
		return manifestEnv
	}

	merged := make(map[string]string, len(fileEnv)+len(manifestEnv))
	for k, v := range fileEnv {
		merged[k] = v
	}
	for k, v := range manifestEnv {
		merged[k] = v
	}

	return merged
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
