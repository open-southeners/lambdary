package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/open-southeners/lambdary/internal/discovery"
)

// newModuleFixture lays out a Kiarga-shaped Go module rooted at root:
//
//	root/go.mod, go.sum, README.md
//	root/internal/shared/util.go   (outside every function dir)
//	root/cmd/<name>/main.go        (one function per name)
//
// and returns one Go-built discovery.Function per name.
func newModuleFixture(t *testing.T, root string, names ...string) []discovery.Function {
	t.Helper()

	files := map[string]string{
		"go.mod":                    "module example.com/m\n\ngo 1.21\n",
		"go.sum":                    "",
		"README.md":                 "docs\n",
		"internal/shared/util.go":   "package shared\n",
		"internal/shared/notes.txt": "notes\n",
	}
	for _, name := range names {
		files[filepath.Join("cmd", name, "main.go")] = "package main\n"
	}

	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", p, err)
		}
	}

	fns := make([]discovery.Function, 0, len(names))
	for _, name := range names {
		fns = append(fns, discovery.Function{
			Name: name,
			Dir:  filepath.Join(root, "cmd", name),
			Go:   &discovery.GoSource{ModuleDir: root, Package: "./cmd/" + name},
		})
	}

	return fns
}

// recvFunctions collects n non-structural Events and returns the set of
// function names seen, failing on a structural event or a duplicate.
func recvFunctions(t *testing.T, ch <-chan Event, n int) map[string]bool {
	t.Helper()

	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		ev := recvEvent(t, ch)
		if ev.Structural {
			t.Fatalf("event = %+v, want Structural=false", ev)
		}
		if seen[ev.Function] {
			t.Fatalf("duplicate event for %q", ev.Function)
		}
		seen[ev.Function] = true
	}

	return seen
}

func writeModuleFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestWatchGoModuleOwnDirChangeIsPerFunction(t *testing.T) {
	root := t.TempDir()
	fns := newModuleFixture(t, root, "ping", "notify")
	ch := startWatch(t, root, fns)

	writeModuleFile(t, filepath.Join(root, "cmd", "ping", "main.go"), "package main // edit\n")

	ev := recvEvent(t, ch)
	if ev.Structural || ev.Function != "ping" {
		t.Fatalf("event = %+v, want Function=ping Structural=false", ev)
	}

	assertNoEvent(t, ch)
}

func TestWatchGoModuleSharedGoFileFansOut(t *testing.T) {
	root := t.TempDir()
	fns := newModuleFixture(t, root, "ping", "notify")
	ch := startWatch(t, root, fns)

	writeModuleFile(t, filepath.Join(root, "internal", "shared", "util.go"), "package shared // edit\n")

	seen := recvFunctions(t, ch, 2)
	if !seen["ping"] || !seen["notify"] {
		t.Fatalf("seen = %v, want both ping and notify", seen)
	}

	assertNoEvent(t, ch)
}

func TestWatchGoModuleGoModAndGoSumFanOut(t *testing.T) {
	for _, file := range []string{"go.mod", "go.sum"} {
		t.Run(file, func(t *testing.T) {
			root := t.TempDir()
			fns := newModuleFixture(t, root, "ping", "notify")
			ch := startWatch(t, root, fns)

			writeModuleFile(t, filepath.Join(root, file), "// edit\n")

			seen := recvFunctions(t, ch, 2)
			if !seen["ping"] || !seen["notify"] {
				t.Fatalf("seen = %v, want both ping and notify", seen)
			}

			assertNoEvent(t, ch)
		})
	}
}

func TestWatchGoModuleNonGoFilesAreIgnored(t *testing.T) {
	root := t.TempDir()
	fns := newModuleFixture(t, root, "ping", "notify")
	ch := startWatch(t, root, fns)

	writeModuleFile(t, filepath.Join(root, "README.md"), "edited\n")
	writeModuleFile(t, filepath.Join(root, "internal", "shared", "notes.txt"), "edited\n")

	assertNoEvent(t, ch)
}

func TestWatchGoModuleOutsideScanRoot(t *testing.T) {
	// Module root is the parent of the scan root, as in
	// `lambdary dev --root functions` with go.mod beside lambdary.yml's
	// parent dir.
	mod := t.TempDir()
	root := filepath.Join(mod, "functions")
	fns := newModuleFixture(t, root, "ping", "notify")

	// Move go.mod above the scan root and point the functions at it.
	if err := os.Rename(filepath.Join(root, "go.mod"), filepath.Join(mod, "go.mod")); err != nil {
		t.Fatalf("Rename go.mod: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(mod, "pkg"), 0o755); err != nil {
		t.Fatalf("MkdirAll pkg: %v", err)
	}
	writeModuleFile(t, filepath.Join(mod, "pkg", "lib.go"), "package pkg\n")

	for i := range fns {
		fns[i].Go.ModuleDir = mod
	}

	ch := startWatch(t, root, fns)

	writeModuleFile(t, filepath.Join(mod, "pkg", "lib.go"), "package pkg // edit\n")

	seen := recvFunctions(t, ch, 2)
	if !seen["ping"] || !seen["notify"] {
		t.Fatalf("seen = %v, want both ping and notify", seen)
	}
}

func TestWatchGoModuleOnlyAffectsItsOwnFunctions(t *testing.T) {
	// Two modules side by side: a shared change in one must not fire the
	// other's functions.
	root := t.TempDir()
	fnsA := newModuleFixture(t, filepath.Join(root, "moda"), "a1", "a2")
	fnsB := newModuleFixture(t, filepath.Join(root, "modb"), "b1")
	ch := startWatch(t, root, append(fnsA, fnsB...))

	writeModuleFile(t, filepath.Join(root, "moda", "internal", "shared", "util.go"), "package shared // edit\n")

	seen := recvFunctions(t, ch, 2)
	if !seen["a1"] || !seen["a2"] {
		t.Fatalf("seen = %v, want a1 and a2", seen)
	}

	assertNoEvent(t, ch)
}

func TestWatchGoModuleNestedDirsLongestPrefixWins(t *testing.T) {
	// "outer" lives at cmd/outer and "inner" nests under it at
	// cmd/outer/inner; a change in inner's dir is inner's alone.
	root := t.TempDir()
	fns := newModuleFixture(t, root, "outer")
	innerDir := filepath.Join(root, "cmd", "outer", "inner")
	if err := os.MkdirAll(innerDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeModuleFile(t, filepath.Join(innerDir, "main.go"), "package main\n")
	fns = append(fns, discovery.Function{
		Name: "inner",
		Dir:  innerDir,
		Go:   &discovery.GoSource{ModuleDir: root, Package: "./cmd/outer/inner"},
	})

	ch := startWatch(t, root, fns)

	writeModuleFile(t, filepath.Join(innerDir, "main.go"), "package main // edit\n")

	ev := recvEvent(t, ch)
	if ev.Structural || ev.Function != "inner" {
		t.Fatalf("event = %+v, want Function=inner", ev)
	}

	assertNoEvent(t, ch)
}
