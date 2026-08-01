// Package watcher watches a project's function tree for filesystem changes
// and reports them as coalesced, classified Events for hot reload, per
// DESIGN.md's "Hot reload" section and plans/m4-dx.md Unit A.
package watcher

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/open-southeners/lambdary/internal/discovery"
)

// debounceWindow is the quiet period Watch waits after the last relevant
// filesystem event before emitting the coalesced Events for that window,
// per DESIGN.md's hot reload section.
const debounceWindow = 300 * time.Millisecond

// rootConfigFileName is the project-wide config watched at root for
// structural changes.
const rootConfigFileName = "lambdary.yml"

// manifestFileName is the per-function manifest watched throughout every
// function dir for structural changes. Mirrors internal/discovery's
// constant of the same name and value.
const manifestFileName = ".lambda.yml"

// vimProbeFile is the zero-byte file vim creates and immediately removes to
// check whether a filesystem preserves arbitrary permission bits. It is
// well-known editor noise, alongside the `~`/`.swp`/`.swx`/`.tmp` patterns.
const vimProbeFile = "4913"

// Event is a coalesced filesystem change Watch reports once its debounce
// window closes.
type Event struct {
	// Function is the name of the function whose code changed. Empty when
	// Structural is true.
	Function string
	// Structural is true when the project's shape changed — the root
	// config, a function manifest, or a direct child directory of root was
	// added, removed, or edited — meaning the caller should re-run
	// discovery rather than restart a single function.
	Structural bool
	// Paths lists every filesystem path that contributed to this Event, in
	// the order Watch observed them. May contain duplicates when the same
	// path changed more than once inside the debounce window.
	Paths []string
}

// Watch watches root (for `lambdary.yml` and direct child directory
// add/remove/rename) and every function in fns recursively (fsnotify is
// non-recursive, so Watch walks each function dir up front and adds newly
// created subdirectories as they appear). Filesystem events are debounced
// over a 300ms window and coalesced into Events per the package doc:
// a structural change anywhere in the window wins and collapses the whole
// window into one Structural Event; otherwise one Event is emitted per
// distinct function whose code changed.
//
// The returned channel is closed, and the underlying fsnotify watcher shut
// down, when ctx is done. Callers must keep draining the channel until it
// closes to avoid leaking the watcher goroutine.
func Watch(ctx context.Context, root string, fns []discovery.Function) (<-chan Event, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watcher: %w", err)
	}

	root = filepath.Clean(root)

	if err := fsw.Add(root); err != nil {
		fsw.Close()
		return nil, fmt.Errorf("watcher: %s: %w", root, err)
	}

	rootChildren, err := rootChildDirs(root)
	if err != nil {
		fsw.Close()
		return nil, fmt.Errorf("watcher: %s: %w", root, err)
	}

	fnDirs := make(map[string]string, len(fns))
	for _, fn := range fns {
		dir := filepath.Clean(fn.Dir)
		fnDirs[dir] = fn.Name

		if err := addTree(fsw, dir); err != nil {
			fsw.Close()
			return nil, fmt.Errorf("watcher: %s: %w", dir, err)
		}
	}

	out := make(chan Event, 8)

	go run(ctx, fsw, root, fnDirs, rootChildren, out)

	return out, nil
}

// rootChildDirs lists the direct child directories of root at watch start,
// so later Remove/Rename events at that level can be recognized as
// directory removals (and thus Structural) even though the removed entry
// no longer exists to be stat'd.
func rootChildDirs(root string) (map[string]bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", root, err)
	}

	children := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			children[filepath.Join(root, entry.Name())] = true
		}
	}

	return children, nil
}

// addTree adds dir and every subdirectory beneath it to fsw, since
// fsnotify only watches the directories it is explicitly told about.
// Subtrees rooted at a directory named in ignoredDirNames are skipped
// entirely (filepath.SkipDir), so a real node_modules or __pycache__ tree
// never burns thousands of watch handles for directories whose contents
// classify would ignore anyway.
func addTree(fsw *fsnotify.Watcher, dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if ignoredDirNames[d.Name()] {
				return filepath.SkipDir
			}
			if err := fsw.Add(path); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		return nil
	})
}

// run owns fsw and all watcher state for the lifetime of Watch: it
// classifies incoming events, extends the watch tree on the fly when new
// function subdirectories appear, and debounces classified events into
// Events on out. Everything here runs on a single goroutine, so the
// debounce state (pending, timer) needs no locking and can never race with
// itself.
func run(ctx context.Context, fsw *fsnotify.Watcher, root string, fnDirs map[string]string, rootChildren map[string]bool, out chan<- Event) {
	defer close(out)
	defer fsw.Close()

	var pending pendingWindow
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	var timerC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-fsw.Events:
			if !ok {
				return
			}

			if ev.Op.Has(fsnotify.Create) {
				addCreatedDir(fsw, ev.Name, root, fnDirs)
			}

			structural, fn, ignore := classify(ev, root, fnDirs, rootChildren)
			if ignore {
				continue
			}

			pending.add(structural, fn, ev.Name)

			if timer == nil {
				timer = time.NewTimer(debounceWindow)
			} else if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
				timer.Reset(debounceWindow)
			} else {
				timer.Reset(debounceWindow)
			}
			timerC = timer.C

		case <-timerC:
			pending.flush(out)
			pending = pendingWindow{}
			timerC = nil

		case _, ok := <-fsw.Errors:
			if !ok {
				return
			}
			// fsnotify surfaces internal watch errors (e.g. a directory
			// removed out from under an active watch). Watch has no error
			// channel of its own, and a single misbehaving entry
			// shouldn't kill the whole watch loop, so these are dropped.
		}
	}
}

