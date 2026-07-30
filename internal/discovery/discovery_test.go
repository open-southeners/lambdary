package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/manifest"
)

// findFunc returns the Function named name from functions, failing the
// test if it isn't present.
func findFunc(t *testing.T, functions []Function, name string) Function {
	t.Helper()

	for _, fn := range functions {
		if fn.Name == name {
			return fn
		}
	}

	t.Fatalf("function %q not found in %v", name, names(functions))
	return Function{}
}

// names extracts the Name field of every function, in slice order.
func names(functions []Function) []string {
	out := make([]string, len(functions))
	for i, fn := range functions {
		out[i] = fn.Name
	}
	return out
}

// containsWarning reports whether any warning in warnings contains substr.
func containsWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestDiscoverMarkers(t *testing.T) {
	functions, err := Discover("testdata/markers", nil)
	if err != nil {
		t.Fatalf("Discover() unexpected error: %v", err)
	}

	// Deterministic sort order: alphabetical by Name. Hidden dirs and
	// plain files at root are skipped, so .hidden and README.md are
	// absent.
	want := []string{
		"docker-app", "dotnet-app", "go-app", "nodejs-app", "php-app",
		"priority-app", "python-app", "python-reqs-app", "ruby-app",
	}
	got := names(functions)
	if len(got) != len(want) {
		t.Fatalf("Discover() returned %d functions, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Discover() order = %v, want %v", got, want)
		}
	}

	cases := []struct {
		name        string
		wantRuntime string
		wantBackend string
	}{
		{"nodejs-app", "nodejs22.x", "auto"},
		{"php-app", "provided.al2023", "auto"},
		{"python-app", "python3.13", "auto"},
		{"python-reqs-app", "python3.13", "auto"},
		{"go-app", "provided.al2023", "auto"},
		{"ruby-app", "ruby3.3", "auto"},
		{"dotnet-app", "dotnet8", "auto"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := findFunc(t, functions, tc.name)
			if fn.Runtime != tc.wantRuntime {
				t.Errorf("Runtime = %q, want %q", fn.Runtime, tc.wantRuntime)
			}
			if fn.Backend != tc.wantBackend {
				t.Errorf("Backend = %q, want %q", fn.Backend, tc.wantBackend)
			}
			if fn.Route != "/"+tc.name {
				t.Errorf("Route = %q, want %q", fn.Route, "/"+tc.name)
			}
			if len(fn.Warnings) != 0 {
				t.Errorf("Warnings = %v, want none", fn.Warnings)
			}
		})
	}

	t.Run("Dockerfile detected as a container function", func(t *testing.T) {
		fn := findFunc(t, functions, "docker-app")
		if fn.Runtime != "" {
			t.Errorf("Runtime = %q, want empty (build from Dockerfile)", fn.Runtime)
		}
		if fn.Backend != "container" {
			t.Errorf("Backend = %q, want %q", fn.Backend, "container")
		}
		if fn.Image != "" {
			t.Errorf("Image = %q, want empty (no local.image override)", fn.Image)
		}
		if len(fn.Warnings) != 0 {
			t.Errorf("Warnings = %v, want none (container backend has no unknown runtime)", fn.Warnings)
		}
	})

	t.Run("Dockerfile takes priority over other markers", func(t *testing.T) {
		fn := findFunc(t, functions, "priority-app")
		if fn.Runtime != "" {
			t.Errorf("Runtime = %q, want empty", fn.Runtime)
		}
		if fn.Backend != "container" {
			t.Errorf("Backend = %q, want %q", fn.Backend, "container")
		}
	})
}

func TestDiscoverManifest(t *testing.T) {
	functions, err := Discover("testdata/manifest", nil)
	if err != nil {
		t.Fatalf("Discover() unexpected error: %v", err)
	}

	t.Run("manifest-only function with no marker file", func(t *testing.T) {
		fn := findFunc(t, functions, "custom-name")
		if fn.Runtime != "nodejs22.x" {
			t.Errorf("Runtime = %q, want %q", fn.Runtime, "nodejs22.x")
		}
		if fn.Handler != "index.handler" {
			t.Errorf("Handler = %q, want %q", fn.Handler, "index.handler")
		}
		if fn.Route != "/custom" {
			t.Errorf("Route = %q, want %q", fn.Route, "/custom")
		}
		if len(fn.Warnings) != 0 {
			t.Errorf("Warnings = %v, want none", fn.Warnings)
		}
	})

	t.Run("manifest runtime wins over a detectable marker", func(t *testing.T) {
		fn := findFunc(t, functions, "override")
		if fn.Runtime != "python3.9" {
			t.Errorf("Runtime = %q, want %q (manifest should win over the package.json marker)", fn.Runtime, "python3.9")
		}
	})

	t.Run("lambda.yml present with no resolvable runtime warns", func(t *testing.T) {
		fn := findFunc(t, functions, "unknown-runtime")
		if fn.Runtime != "" {
			t.Errorf("Runtime = %q, want empty", fn.Runtime)
		}
		if !containsWarning(fn.Warnings, "runtime unknown") {
			t.Errorf("Warnings = %v, want one containing %q", fn.Warnings, "runtime unknown")
		}
	})

	t.Run("invalid manifest is included with a warning, not dropped", func(t *testing.T) {
		fn := findFunc(t, functions, "invalid")
		if !containsWarning(fn.Warnings, "invalid manifest") {
			t.Errorf("Warnings = %v, want one containing %q", fn.Warnings, "invalid manifest")
		}
		if !containsWarning(fn.Warnings, "local.backend") {
			t.Errorf("Warnings = %v, want the underlying validation error to be carried through", fn.Warnings)
		}
	})

	t.Run("manifest backend override wins over the Dockerfile marker", func(t *testing.T) {
		fn := findFunc(t, functions, "docker-with-manifest-backend")
		if fn.Backend != "process" {
			t.Errorf("Backend = %q, want %q (manifest should win over the Dockerfile marker)", fn.Backend, "process")
		}
		// No marker resolves a runtime once the Dockerfile's implied
		// "container" backend has been overridden away, so this is a
		// genuine "runtime unknown" case.
		if !containsWarning(fn.Warnings, "runtime unknown") {
			t.Errorf("Warnings = %v, want one containing %q", fn.Warnings, "runtime unknown")
		}
	})
}

