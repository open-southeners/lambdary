package container

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// archPlatforms maps AWS Lambda architecture identifiers (manifest
// `architectures` entries) to the arch component of a `docker run
// --platform linux/<arch>` value. AWS uses "x86_64" where Docker's platform
// syntax uses "amd64"; "arm64" is spelled the same in both.
var archPlatforms = map[string]string{
	"arm64":  "arm64",
	"x86_64": "amd64",
}

// runArgs builds the full `<cli> run ...` argv for starting fn as a
// container from image, with fnDir (an absolute path) bind-mounted at
// /var/task, per plans/m1-container-path.md Unit B's exact flag order —
// extended by plans/layers.md's Unit C, which inserts an `/opt` mount right
// after `/var/task` and repoints the provided.* bootstrap mount at a
// resolved source instead of always fnDir's own. fileEnv is the
// (already-loaded) content of the function's local.env_file, or nil when it
// has none — see envMerge for the precedence between it and the manifest's
// own environment block. staging is fn's layer staging directory
// (layers.Stage's return value) or "" when fn has no `layers:` entries — in
// which case the `/opt` mount is skipped entirely, keeping the argv
// byte-identical to before layers existed. bootstrapSrc is the bind-mount
// source Start resolved for a provided.* runtime's
// `/var/runtime/bootstrap` (see Start's comment for the search order); it
// is ignored for non-provided runtimes. runArgs is a pure function — no
// I/O, no CLI invocation, no stat calls of its own — so tests can assert
// the built argv directly against a fake runner's recorded calls; every
// filesystem decision (staging, bootstrap resolution) is made by the
// caller, in Start.
func runArgs(fn discovery.Function, image, fnDir string, fileEnv map[string]string, staging, bootstrapSrc string) []string {
	m := fn.Manifest
	if m == nil {
		m = &manifest.Manifest{}
	}

	provided := strings.HasPrefix(fn.Runtime, "provided.")

	args := []string{
		"run", "-d", "--rm",
		"--label", "lambdary=1",
		"--label", "lambdary.function=" + fn.Name,
		"-p", "127.0.0.1:0:8080",
		"-v", fnDir + ":/var/task:ro",
	}

	// staging (fn's merged layer content, "" when fn has none) is mounted
	// read-only at /opt, right after /var/task — the same location and
	// precedence AWS's own base images use for layer content, per
	// plans/layers.md's Unit C.
	if staging != "" {
		args = append(args, "-v", staging+":/opt:ro")
	}

	// AWS's provided.* base images hardcode RUNTIME_ENTRYPOINT to
	// /var/runtime/bootstrap, which is empty in the base image itself — so
	// without this mount the entrypoint has nothing to exec. bootstrapSrc is
	// Start's resolution of real Lambda's own search order: fnDir's own
	// bootstrap wins when present (matching /var/task's precedence over
	// /opt); otherwise a bootstrap supplied by a layer, staged at
	// <staging>/bootstrap — the Bref parity win, so a composer.json/
	// `layers:` function needs no local bootstrap file at all
	// (plans/layers.md Unit C); otherwise fnDir's own (missing) bootstrap
	// path, preserving the pre-layers Docker empty-directory bind-mount
	// quirk unchanged (Start's own stat guard is what actually keeps
	// functions from hitting it in practice — see its comment). This
	// applies even under a local.image override, since a custom
	// provided-family image mimics the AWS base image's entrypoint.
	// Dockerfile-marker functions never reach here with fn.Runtime set (see
	// isDockerfileFunction), so they're unaffected. See
	// plans/dead-runtime-and-provided-container.md and plans/layers.md Unit
	// C.
	if provided {
		args = append(args, "-v", bootstrapSrc+":/var/runtime/bootstrap:ro")
	}

	env := envMerge(fileEnv, m.Environment)
	for _, k := range sortedKeys(env) {
		args = append(args, "-e", k+"="+env[k])
	}

	args = append(args, "-e", "AWS_LAMBDA_FUNCTION_NAME="+fn.Name)

	if m.Timeout > 0 {
		args = append(args, "-e", fmt.Sprintf("AWS_LAMBDA_FUNCTION_TIMEOUT=%d", m.Timeout))
	}

	if m.Memory > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", m.Memory))
		args = append(args, "-e", fmt.Sprintf("AWS_LAMBDA_FUNCTION_MEMORY_SIZE=%d", m.Memory))
	}

	if len(m.Architectures) > 0 {
		args = append(args, "--platform", platformFor(m.Architectures[0]))
	}

	args = append(args, image)

	if fn.Handler != "" {
		args = append(args, fn.Handler)
	} else if provided {
		// The entrypoint requires *a* handler argument or it exits
		// immediately — it becomes _HANDLER, which a custom provided.*
		// runtime is free to ignore, so any placeholder value works.
		args = append(args, "bootstrap")
	}

	return args
}

// platformFor renders arch (an AWS architecture identifier, e.g. "arm64" or
// "x86_64") as a `docker run --platform` value. Architectures not in
// archPlatforms are passed through as-is under "linux/" — best effort for
// any future AWS architecture identifier that happens to already match
// Docker's own spelling.
func platformFor(arch string) string {
	if a, ok := archPlatforms[arch]; ok {
		arch = a
	}

	return "linux/" + arch
}

// envMerge combines fileEnv (local.env_file's parsed content) and
// manifestEnv (the manifest's own `environment` block) into the environment
// runArgs passes to the container, with manifestEnv's keys winning on
// conflict — per plans/m5-extras.md's Unit D ("merge UNDER manifest
// environment"), env_file exists to supply defaults or secrets the manifest
// itself doesn't already set, not to override it.
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
// passed to the container in a deterministic sequence regardless of Go's
// randomized map iteration — both for reproducible `docker run` argv across
// runs and so unit tests can assert an exact argv slice.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
