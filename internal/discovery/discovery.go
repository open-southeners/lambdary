// Package discovery scans a functions root folder and resolves each direct
// subdirectory that looks like a Lambda function into a discovery.Function,
// per DESIGN.md's "Function discovery & runtime detection" section.
package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-southeners/lambdary/internal/manifest"
)

// manifestFileName is the function-level manifest DESIGN.md calls
// `.lambda.yml`.
const manifestFileName = ".lambda.yml"

// Function is a discovered Lambda function: a direct subdirectory of the
// scan root that contains a `.lambda.yml` manifest or one of the
// detectable project marker files from DESIGN.md's detection table.
type Function struct {
	// Name is the function name: the manifest's name, or the directory
	// name when unset.
	Name string
	// Dir is the function's directory, joined with the scan root.
	Dir string
	// Runtime is the resolved AWS Lambda runtime identifier (e.g.
	// "nodejs22.x"). Left empty for container functions detected from a
	// bare Dockerfile, which have no fixed runtime tag to report.
	Runtime string
	// Handler is the function handler, from the manifest.
	Handler string
	// Route is the local HTTP route prefix: the manifest's url.path, or
	// "/"+Name when unset.
	Route string
	// Backend is the manifest's local.backend hint ("auto", "container",
	// or "process"), defaulting to "auto" when unset. A bare Dockerfile
	// marker with no manifest override resolves this to "container".
	Backend string
	// Image is the container image hint, taken from the manifest's
	// local.image override. A Dockerfile-marker function with no
	// override leaves Image empty, meaning "build from the function's
	// own Dockerfile" rather than pull a named image.
	Image string
	// Manifest is the function's manifest after Load, project defaults,
	// and Lambdary's built-in defaults have all been applied — see
	// manifest.Manifest's doc comment for the layering order. Detected
	// runtime/backend hints are folded in only where the manifest and
	// project defaults left the corresponding field unset, so manifest
	// values always win over detection.
	Manifest *manifest.Manifest
	// Warnings lists non-fatal problems found for this function: route
	// collisions, duplicate names, unresolvable runtimes, and manifest
	// validation errors. Per DESIGN.md's route-collision decision,
	// discovery never fails on these — it reports them so the dev
	// server can keep serving healthy functions.
	Warnings []string
}

// marker maps a project file present in a function directory to the
// runtime it implies.
type marker struct {
	file    string
	runtime string
}

// markers is the detection table from DESIGN.md, in priority order:
// checked top to bottom, first match wins. Dockerfile and *.csproj are
// handled separately in detect, the former because it implies a
// backend/image rather than a runtime, the latter because it is a glob
// rather than a fixed filename.
var markers = []marker{
	{"package.json", "nodejs22.x"},
	{"composer.json", "provided.al2023"},
	{"pyproject.toml", "python3.13"},
	{"requirements.txt", "python3.13"},
	{"go.mod", "provided.al2023"},
	{"Gemfile", "ruby3.3"},
}

// Discover scans the direct subdirectories of root and resolves each one
// that looks like a Lambda function. cfg may be nil, in which case no
// project-wide defaults are applied. The returned slice is sorted by Name
// (then Dir, to break ties between duplicate names) for deterministic
// output. Discovery only returns a non-nil error for root-level failures
// (root missing or unreadable); per-function problems are reported as
// Warnings instead, per DESIGN.md's fail-safe decision.
func Discover(root string, cfg *manifest.Config) ([]Function, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("discovery: %s: %w", root, err)
	}

	functions := make([]Function, 0, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		fn, ok, err := discoverFunction(filepath.Join(root, entry.Name()), entry.Name(), cfg)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		functions = append(functions, fn)
	}

	sort.Slice(functions, func(i, j int) bool {
		if functions[i].Name != functions[j].Name {
			return functions[i].Name < functions[j].Name
		}
		return functions[i].Dir < functions[j].Dir
	})

	warnCollisions(functions)

	return functions, nil
}

