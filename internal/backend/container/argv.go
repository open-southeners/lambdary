package container

import (
	"fmt"
	"sort"

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
// /var/task, per plans/m1-container-path.md Unit B's exact flag order. It
// is a pure function — no I/O, no CLI invocation — so tests can assert the
// built argv directly against a fake runner's recorded calls.
func runArgs(fn discovery.Function, image, fnDir string) []string {
	m := fn.Manifest
	if m == nil {
		m = &manifest.Manifest{}
	}

	args := []string{
		"run", "-d", "--rm",
		"--label", "lambdary=1",
		"--label", "lambdary.function=" + fn.Name,
		"-p", "127.0.0.1:0:8080",
		"-v", fnDir + ":/var/task:ro",
	}

	for _, k := range sortedKeys(m.Environment) {
		args = append(args, "-e", k+"="+m.Environment[k])
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
