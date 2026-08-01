package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
)

// eventDeadline is the receive timeout for a single expected Event.
// Generous to absorb macOS FSEvents/kqueue latency in CI.
const eventDeadline = 3 * time.Second

// quietPeriod is how long tests wait, after an expected Event (or after
// producing input that must NOT produce one), to make sure no further
// Event shows up — long enough to clear a 300ms debounce window plus
// scheduling slack.
const quietPeriod = 800 * time.Millisecond

// startDelay is a small pause after Watch returns, before touching the
// filesystem, giving the OS watch (kqueue/inotify) time to actually attach
// before the triggering write happens.
const startDelay = 200 * time.Millisecond

// newTestFunctions creates a directory per name under root (each containing
// a placeholder file so the dir isn't empty) and returns the matching
// discovery.Function slice.
func newTestFunctions(t *testing.T, root string, names ...string) []discovery.Function {
	t.Helper()

	fns := make([]discovery.Function, 0, len(names))
	for _, name := range names {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".lambda.yml"), []byte("name: "+name+"\n"), 0o644); err != nil {
			t.Fatalf("seeding manifest for %s: %v", name, err)
		}
		fns = append(fns, discovery.Function{Name: name, Dir: dir})
	}
	return fns
}

// startWatch starts Watch over root/fns, arranges for it to be cancelled at
// test cleanup, and gives the underlying OS watch a moment to attach.
func startWatch(t *testing.T, root string, fns []discovery.Function) <-chan Event {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := Watch(ctx, root, fns)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(startDelay)

	return ch
}

// recvEvent waits up to eventDeadline for an Event, failing the test if
// none arrives.
func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()

	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("event channel closed unexpectedly")
		}
		return ev
	case <-time.After(eventDeadline):
		t.Fatalf("timed out waiting for event")
		return Event{}
	}
}

// assertNoEvent fails the test if an Event arrives on ch within
// quietPeriod.
func assertNoEvent(t *testing.T, ch <-chan Event) {
	t.Helper()

	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(quietPeriod):
	}
}

func TestWatchCodeChangeEmitsFunctionEvent(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	handlerPath := filepath.Join(root, "a", "handler.js")
	if err := os.WriteFile(handlerPath, []byte("exports.handler = () => {}"), 0o644); err != nil {
		t.Fatalf("writing handler: %v", err)
	}

	ev := recvEvent(t, ch)
	if ev.Structural {
		t.Fatalf("event = %+v, want Structural=false", ev)
	}
	if ev.Function != "a" {
		t.Fatalf("event.Function = %q, want %q", ev.Function, "a")
	}
}

func TestWatchNestedSubdirChange(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	nestedDir := filepath.Join(root, "a", "lib")
	if err := os.Mkdir(nestedDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	// Drain the (structural=false) event the mkdir itself produces before
	// writing into the freshly-watched subdirectory.
	recvEvent(t, ch)

	// Give the on-the-fly watch add time to land before writing into it.
	time.Sleep(startDelay)

	nestedFile := filepath.Join(nestedDir, "util.js")
	if err := os.WriteFile(nestedFile, []byte("module.exports = {}"), 0o644); err != nil {
		t.Fatalf("writing nested file: %v", err)
	}

	ev := recvEvent(t, ch)
	if ev.Structural {
		t.Fatalf("event = %+v, want Structural=false", ev)
	}
	if ev.Function != "a" {
		t.Fatalf("event.Function = %q, want %q", ev.Function, "a")
	}
}

func TestWatchManifestEditIsStructural(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	manifestPath := filepath.Join(root, "a", ".lambda.yml")
	if err := os.WriteFile(manifestPath, []byte("name: a\nhandler: index.handler\n"), 0o644); err != nil {
		t.Fatalf("editing manifest: %v", err)
	}

	ev := recvEvent(t, ch)
	if !ev.Structural {
		t.Fatalf("event = %+v, want Structural=true", ev)
	}
}

func TestWatchNewDirAtRootIsStructural(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	if err := os.Mkdir(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	ev := recvEvent(t, ch)
	if !ev.Structural {
		t.Fatalf("event = %+v, want Structural=true", ev)
	}
}

func TestWatchRootConfigWriteIsStructural(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	cfgPath := filepath.Join(root, "lambdary.yml")
	if err := os.WriteFile(cfgPath, []byte("port: 9000\n"), 0o644); err != nil {
		t.Fatalf("writing lambdary.yml: %v", err)
	}

	ev := recvEvent(t, ch)
	if !ev.Structural {
		t.Fatalf("event = %+v, want Structural=true", ev)
	}
}

func TestWatchBurstOfWritesCoalescesToOneEvent(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	handlerPath := filepath.Join(root, "a", "handler.js")
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(handlerPath, []byte("v"+string(rune('0'+i))), 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	ev := recvEvent(t, ch)
	if ev.Structural || ev.Function != "a" {
		t.Fatalf("event = %+v, want {Function: a, Structural: false}", ev)
	}

	assertNoEvent(t, ch)
}

func TestWatchTwoFunctionsInOneWindowEmitTwoEvents(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a", "b")
	ch := startWatch(t, root, fns)

	if err := os.WriteFile(filepath.Join(root, "a", "handler.js"), []byte("a"), 0o644); err != nil {
		t.Fatalf("writing a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b", "handler.js"), []byte("b"), 0o644); err != nil {
		t.Fatalf("writing b: %v", err)
	}

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		ev := recvEvent(t, ch)
		if ev.Structural {
			t.Fatalf("event = %+v, want Structural=false", ev)
		}
		seen[ev.Function] = true
	}

	if !seen["a"] || !seen["b"] {
		t.Fatalf("seen = %v, want both a and b", seen)
	}

	assertNoEvent(t, ch)
}

func TestWatchMixedFunctionAndManifestIsSingleStructuralEvent(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	if err := os.WriteFile(filepath.Join(root, "a", "handler.js"), []byte("code"), 0o644); err != nil {
		t.Fatalf("writing handler: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", ".lambda.yml"), []byte("name: a\n"), 0o644); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}

	ev := recvEvent(t, ch)
	if !ev.Structural {
		t.Fatalf("event = %+v, want Structural=true", ev)
	}

	assertNoEvent(t, ch)
}

func TestWatchEditorNoiseIsIgnored(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	dir := filepath.Join(root, "a")
	noise := []string{"handler.js~", ".DS_Store", "4913"}
	for _, name := range noise {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("noise"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	assertNoEvent(t, ch)
}

func TestWatchPycacheIsIgnored(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	cacheDir := filepath.Join(root, "a", "__pycache__")
	if err := os.Mkdir(cacheDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "handler.cpython-314.pyc"), []byte("bytecode"), 0o644); err != nil {
		t.Fatalf("writing pyc: %v", err)
	}

	assertNoEvent(t, ch)
}

// TestWatchNodeModulesIsIgnored creates node_modules AFTER Watch has
// already started, exercising addCreatedDir's ignored-dir skip: the mkdir
// itself is never added to the fsnotify watch tree, so a write nested
// inside it produces nothing.
func TestWatchNodeModulesIsIgnored(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	pkgDir := filepath.Join(root, "a", "node_modules", "x")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "y.js"), []byte("module.exports = {}"), 0o644); err != nil {
		t.Fatalf("writing y.js: %v", err)
	}

	assertNoEvent(t, ch)
}