// discoverFunction resolves a single candidate directory into a Function.
// ok is false when dir contains neither a `.lambda.yml` manifest nor a
// detectable marker file, meaning it is not a function at all.
func discoverFunction(dir, dirName string, cfg *manifest.Config) (Function, bool, error) {
	manifestPath := filepath.Join(dir, manifestFileName)
	hasManifestFile := fileExists(manifestPath)

	detectedRuntime, isContainer, err := detect(dir)
	if err != nil {
		return Function{}, false, fmt.Errorf("discovery: %s: %w", dir, err)
	}

	if !hasManifestFile && detectedRuntime == "" && !isContainer {
		return Function{}, false, nil
	}

	var warnings []string

	m := &manifest.Manifest{}
	if hasManifestFile {
		loaded, err := manifest.Load(manifestPath)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("invalid manifest: %s", err))
		} else {
			m = loaded
		}
	}

	if cfg != nil {
		cfg.ApplyDefaults(m)
	}

	// Detection-derived values fill in only what neither the manifest
	// nor the project defaults already set — manifest and project
	// defaults always win over detection. Apply before
	// ApplyBuiltinDefaults, per manifest.Manifest's layering order.
	if m.Runtime == "" && detectedRuntime != "" {
		m.Runtime = detectedRuntime
	}
	if m.Local.Backend == "" && isContainer {
		m.Local.Backend = "container"
	}

	m.ApplyBuiltinDefaults()

	name := m.Name
	if name == "" {
		name = dirName
	}

	route := m.URL.Path
	if route == "" {
		route = "/" + name
	}

	backend := m.Local.Backend
	if backend == "" {
		backend = "auto"
	}

	if m.Runtime == "" && backend != "container" {
		warnings = append(warnings, "runtime unknown")
	}

	return Function{
		Name:     name,
		Dir:      dir,
		Runtime:  m.Runtime,
		Handler:  m.Handler,
		Route:    route,
		Backend:  backend,
		Image:    m.Local.Image,
		Manifest: m,
		Warnings: warnings,
	}, true, nil
}

// detect inspects dir for the marker files in DESIGN.md's detection table
// and returns the runtime the first match implies. A bare Dockerfile takes
// priority over every other marker and reports container=true with an
// empty runtime, since a container function has no fixed runtime tag.
func detect(dir string) (runtime string, container bool, err error) {
	if fileExists(filepath.Join(dir, "Dockerfile")) {
		return "", true, nil
	}

	for _, mk := range markers {
		if fileExists(filepath.Join(dir, mk.file)) {
			return mk.runtime, false, nil
		}
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.csproj"))
	if err != nil {
		return "", false, err
	}
	if len(matches) > 0 {
		return "dotnet8", false, nil
	}

	return "", false, nil
}

// fileExists reports whether path exists and is a regular file (not a
// directory).
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// warnCollisions annotates functions in place with route-collision and
// duplicate-name warnings, naming every competing function so the caller
// knows exactly where the collision would show up. Per DESIGN.md's
// route-collision decision, these are warnings, never errors. functions
// must already be sorted, so the competitor lists in each message are in
// deterministic order.
func warnCollisions(functions []Function) {
	byRoute := make(map[string][]int)
	byName := make(map[string][]int)

	for i, fn := range functions {
		byRoute[fn.Route] = append(byRoute[fn.Route], i)
		byName[fn.Name] = append(byName[fn.Name], i)
	}

	for route, idxs := range byRoute {
		if len(idxs) < 2 {
			continue
		}

		names := make([]string, len(idxs))
		for j, idx := range idxs {
			names[j] = functions[idx].Name
		}

		msg := fmt.Sprintf("route %q is shared by functions: %s", route, strings.Join(names, ", "))
		for _, idx := range idxs {
			functions[idx].Warnings = append(functions[idx].Warnings, msg)
		}
	}

	for name, idxs := range byName {
		if len(idxs) < 2 {
			continue
		}

		dirs := make([]string, len(idxs))
		for j, idx := range idxs {
			dirs[j] = functions[idx].Dir
		}

		msg := fmt.Sprintf("duplicate function name %q: used by %s", name, strings.Join(dirs, ", "))
		for _, idx := range idxs {
			functions[idx].Warnings = append(functions[idx].Warnings, msg)
		}
	}
}
