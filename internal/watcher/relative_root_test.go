package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
)

// TestWatchRelativeRootInsideGoModule covers functions under a relative root,
// as `lambdary dev --root functions` gives, inside an absolutely addressed Go
// module. Each edit must reach exactly the function that owns the file, rather
// than being lost or reloading the whole module.
func TestWatchRelativeRootInsideGoModule(t *testing.T) {
	tmp := t.TempDir()

	for _, dir := range []string{"cmd/hello", "cmd/echo", "py"} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}

	t.Chdir(tmp)

	module := &discovery.GoSource{ModuleDir: tmp, Package: "./cmd/hello"}
	fns := []discovery.Function{
		{Name: "hello", Dir: "cmd/hello", Go: module},
		{Name: "echo", Dir: "cmd/echo", Go: module},
		{Name: "py", Dir: "py"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := Watch(ctx, ".", fns)
	if err != nil {
		t.Fatalf("Watch() unexpected error: %v", err)
	}

	for _, tc := range []struct{ file, want string }{
		{"py/lambda_function.py", "py"},
		{"cmd/hello/main.go", "hello"},
	} {
		if err := os.WriteFile(filepath.Join(tmp, tc.file), []byte("edited"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", tc.file, err)
		}

		select {
		case ev := <-events:
			if ev.Structural || ev.Function != tc.want {
				t.Errorf("editing %s: event = %+v, want a code change for %s", tc.file, ev, tc.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("editing %s: no event", tc.file)
		}
	}
}
