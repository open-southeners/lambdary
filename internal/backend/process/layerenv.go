package process

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/open-southeners/lambdary/internal/discovery"
)

// nodeRuntimeVersionPattern, pythonRuntimeVersionPattern, and
// rubyRuntimeVersionPattern extract the version component layerSearchPathEnv
// needs from fn.Runtime, e.g. "nodejs22.x" -> "22", "python3.13" -> "3.13",
// "ruby3.3" -> "3.3". A runtime string that doesn't match (a form AWS hasn't
// shipped, or a test fixture using a bare "nodejs") just skips the
// version-specific search-path entry below rather than erroring — Stage
// already ran successfully by the time this is called, so refusing to add
// *any* search-path env over one unparsed version would throw away the
// entries that don't need it (PATH, and NODE_PATH/PYTHONPATH's
// version-independent form).
var (
	nodeRuntimeVersionPattern   = regexp.MustCompile(`^nodejs(\d+)\.x$`)
	pythonRuntimeVersionPattern = regexp.MustCompile(`^python(\d+\.\d+)$`)
	rubyRuntimeVersionPattern   = regexp.MustCompile(`^ruby(\d+\.\d+)$`)
)

// layerSearchPathEnv upserts, into env, the runtime search-path environment
// variables AWS's own layer merge points at /opt on real Lambda — here
// pointed at staging, the function's per-function layer staging directory
// built by internal/layers.Stage. Only called when staging is non-empty
// (Start skips this entirely for the common case of a function with no
// layers: configured, keeping behavior unchanged for it). fn.Runtime's
// prefix picks which family-specific vars apply, mirroring AWS's own layer
// path documentation, per plans/layers.md Unit D:
//
//   - nodejs<major>.x: NODE_PATH gains <staging>/nodejs/node_modules and
//     <staging>/nodejs/node<major>/node_modules;
//   - python<ver>: PYTHONPATH gains <staging>/python and
//     <staging>/python/lib/python<ver>/site-packages;
//   - ruby<ver>: RUBYLIB gains <staging>/ruby/lib, GEM_PATH gains
//     <staging>/ruby/gems/<abi> (the Ruby ABI version, e.g. ruby3.3 ->
//     3.3.0);
//   - every family, including provided.* and local.command overrides: PATH
//     gains <staging>/bin.
//
// Every path is added whether or not the subdirectory actually exists
// under staging — matching AWS, which sets these unconditionally, and
// every language's own module/PATH resolver already tolerates a missing
// search-path entry, so there is nothing to gain from an extra stat per
// entry on every Start.
func layerSearchPathEnv(env []string, fn discovery.Function, staging string) []string {
	env = upsertEnvPath(env, "PATH", filepath.Join(staging, "bin"))

	switch {
	case strings.HasPrefix(fn.Runtime, nodeRuntimePrefix):
		paths := []string{filepath.Join(staging, "nodejs", "node_modules")}
		if m := nodeRuntimeVersionPattern.FindStringSubmatch(fn.Runtime); m != nil {
			paths = append(paths, filepath.Join(staging, "nodejs", "node"+m[1], "node_modules"))
		}

		env = upsertEnvPath(env, "NODE_PATH", paths...)
	case strings.HasPrefix(fn.Runtime, pythonRuntimePrefix):
		paths := []string{filepath.Join(staging, "python")}
		if m := pythonRuntimeVersionPattern.FindStringSubmatch(fn.Runtime); m != nil {
			paths = append(paths, filepath.Join(staging, "python", "lib", "python"+m[1], "site-packages"))
		}

		env = upsertEnvPath(env, "PYTHONPATH", paths...)
	case strings.HasPrefix(fn.Runtime, rubyRuntimePrefix):
		env = upsertEnvPath(env, "RUBYLIB", filepath.Join(staging, "ruby", "lib"))

		if m := rubyRuntimeVersionPattern.FindStringSubmatch(fn.Runtime); m != nil {
			env = upsertEnvPath(env, "GEM_PATH", filepath.Join(staging, "ruby", "gems", m[1]+".0"))
		}
	}

	return env
}

// upsertEnvPath appends values (joined with os.PathListSeparator) to key's
// existing value in env, or adds a new "key=values" entry when key isn't
// already present — never a second, duplicate "key=..." entry, since two
// env vars with the same name have platform-dependent exec behavior (which
// one wins isn't guaranteed the same way on every OS/libc).
//
// The staging paths are appended AFTER any existing value rather than
// before: on real Lambda these search-path vars mostly don't pre-exist (the
// base image starts clean), so AWS's own "/opt entries first" ordering
// doesn't actually settle anything here. Putting the host's own value first
// instead means a developer's own NODE_PATH/PYTHONPATH/PATH — set in their
// shell profile, common for PATH especially — keeps resolving first, and
// layer content only fills in what the host doesn't already provide.
func upsertEnvPath(env []string, key string, values ...string) []string {
	if len(values) == 0 {
		return env
	}

	addition := strings.Join(values, string(os.PathListSeparator))
	prefix := key + "="

	for i, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			continue
		}

		existing := strings.TrimPrefix(kv, prefix)
		if existing == "" {
			env[i] = prefix + addition
		} else {
			env[i] = kv + string(os.PathListSeparator) + addition
		}

		return env
	}

	return append(env, prefix+addition)
}