func TestDiscoverCollisions(t *testing.T) {
	functions, err := Discover("testdata/collisions", nil)
	if err != nil {
		t.Fatalf("Discover() unexpected error: %v", err)
	}

	t.Run("route collision warns every competitor, naming all of them", func(t *testing.T) {
		a := findFunc(t, functions, "route-a")
		b := findFunc(t, functions, "route-b")

		for _, fn := range []Function{a, b} {
			if !containsWarning(fn.Warnings, `route "/shared"`) {
				t.Errorf("%s: Warnings = %v, want one naming the shared route", fn.Name, fn.Warnings)
			}
			if !containsWarning(fn.Warnings, "route-a") || !containsWarning(fn.Warnings, "route-b") {
				t.Errorf("%s: Warnings = %v, want both competitors named", fn.Name, fn.Warnings)
			}
		}
	})

	t.Run("duplicate names warn every competitor, naming all of them", func(t *testing.T) {
		dup := 0
		for _, fn := range functions {
			if fn.Name != "duplicated" {
				continue
			}
			dup++

			if !containsWarning(fn.Warnings, `duplicate function name "duplicated"`) {
				t.Errorf("Warnings = %v, want one naming the duplicate", fn.Warnings)
			}
			wantDirA := filepath.Join("testdata", "collisions", "dup-a")
			wantDirB := filepath.Join("testdata", "collisions", "dup-b")
			if !containsWarning(fn.Warnings, wantDirA) || !containsWarning(fn.Warnings, wantDirB) {
				t.Errorf("Warnings = %v, want both competitor dirs named", fn.Warnings)
			}
		}
		if dup != 2 {
			t.Fatalf("found %d functions named %q, want 2", dup, "duplicated")
		}
	})

	t.Run("deterministic order ties broken by Dir", func(t *testing.T) {
		got := names(functions)
		want := []string{"duplicated", "duplicated", "route-a", "route-b"}
		if len(got) != len(want) {
			t.Fatalf("Discover() returned %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Discover() order = %v, want %v", got, want)
			}
		}
		if functions[0].Dir >= functions[1].Dir {
			t.Errorf("tie between duplicate names not broken by Dir: %q then %q", functions[0].Dir, functions[1].Dir)
		}
	})
}

func TestDiscoverProjectDefaults(t *testing.T) {
	cfg := &manifest.Config{
		Defaults: manifest.Defaults{Runtime: "nodejs22.x"},
	}

	functions, err := Discover("testdata/defaults", cfg)
	if err != nil {
		t.Fatalf("Discover() unexpected error: %v", err)
	}

	t.Run("project default fills an unset runtime", func(t *testing.T) {
		fn := findFunc(t, functions, "inherits")
		if fn.Runtime != "nodejs22.x" {
			t.Errorf("Runtime = %q, want %q (from project defaults)", fn.Runtime, "nodejs22.x")
		}
	})

	t.Run("manifest runtime wins over the project default", func(t *testing.T) {
		fn := findFunc(t, functions, "overrides")
		if fn.Runtime != "python3.13" {
			t.Errorf("Runtime = %q, want %q (the manifest's own value)", fn.Runtime, "python3.13")
		}
	})
}

func TestDiscoverEmptyRoot(t *testing.T) {
	functions, err := Discover(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Discover() unexpected error: %v", err)
	}
	if len(functions) != 0 {
		t.Errorf("Discover() = %v, want an empty slice", functions)
	}
}

func TestDiscoverMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := Discover(missing, nil)
	if err == nil {
		t.Fatal("Discover() expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Discover() error = %v, want wrapping os.ErrNotExist", err)
	}
}

func TestDiscoverNilConfig(t *testing.T) {
	// cfg may be nil: treat as zero-value defaults, no project-wide
	// overrides applied.
	functions, err := Discover("testdata/defaults", nil)
	if err != nil {
		t.Fatalf("Discover() unexpected error: %v", err)
	}

	fn := findFunc(t, functions, "inherits")
	if fn.Runtime != "" {
		t.Errorf("Runtime = %q, want empty (no cfg, so no project default to inherit)", fn.Runtime)
	}
	if !containsWarning(fn.Warnings, "runtime unknown") {
		t.Errorf("Warnings = %v, want one containing %q", fn.Warnings, "runtime unknown")
	}
}