// addCreatedDir extends the watch tree when ev's Create event is a new
// directory somewhere inside an already-watched function tree, so files
// written into it are picked up without a restart. Directly created root
// children are handled separately by classifyRootChild — they become
// Structural events that a caller is expected to react to by re-resolving
// and calling Watch again with the updated function list. A newly created
// ignored dir (node_modules, __pycache__, ...) is skipped rather than
// watched, matching addTree's initial-registration behavior.
func addCreatedDir(fsw *fsnotify.Watcher, path, root string, fnDirs map[string]string) {
	path = filepath.Clean(path)

	if filepath.Dir(path) == root {
		return
	}

	if inIgnoredDir(path) {
		return
	}

	if _, ok := underFunctionDir(path, fnDirs); !ok {
		return
	}

	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return
	}

	_ = addTree(fsw, path)
}

// classify determines whether ev represents a structural change, a code
// change for a known function, or noise to ignore.
func classify(ev fsnotify.Event, root string, fnDirs map[string]string, rootChildren map[string]bool) (structural bool, fn string, ignore bool) {
	path := filepath.Clean(ev.Name)
	base := filepath.Base(path)

	// Build/cache subtrees are skipped entirely, before any other check
	// (including the manifest check below), since nothing inside them —
	// however named — is a meaningful project change.
	if inIgnoredDir(path) {
		return false, "", true
	}

	if isEditorNoise(base) {
		return false, "", true
	}

	// The manifest and root config checks run before the dotfile ignore
	// below, since .lambda.yml is itself a dotfile.
	if base == manifestFileName {
		return true, "", false
	}
	if base == rootConfigFileName && filepath.Dir(path) == root {
		return true, "", false
	}

	if strings.HasPrefix(base, ".") {
		return false, "", true
	}

	if filepath.Dir(path) == root {
		return classifyRootChild(ev, path, rootChildren)
	}

	if name, ok := underFunctionDir(path, fnDirs); ok {
		return false, name, false
	}

	return false, "", true
}

// classifyRootChild classifies an event whose path is a direct child of
// root: a create, remove, or rename of a directory there is Structural
// (per DESIGN.md, since it may add or remove a function); anything else
// (writes to, or creation/removal of, plain files other than lambdary.yml)
// is ignored.
func classifyRootChild(ev fsnotify.Event, path string, rootChildren map[string]bool) (structural bool, fn string, ignore bool) {
	if ev.Op.Has(fsnotify.Create) {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			rootChildren[path] = true
			return true, "", false
		}
		return false, "", true
	}

	if ev.Op.Has(fsnotify.Remove) || ev.Op.Has(fsnotify.Rename) {
		if rootChildren[path] {
			delete(rootChildren, path)
			return true, "", false
		}
		return false, "", true
	}

	return false, "", true
}

// underFunctionDir reports whether path is inside (or is) one of fnDirs'
// directories, returning that function's name.
func underFunctionDir(path string, fnDirs map[string]string) (string, bool) {
	for dir, name := range fnDirs {
		if path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)) {
			return name, true
		}
	}
	return "", false
}

// ignoredDirNames are well-known build/cache directory names whose entire
// subtree is skipped regardless of what's inside — e.g. Python writing its
// __pycache__ after a function's first invocation is not a code change.
// .git and .lambdary are already caught by the dotfile check below at their
// own top level, but that check only inspects a path's basename, so this
// segment-based check is what protects paths nested inside them too.
var ignoredDirNames = map[string]bool{
	"__pycache__":  true,
	"node_modules": true,
	".git":         true,
	".lambdary":    true,
}

// inIgnoredDir reports whether any segment of path names a directory in
// ignoredDirNames.
func inIgnoredDir(path string) bool {
	for _, seg := range strings.Split(path, string(filepath.Separator)) {
		if ignoredDirNames[seg] {
			return true
		}
	}
	return false
}

// editorNoiseSuffixes are filename suffixes that mark common editor
// temp/swap files and compiled artifacts, ignored so they never trigger a
// reload.
var editorNoiseSuffixes = []string{"~", ".swp", ".swx", ".tmp", ".pyc"}

// isEditorNoise reports whether base names a well-known editor temp file or
// compiled artifact.
func isEditorNoise(base string) bool {
	if base == vimProbeFile {
		return true
	}
	for _, suffix := range editorNoiseSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// pendingWindow accumulates classified events during a single debounce
// window before flush emits the coalesced Event(s) for it.
type pendingWindow struct {
	structural      bool
	structuralPaths []string
	functions       map[string][]string
}

// add records path's classification into the window.
func (p *pendingWindow) add(structural bool, fn, path string) {
	if structural {
		p.structural = true
		p.structuralPaths = append(p.structuralPaths, path)
		return
	}

	if p.functions == nil {
		p.functions = make(map[string][]string)
	}
	p.functions[fn] = append(p.functions[fn], path)
}

// flush emits the window's coalesced Event(s): a single Structural Event
// when any structural change was observed, even mixed with function code
// changes (structural wins); otherwise one Event per distinct function
// whose code changed, in a deterministic (sorted by name) order.
func (p *pendingWindow) flush(out chan<- Event) {
	if p.structural {
		paths := p.structuralPaths
		for _, fnPaths := range p.functions {
			paths = append(paths, fnPaths...)
		}
		out <- Event{Structural: true, Paths: paths}
		return
	}

	names := make([]string, 0, len(p.functions))
	for name := range p.functions {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		out <- Event{Function: name, Paths: p.functions[name]}
	}
}
