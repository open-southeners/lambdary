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
// /var/task, per plans/m1-container-path.md Unit B's exact flag order.
// fileEnv is the (already-loaded) content of the function's local.env_file,
// or nil when it has none — see envMerge for the precedence between it and
// the manifest's own environment block. It is a pure function — no I/O, no
// CLI invocation — so tests can assert the built argv directly against a
// fake runner's recorded calls.
func runArgs(fn discovery.Function, image, fnDir string, fileEnv map[string]string) []string {
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

	// AWS's provided.* base images hardcode RUNTIME_ENTRYPOINT to
	// /var/runtime/bootstrap, which is empty in the base image itself — so
	// without this second mount the entrypoint has nothing to exec. This
	// applies even under a local.image override, since a custom
	// provided-family image mimics the AWS base image's entrypoint.
	// Dockerfile-marker functions never reach here with fn.Runtime set (see
	// isDockerfileFunction), so they're unaffected. See
	// plans/dead-runtime-and-provided-container.md.
	if provided {
		args = append(args, "-v", fnDir+"/bootstrap:/var/runtime/bootstrap:ro")
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
