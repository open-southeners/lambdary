package discovery

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/manifest"
)

// writeTree creates every path in files (relative to root, slash
// separated) with the given content, making parent dirs as needed.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", p, err)
		}
	}
}

const goMod = "module example.com/m\n\ngo 1.21\n"

func TestDiscoverScanGoSource(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"gofn/go.mod":          goMod,
		"gofn/main.go":         "package main\n",
		"dockergo/go.mod":      goMod,
		"dockergo/Dockerfile":  "FROM scratch\n",
		"node/package.json":    "{}\n",
		"gonode/go.mod":        goMod,
		"gonode/.lambda.yml":   "runtime: nodejs22.x\n",
		"gocustom/go.mod":      goMod,
		"gocustom/.lambda.yml": "runtime: provided.al2\n",
	})

	cfg := &manifest.Config{Build: manifest.Build{Go: manifest.GoBuild{Tags: []string{"netgo"}, LDFlags: "-s -w"}}}

	functions, err := Discover(root, cfg)
	if err != nil {
		t.Fatalf("Discover() unexpected error: %v", err)
	}

	t.Run("go.mod with provided runtime gets a GoSource", func(t *testing.T) {
		fn := findFunc(t, functions, "gofn")
		want := &GoSource{ModuleDir: filepath.Join(root, "gofn"), Package: ".", Tags: []string{"netgo"}, LDFlags: "-s -w"}
		if !reflect.DeepEqual(fn.Go, want) {
			t.Errorf("Go = %#v, want %#v", fn.Go, want)
		}
	})

	t.Run("explicit provided.* runtime also qualifies", func(t *testing.T) {
		fn := findFunc(t, functions, "gocustom")
		if fn.Go == nil {
			t.Error("Go = nil, want a GoSource")
		}
	})

	t.Run("Dockerfile function is not Go-built", func(t *testing.T) {
		if fn := findFunc(t, functions, "dockergo"); fn.Go != nil {
			t.Errorf("Go = %#v, want nil", fn.Go)
		}
	})

	t.Run("non-provided runtimes are not Go-built", func(t *testing.T) {
		for _, name := range []string{"node", "gonode"} {
			if fn := findFunc(t, functions, name); fn.Go != nil {
				t.Errorf("%s: Go = %#v, want nil", name, fn.Go)
			}
		}
	})
}

