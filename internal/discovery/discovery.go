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
	// Go is set for a function built from Go source: a lambdary.yml
	// `go:` entry, or a provided.* function directory with a go.mod. The
	// backends build it into the function's bootstrap on every start.
	// Nil for every other function.
	Go *GoSource
}

// GoSource describes how to build a Go function's bootstrap.
type GoSource struct {
	// ModuleDir is the directory holding the function's go.mod.
	ModuleDir string
	// Package is the main package to build, relative to ModuleDir in
	// `go build` notation ("." or "./cmd/ping").
	Package string
	// Tags are extra build tags from lambdary.yml's build.go.tags.
	Tags []string
	// LDFlags is lambdary.yml's build.go.ldflags.
	LDFlags string
}

// goRuntime and goHandler are the runtime and handler a lambdary.yml `go:`
// entry defaults to: a Go function is a custom-runtime bootstrap binary.
const (
	goRuntime = "provided.al2023"
	goHandler = "bootstrap"
)

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
//
// When cfg lists functions explicitly (lambdary.yml's `functions:`), root
// is not scanned: exactly those functions are returned, and a listed
// directory that is missing (or a `go:` package with no go.mod above it)
// is a root-level error, since the project's own config names it.
func Discover(root string, cfg *manifest.Config) ([]Function, error) {
	if cfg != nil && len(cfg.Functions) > 0 {
		return discoverListed(root, cfg)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("discovery: %s: %w", root, err)
	}

	functions := make([]Function, 0, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		fn, ok, err := discoverFunction(filepath.Join(root, entry.Name()), entry.Name(), cfg, nil)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		functions = append(functions, fn)
	}

	return finish(functions), nil
}

// discoverListed resolves cfg.Functions, lambdary.yml's explicit function
// list, instead of scanning root. Every listed entry yields a function,
// even a `path:` directory with no manifest or marker file.
func discoverListed(root string, cfg *manifest.Config) ([]Function, error) {
	functions := make([]Function, 0, len(cfg.Functions))

	for name, entry := range cfg.Functions {
		src := entry.Path
		if entry.Go != "" {
			src = entry.Go
		}

		dir := filepath.Join(root, src)

		info, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("discovery: functions.%s: %w", name, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("discovery: functions.%s: %s is not a directory", name, dir)
		}

		e := entry
		e.Name = name

		var goSrc *GoSource
		if entry.Go != "" {
			goSrc, err = goPackage(dir)
			if err != nil {
				return nil, fmt.Errorf("discovery: functions.%s: %w", name, err)
			}

			goSrc.Tags = cfg.Build.Go.Tags
			goSrc.LDFlags = cfg.Build.Go.LDFlags
		}

		fn, _, err := discoverFunction(dir, name, cfg, &e)
		if err != nil {
			return nil, err
		}

		if goSrc != nil {
			fn.Go = goSrc
		}

		functions = append(functions, fn)
	}

	return finish(functions), nil
}

// goPackage locates the go.mod at or above pkgDir and returns the
// GoSource for building pkgDir's main package from it.
func goPackage(pkgDir string) (*GoSource, error) {
	abs, err := filepath.Abs(pkgDir)
	if err != nil {
		return nil, err
	}

	for dir := abs; ; dir = filepath.Dir(dir) {
		if fileExists(filepath.Join(dir, "go.mod")) {
			rel, err := filepath.Rel(dir, abs)
			if err != nil {
				return nil, err
			}

			pkg := "."
			if rel != "." {
				pkg = "./" + filepath.ToSlash(rel)
			}

			return &GoSource{ModuleDir: dir, Package: pkg}, nil
		}

		if filepath.Dir(dir) == dir {
			return nil, fmt.Errorf("no go.mod found at or above %s", pkgDir)
		}
	}
}

// finish sorts functions for deterministic output and annotates route and
// name collisions.
func finish(functions []Function) []Function {
	sort.Slice(functions, func(i, j int) bool {
		if functions[i].Name != functions[j].Name {
			return functions[i].Name < functions[j].Name
		}
		return functions[i].Dir < functions[j].Dir
	})

	warnCollisions(functions)

	return functions
}

// discoverFunction resolves a single candidate directory into a Function.
// ok is false when dir contains neither a `.lambda.yml` manifest nor a
// detectable marker file, meaning it is not a function at all. listed is
// the lambdary.yml functions entry dir came from (nil when scanning):
// its inline settings override the directory's own manifest, and a listed
// directory is a function whether or not it has a manifest or marker.
func discoverFunction(dir, dirName string, cfg *manifest.Config, listed *manifest.FunctionEntry) (Function, bool, error) {
	manifestPath := filepath.Join(dir, manifestFileName)
	hasManifestFile := fileExists(manifestPath)

	detectedRuntime, isContainer, err := detect(dir)
	if err != nil {
		return Function{}, false, fmt.Errorf("discovery: %s: %w", dir, err)
	}

	if listed == nil && !hasManifestFile && detectedRuntime == "" && !isContainer {
		return Function{}, false, nil
	}

	goEntry := listed != nil && listed.Go != ""

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

	if listed != nil {
		m.ApplyOverrides(listed.Manifest)
	}

	// A `go:` entry says what the function is, so its runtime and
	// handler outrank project-wide defaults (a project defaulting to
	// nodejs22.x must not turn its Go functions into Node ones); only the
	// entry itself or the package's own .lambda.yml can change them.
	if goEntry {
		if m.Runtime == "" {
			m.Runtime = goRuntime
		}
		if m.Handler == "" {
			m.Handler = goHandler
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

	// A scanned provided.* function with its own go.mod is a Go function:
	// built rather than expected to ship a hand-built bootstrap. A
	// Dockerfile function builds itself, so it is left alone.
	var goSrc *GoSource
	if listed == nil && !isContainer && strings.HasPrefix(m.Runtime, "provided.") && fileExists(filepath.Join(dir, "go.mod")) {
		goSrc = &GoSource{ModuleDir: dir, Package: "."}
		if cfg != nil {
			goSrc.Tags = cfg.Build.Go.Tags
			goSrc.LDFlags = cfg.Build.Go.LDFlags
		}
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
		Go:       goSrc,
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