// TestWatchNodeModulesTreeSkippedAtRegistration seeds a node_modules tree
// BEFORE Watch starts, exercising addTree's filepath.SkipDir path: nothing
// under node_modules is ever registered with fsnotify in the first place
// (not merely filtered after the fact by classify), so a write deep inside
// it produces nothing, while an ordinary function file still fires.
func TestWatchNodeModulesTreeSkippedAtRegistration(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")

	deepDir := filepath.Join(root, "a", "node_modules", "pkg", "lib")
	if err := os.MkdirAll(deepDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deepDir, "index.js"), []byte("module.exports = {}"), 0o644); err != nil {
		t.Fatalf("seeding node_modules file: %v", err)
	}

	ch := startWatch(t, root, fns)

	if err := os.WriteFile(filepath.Join(deepDir, "x.js"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("writing x.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "main.js"), []byte("main"), 0o644); err != nil {
		t.Fatalf("writing main.js: %v", err)
	}

	ev := recvEvent(t, ch)
	if ev.Structural || ev.Function != "a" {
		t.Fatalf("event = %+v, want {Function: a, Structural: false}", ev)
	}

	assertNoEvent(t, ch)
}

// TestWatchLegitNestedChangeStillFires guards against the __pycache__ and
// node_modules ignore rules above over-matching: an ordinary nested source
// file must still produce a code-change Event.
func TestWatchLegitNestedChangeStillFires(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")
	ch := startWatch(t, root, fns)

	libDir := filepath.Join(root, "a", "lib")
	if err := os.Mkdir(libDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	// Drain the mkdir's own (legitimate) code-change event before writing
	// into the freshly-watched subdirectory.
	ev := recvEvent(t, ch)
	if ev.Structural || ev.Function != "a" {
		t.Fatalf("mkdir event = %+v, want {Function: a, Structural: false}", ev)
	}

	// Give the on-the-fly watch add time to land before writing into it.
	time.Sleep(startDelay)

	if err := os.WriteFile(filepath.Join(libDir, "util.py"), []byte("def handler(): pass"), 0o644); err != nil {
		t.Fatalf("writing util.py: %v", err)
	}

	ev = recvEvent(t, ch)
	if ev.Structural || ev.Function != "a" {
		t.Fatalf("event = %+v, want {Function: a, Structural: false}", ev)
	}
}

func TestWatchContextCancelClosesChannel(t *testing.T) {
	root := t.TempDir()
	fns := newTestFunctions(t, root, "a")

	ctx, cancel := context.WithCancel(context.Background())

	ch, err := Watch(ctx, root, fns)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	time.Sleep(startDelay)

	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("expected channel to close, got an event instead")
		}
	case <-time.After(eventDeadline):
		t.Fatalf("timed out waiting for channel to close")
	}
}