func TestDiscoverExplicitGoEntries(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":                  goMod,
		"cmd/ping/main.go":        "package main\n",
		"cmd/notify/main.go":      "package main\n",
		"cmd/zeta/main.go":        "package main\n",
		"unlisted/package.json":   "{}\n",
		"sub/go.mod":              goMod,
		"sub/cmd/inner/main.go":   "package main\n",
		"selfmod/go.mod":          goMod,
		"selfmod/main.go":         "package main\n",
		"cmd/zeta/.lambda.yml":    "timeout: 7\nmemory: 256\n",
		"legacy-node/index.js":    "x\n",
		"legacy-node/.lambda.yml": "runtime: nodejs22.x\nhandler: index.handler\ntimeout: 9\n",
	})

	cfg := &manifest.Config{
		Functions: map[string]manifest.FunctionEntry{
			"zeta":   {Go: "./cmd/zeta", Manifest: manifest.Manifest{Memory: 512}},
			"ping":   {Go: "./cmd/ping"},
			"notify": {Go: "./cmd/notify", Manifest: manifest.Manifest{Timeout: 30, URL: manifest.URL{Path: "/hooks/notify"}}},
			"inner":  {Go: "./sub/cmd/inner"},
			"self":   {Go: "./selfmod"},
			"legacy": {Path: "./legacy-node", Manifest: manifest.Manifest{Timeout: 20}},
		},
		Defaults: manifest.Defaults{Timeout: 3, Memory: 64, Handler: "defhandler"},
		Build:    manifest.Build{Go: manifest.GoBuild{Tags: []string{"netgo"}, LDFlags: "-s"}},
	}

	functions, err := Discover(root, cfg)
	if err != nil {
		t.Fatalf("Discover() unexpected error: %v", err)
	}

	wantNames := []string{"inner", "legacy", "notify", "ping", "self", "zeta"}
	if got := names(functions); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("names = %v, want %v (only listed functions, sorted)", got, wantNames)
	}

	t.Run("go entry resolves module and package", func(t *testing.T) {
		fn := findFunc(t, functions, "ping")
		if fn.Dir != filepath.Join(root, "cmd", "ping") {
			t.Errorf("Dir = %q", fn.Dir)
		}
		want := &GoSource{ModuleDir: root, Package: "./cmd/ping", Tags: []string{"netgo"}, LDFlags: "-s"}
		if !reflect.DeepEqual(fn.Go, want) {
			t.Errorf("Go = %#v, want %#v", fn.Go, want)
		}
		if fn.Runtime != "provided.al2023" {
			t.Errorf("Runtime = %q, want provided.al2023", fn.Runtime)
		}
		if fn.Route != "/ping" {
			t.Errorf("Route = %q, want /ping", fn.Route)
		}
	})

	t.Run("nearest go.mod above wins", func(t *testing.T) {
		fn := findFunc(t, functions, "inner")
		if fn.Go == nil {
			t.Fatal("Go = nil")
		}
		if fn.Go.ModuleDir != filepath.Join(root, "sub") {
			t.Errorf("ModuleDir = %q, want %q", fn.Go.ModuleDir, filepath.Join(root, "sub"))
		}
		if fn.Go.Package != "./cmd/inner" {
			t.Errorf("Package = %q, want ./cmd/inner", fn.Go.Package)
		}
	})

	t.Run("go.mod in the entry dir itself gives package dot", func(t *testing.T) {
		fn := findFunc(t, functions, "self")
		if fn.Go == nil {
			t.Fatal("Go = nil")
		}
		if fn.Go.ModuleDir != filepath.Join(root, "selfmod") || fn.Go.Package != "." {
			t.Errorf("Go = %#v, want ModuleDir=selfmod Package=.", fn.Go)
		}
	})

	t.Run("handler defaults to bootstrap for go entries", func(t *testing.T) {
		fn := findFunc(t, functions, "ping")
		if fn.Handler != "bootstrap" {
			t.Errorf("Handler = %q, want bootstrap", fn.Handler)
		}
	})

	t.Run("inline fields apply and url.path sets the route", func(t *testing.T) {
		fn := findFunc(t, functions, "notify")
		if fn.Manifest.Timeout != 30 {
			t.Errorf("Timeout = %d, want 30", fn.Manifest.Timeout)
		}
		if fn.Route != "/hooks/notify" {
			t.Errorf("Route = %q, want /hooks/notify", fn.Route)
		}
	})

	t.Run("precedence: inline over dir manifest over defaults", func(t *testing.T) {
		zeta := findFunc(t, functions, "zeta")
		if zeta.Manifest.Memory != 512 {
			t.Errorf("zeta Memory = %d, want 512 (inline beats .lambda.yml)", zeta.Manifest.Memory)
		}
		if zeta.Manifest.Timeout != 7 {
			t.Errorf("zeta Timeout = %d, want 7 (.lambda.yml beats defaults)", zeta.Manifest.Timeout)
		}

		ping := findFunc(t, functions, "ping")
		if ping.Manifest.Timeout != 3 || ping.Manifest.Memory != 64 {
			t.Errorf("ping Timeout/Memory = %d/%d, want defaults 3/64", ping.Manifest.Timeout, ping.Manifest.Memory)
		}
	})

	t.Run("path entry behaves like a scanned dir, named by key", func(t *testing.T) {
		fn := findFunc(t, functions, "legacy")
		if fn.Dir != filepath.Join(root, "legacy-node") {
			t.Errorf("Dir = %q", fn.Dir)
		}
		if fn.Go != nil {
			t.Errorf("Go = %#v, want nil", fn.Go)
		}
		if fn.Runtime != "nodejs22.x" || fn.Handler != "index.handler" {
			t.Errorf("Runtime/Handler = %q/%q, want from .lambda.yml", fn.Runtime, fn.Handler)
		}
		if fn.Manifest.Timeout != 20 {
			t.Errorf("Timeout = %d, want 20 (inline beats .lambda.yml's 9)", fn.Manifest.Timeout)
		}
		if fn.Route != "/legacy" {
			t.Errorf("Route = %q, want /legacy", fn.Route)
		}
	})
}

func TestDiscoverExplicitPathDetection(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"dirname/package.json": "{}\n",
	})

	cfg := &manifest.Config{Functions: map[string]manifest.FunctionEntry{
		"keyname": {Path: "./dirname"},
	}}

	functions, err := Discover(root, cfg)
	if err != nil {
		t.Fatalf("Discover() unexpected error: %v", err)
	}
	if len(functions) != 1 {
		t.Fatalf("functions = %v, want 1", names(functions))
	}
	if functions[0].Name != "keyname" || functions[0].Runtime != "nodejs22.x" {
		t.Errorf("fn = %q/%q, want keyname/nodejs22.x (detected)", functions[0].Name, functions[0].Runtime)
	}
}

func TestDiscoverExplicitErrors(t *testing.T) {
	t.Run("missing go dir", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"go.mod": goMod})

		cfg := &manifest.Config{Functions: map[string]manifest.FunctionEntry{"ghost": {Go: "./cmd/ghost"}}}
		_, err := Discover(root, cfg)
		if err == nil {
			t.Fatal("Discover() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "functions.ghost") {
			t.Errorf("error = %v, want it to name functions.ghost", err)
		}
	})

	t.Run("missing path dir", func(t *testing.T) {
		cfg := &manifest.Config{Functions: map[string]manifest.FunctionEntry{"ghost": {Path: "./nope"}}}
		_, err := Discover(t.TempDir(), cfg)
		if err == nil {
			t.Fatal("Discover() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "functions.ghost") {
			t.Errorf("error = %v, want it to name functions.ghost", err)
		}
	})

	t.Run("go entry with no go.mod above it", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"cmd/lonely/main.go": "package main\n"})

		cfg := &manifest.Config{Functions: map[string]manifest.FunctionEntry{"lonely": {Go: "./cmd/lonely"}}}
		_, err := Discover(root, cfg)
		if err == nil {
			t.Fatal("Discover() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "functions.lonely") {
			t.Errorf("error = %v, want it to name functions.lonely", err)
		}
	})
}
