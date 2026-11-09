package versions

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// newFn makes a function directory with one handler file.
func newFn(t *testing.T, name string) discovery.Function {
	t.Helper()

	dir := filepath.Join(t.TempDir(), name)
	writeFile(t, filepath.Join(dir, "index.js"), "v1", 0o644)

	return discovery.Function{
		Name:     name,
		Dir:      dir,
		Runtime:  "nodejs22.x",
		Handler:  "index.handler",
		Backend:  "process",
		Manifest: &manifest.Manifest{Timeout: 5},
	}
}

func publish(t *testing.T, s *Store, fn discovery.Function) Version {
	t.Helper()

	v, err := s.Publish(context.Background(), fn, PublishInput{}, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	return v
}

func codeDir(s *Store, name, version string) string {
	return filepath.Join(s.functionDir(name), version, "code")
}

func TestPublishNumbersAndDedup(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "f")

	v1 := publish(t, s, fn)
	if v1.Version != "1" {
		t.Fatalf("first version = %q, want 1", v1.Version)
	}

	again := publish(t, s, fn)
	if again.Version != "1" || again.RevisionID != v1.RevisionID {
		t.Errorf("unchanged publish = %+v, want version 1 returned", again)
	}

	list, err := s.List("f")
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v; want 1 version", list, err)
	}

	// Changed code.
	writeFile(t, filepath.Join(fn.Dir, "index.js"), "v2", 0o644)
	v2 := publish(t, s, fn)
	if v2.Version != "2" {
		t.Errorf("after code change version = %q, want 2", v2.Version)
	}

	// Changed manifest config.
	fn.Manifest = &manifest.Manifest{Timeout: 9}
	v3 := publish(t, s, fn)
	if v3.Version != "3" {
		t.Errorf("after config change version = %q, want 3", v3.Version)
	}

	// Changed environment alone.
	fn.Manifest = &manifest.Manifest{Timeout: 9, Environment: map[string]string{"A": "1"}}
	if v4 := publish(t, s, fn); v4.Version != "4" {
		t.Errorf("after env change version = %q, want 4", v4.Version)
	}
}

func TestPublishNumbersNeverReused(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "f")

	publish(t, s, fn)
	writeFile(t, filepath.Join(fn.Dir, "index.js"), "v2", 0o644)
	publish(t, s, fn)

	// Going back to the v1 code is a new version, not a reuse of 1.
	writeFile(t, filepath.Join(fn.Dir, "index.js"), "v1", 0o644)
	if v := publish(t, s, fn); v.Version != "3" {
		t.Errorf("version = %q, want 3", v.Version)
	}
}

func TestPublishSnapshotImmutable(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "f")

	v := publish(t, s, fn)

	writeFile(t, filepath.Join(fn.Dir, "index.js"), "changed", 0o644)
	writeFile(t, filepath.Join(fn.Dir, "new.js"), "new", 0o644)

	if got := readFile(t, filepath.Join(codeDir(s, "f", v.Version), "index.js")); got != "v1" {
		t.Errorf("snapshot index.js = %q, want v1", got)
	}
	if _, err := os.Stat(filepath.Join(codeDir(s, "f", v.Version), "new.js")); !os.IsNotExist(err) {
		t.Errorf("new.js leaked into snapshot: %v", err)
	}
}

func TestPublishExcludesAndPreservesModes(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "f")

	writeFile(t, filepath.Join(fn.Dir, ".git", "config"), "x", 0o644)
	writeFile(t, filepath.Join(fn.Dir, "__pycache__", "a.pyc"), "x", 0o644)
	writeFile(t, filepath.Join(fn.Dir, "sub", "__pycache__", "b.pyc"), "x", 0o644)
	writeFile(t, filepath.Join(fn.Dir, ".lambda.yml"), "timeout: 5", 0o644)
	writeFile(t, filepath.Join(fn.Dir, "run.sh"), "#!/bin/sh", 0o755)
	writeFile(t, filepath.Join(fn.Dir, "sub", "keep.txt"), "k", 0o644)

	if err := os.Symlink("index.js", filepath.Join(fn.Dir, "link.js")); err != nil {
		t.Fatal(err)
	}

	v := publish(t, s, fn)
	code := codeDir(s, "f", v.Version)

	for _, gone := range []string{".git", "__pycache__", "sub/__pycache__", ".lambda.yml"} {
		if _, err := os.Lstat(filepath.Join(code, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be excluded (err = %v)", gone, err)
		}
	}

	for _, kept := range []string{"index.js", "sub/keep.txt", "run.sh"} {
		if _, err := os.Stat(filepath.Join(code, kept)); err != nil {
			t.Errorf("%s missing: %v", kept, err)
		}
	}

	info, err := os.Stat(filepath.Join(code, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("run.sh lost its exec bit: %v", info.Mode())
	}

	linfo, err := os.Lstat(filepath.Join(code, "link.js"))
	if err != nil {
		t.Fatal(err)
	}
	if linfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.js is not a symlink: %v", linfo.Mode())
	}
	if target, _ := os.Readlink(filepath.Join(code, "link.js")); target != "index.js" {
		t.Errorf("symlink target = %q, want index.js", target)
	}
}

func TestPublishExecBitChangesHash(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "f")

	writeFile(t, filepath.Join(fn.Dir, "run.sh"), "x", 0o644)
	publish(t, s, fn)

	if err := os.Chmod(filepath.Join(fn.Dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if v := publish(t, s, fn); v.Version != "2" {
		t.Errorf("exec bit change gave version %q, want 2", v.Version)
	}
}

func TestPublishCodeSha256Shape(t *testing.T) {
	s := NewStore(t.TempDir())
	v := publish(t, s, newFn(t, "f"))

	raw, err := base64.StdEncoding.DecodeString(v.CodeSha256)
	if err != nil {
		t.Fatalf("CodeSha256 %q is not base64: %v", v.CodeSha256, err)
	}
	if len(raw) != 32 {
		t.Errorf("CodeSha256 decodes to %d bytes, want 32", len(raw))
	}
	if v.CodeSize != 2 {
		t.Errorf("CodeSize = %d, want 2", v.CodeSize)
	}
}

func TestPublishPreconditions(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "f")
	ctx := context.Background()

	if _, err := s.Publish(ctx, fn, PublishInput{CodeSha256: "nope"}, nil); !errors.Is(err, ErrPrecondition) {
		t.Errorf("bad CodeSha256 err = %v, want ErrPrecondition", err)
	}
	if _, err := s.Publish(ctx, fn, PublishInput{RevisionID: "nope"}, nil); !errors.Is(err, ErrPrecondition) {
		t.Errorf("bad RevisionID err = %v, want ErrPrecondition", err)
	}

	if list, _ := s.List("f"); len(list) != 0 {
		t.Errorf("failed preconditions created versions: %v", list)
	}

	// The right values pass.
	probe := publish(t, s, fn)
	rev, err := s.LatestRevision("f")
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(fn.Dir, "index.js"), "v2", 0o644)

	if _, err := s.Publish(ctx, fn, PublishInput{RevisionID: rev}, nil); err != nil {
		t.Errorf("matching RevisionID: %v", err)
	}

	// Stale revision after the publish above.
	if _, err := s.Publish(ctx, fn, PublishInput{RevisionID: rev}, nil); !errors.Is(err, ErrPrecondition) {
		t.Errorf("stale RevisionID err = %v, want ErrPrecondition", err)
	}

	if _, err := s.Publish(ctx, fn, PublishInput{CodeSha256: probe.CodeSha256}, nil); err != nil {
		// v1's code differs from the current v2 source, so this must fail.
		if !errors.Is(err, ErrPrecondition) {
			t.Errorf("err = %v, want ErrPrecondition", err)
		}
	} else {
		t.Error("CodeSha256 of a different tree was accepted")
	}
}

func TestPublishGoFunction(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "g")
	fn.Go = &discovery.GoSource{ModuleDir: fn.Dir, Package: "."}
	writeFile(t, filepath.Join(fn.Dir, "main.go"), "package main", 0o644)

	if _, err := s.Publish(context.Background(), fn, PublishInput{}, nil); err == nil {
		t.Fatal("nil build for a Go function should error")
	}

	if list, _ := s.List("g"); len(list) != 0 {
		t.Errorf("failed publish left versions: %v", list)
	}

	build := func(_ context.Context, _ discovery.Function, out string) error {
		return os.WriteFile(out, []byte("BIN"), 0o755)
	}

	v, err := s.Publish(context.Background(), fn, PublishInput{}, build)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(codeDir(s, "g", v.Version))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "bootstrap" {
		t.Errorf("code/ = %v, want only bootstrap", entries)
	}
	if got := readFile(t, filepath.Join(codeDir(s, "g", v.Version), "bootstrap")); got != "BIN" {
		t.Errorf("bootstrap = %q", got)
	}
}

func TestPublishGoWithDockerfileIsNotCompiled(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "g")
	fn.Go = &discovery.GoSource{ModuleDir: fn.Dir, Package: "."}
	writeFile(t, filepath.Join(fn.Dir, "Dockerfile"), "FROM scratch", 0o644)

	called := false
	build := func(context.Context, discovery.Function, string) error {
		called = true
		return nil
	}

	v, err := s.Publish(context.Background(), fn, PublishInput{}, build)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("build was called despite a Dockerfile")
	}
	if _, err := os.Stat(filepath.Join(codeDir(s, "g", v.Version), "Dockerfile")); err != nil {
		t.Errorf("Dockerfile not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(codeDir(s, "g", v.Version), "index.js")); err != nil {
		t.Errorf("source not copied: %v", err)
	}
}

func TestPublishEnvFile(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "f")
	writeFile(t, filepath.Join(fn.Dir, ".env"), "A=fromfile\nB=onlyfile\n", 0o644)
	fn.Manifest = &manifest.Manifest{
		Environment: map[string]string{"A": "manifest", "C": "c"},
		Local:       manifest.Local{EnvFile: ".env"},
	}

	v := publish(t, s, fn)

	env := v.Manifest.Environment
	if env["A"] != "manifest" || env["B"] != "onlyfile" || env["C"] != "c" {
		t.Errorf("Environment = %v", env)
	}
	if v.Manifest.Local.EnvFile != "" {
		t.Errorf("EnvFile = %q, want cleared", v.Manifest.Local.EnvFile)
	}

	// The caller's manifest is not mutated.
	if fn.Manifest.Local.EnvFile != ".env" {
		t.Errorf("input manifest EnvFile was cleared")
	}
}

func TestPublishMissingEnvFile(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "f")
	fn.Manifest = &manifest.Manifest{Local: manifest.Local{EnvFile: "nope.env"}}

	if _, err := s.Publish(context.Background(), fn, PublishInput{}, nil); err == nil {
		t.Fatal("missing env_file should error")
	}
	assertNoPublishLeftovers(t, s, "f")
}

func assertNoPublishLeftovers(t *testing.T, s *Store, name string) {
	t.Helper()

	if list, _ := s.List(name); len(list) != 0 {
		t.Errorf("failed publish left versions: %v", list)
	}

	entries, err := os.ReadDir(s.functionDir(name))
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".publish-") {
			t.Errorf("leftover temp dir %s", e.Name())
		}
		if e.IsDir() && e.Name() == "1" {
			t.Errorf("leftover version dir 1")
		}
	}
}

func TestPublishFailureLeavesNothing(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "g")
	fn.Go = &discovery.GoSource{ModuleDir: fn.Dir, Package: "."}

	build := func(context.Context, discovery.Function, string) error {
		return errors.New("compile failed")
	}

	if _, err := s.Publish(context.Background(), fn, PublishInput{}, build); err == nil {
		t.Fatal("expected build error")
	}

	assertNoPublishLeftovers(t, s, "g")

	// And the next publish still gets number 1.
	build = func(_ context.Context, _ discovery.Function, out string) error {
		return os.WriteFile(out, []byte("x"), 0o755)
	}
	v, err := s.Publish(context.Background(), fn, PublishInput{}, build)
	if err != nil || v.Version != "1" {
		t.Errorf("publish after failure = %v, %v; want version 1", v.Version, err)
	}
}

func TestPublishFreezesLayers(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "f")
	writeFile(t, filepath.Join(fn.Dir, "mylayer", "lib", "dep.txt"), "dep1", 0o644)
	fn.Manifest = &manifest.Manifest{Layers: []string{"mylayer"}}

	v := publish(t, s, fn)

	if len(v.Manifest.Layers) != 1 || v.Manifest.Layers[0] != "../layers" {
		t.Fatalf("Layers = %v, want [../layers]", v.Manifest.Layers)
	}

	// Resolved the way a backend does: relative to the code dir.
	frozen := filepath.Join(codeDir(s, "f", v.Version), v.Manifest.Layers[0])

	var found string
	_ = filepath.WalkDir(frozen, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "dep.txt" {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatalf("dep.txt not found under %s", frozen)
	}

	writeFile(t, filepath.Join(fn.Dir, "mylayer", "lib", "dep.txt"), "dep2", 0o644)

	if got := readFile(t, found); got != "dep1" {
		t.Errorf("frozen layer changed to %q", got)
	}

	// The input manifest still names the source layer.
	if fn.Manifest.Layers[0] != "mylayer" {
		t.Errorf("input manifest Layers mutated: %v", fn.Manifest.Layers)
	}
}

func TestStateSurvivesNewStore(t *testing.T) {
	cache := t.TempDir()
	s := NewStore(cache)
	fn := newFn(t, "f")

	publish(t, s, fn)
	if _, err := s.CreateAlias("f", "live", "1", "d", nil); err != nil {
		t.Fatal(err)
	}

	s2 := NewStore(cache)

	list, err := s2.List("f")
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if a, err := s2.GetAlias("f", "live"); err != nil || a.Version != "1" {
		t.Errorf("GetAlias = %+v, %v", a, err)
	}

	writeFile(t, filepath.Join(fn.Dir, "index.js"), "v2", 0o644)
	if v := publish(t, s2, fn); v.Version != "2" {
		t.Errorf("next version = %q, want 2", v.Version)
	}
}

// aliasFixture publishes three versions of "f".
func aliasFixture(t *testing.T) *Store {
	t.Helper()

	s := NewStore(t.TempDir())
	fn := newFn(t, "f")

	for _, c := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(fn.Dir, "index.js"), c, 0o644)
		publish(t, s, fn)
	}

	return s
}

func TestCreateAliasValidation(t *testing.T) {
	long := strings.Repeat("a", 129)

	tests := []struct {
		name    string
		alias   string
		version string
		weights map[string]float64
		want    error
	}{
		{"ok", "live", "1", nil, nil},
		{"ok latest", "dev", Latest, nil, nil},
		{"ok with underscore dash digit", "a-b_1", "1", nil, nil},
		{"ok canary", "canary", "1", map[string]float64{"2": 0.5}, nil},
		{"ok weight 0", "w0", "1", map[string]float64{"2": 0}, nil},
		{"ok weight 1", "w1", "1", map[string]float64{"2": 1}, nil},
		{"ok 128 chars", strings.Repeat("a", 128), "1", nil, nil},
		{"empty name", "", "1", nil, ErrInvalid},
		{"too long", long, "1", nil, ErrInvalid},
		{"all digits", "123", "1", nil, ErrInvalid},
		{"bad char", "a b", "1", nil, ErrInvalid},
		{"dot", "a.b", "1", nil, ErrInvalid},
		{"colon", "a:b", "1", nil, ErrInvalid},
		{"empty version", "x", "", nil, ErrInvalid},
		{"non numeric version", "x", "abc", nil, ErrInvalid},
		{"unknown version", "x", "9", nil, ErrNotFound},
		{"latest with weights", "x", Latest, map[string]float64{"1": 0.1}, ErrInvalid},
		{"two weights", "x", "1", map[string]float64{"2": 0.1, "3": 0.1}, ErrInvalid},
		{"canary equals primary", "x", "1", map[string]float64{"1": 0.1}, ErrInvalid},
		{"weight negative", "x", "1", map[string]float64{"2": -0.1}, ErrInvalid},
		{"weight above 1", "x", "1", map[string]float64{"2": 1.1}, ErrInvalid},
		{"unknown canary", "x", "1", map[string]float64{"9": 0.1}, ErrNotFound},
		{"latest as canary", "x", "1", map[string]float64{Latest: 0.1}, ErrInvalid},
		{"alias as canary", "x", "1", map[string]float64{"live": 0.1}, ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := aliasFixture(t)

			a, err := s.CreateAlias("f", tt.alias, tt.version, "desc", tt.weights)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if a.RevisionID == "" || a.Name != tt.alias || a.Version != tt.version {
					t.Errorf("alias = %+v", a)
				}
				return
			}

			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if _, gerr := s.GetAlias("f", tt.alias); !errors.Is(gerr, ErrNotFound) {
				t.Errorf("failed create left an alias (GetAlias err = %v)", gerr)
			}
		})
	}
}

func TestCreateAliasDuplicateAndUnknownFunction(t *testing.T) {
	s := aliasFixture(t)

	if _, err := s.CreateAlias("f", "live", "1", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAlias("f", "live", "2", "", nil); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate err = %v, want ErrConflict", err)
	}
	if _, err := s.GetAlias("f", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown alias err = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateAlias("f", "nope", AliasUpdate{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown alias err = %v, want ErrNotFound", err)
	}
	// A function that never published has no versions to point at.
	if _, err := s.CreateAlias("ghost", "live", "1", "", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("alias on unpublished function err = %v, want ErrNotFound", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestUpdateAlias(t *testing.T) {
	s := aliasFixture(t)

	orig, err := s.CreateAlias("f", "live", "1", "first", map[string]float64{"2": 0.25})
	if err != nil {
		t.Fatal(err)
	}

	// An empty update keeps everything, including the canary.
	a, err := s.UpdateAlias("f", "live", AliasUpdate{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != "1" || a.Description != "first" || a.CanaryVersion != "2" || a.CanaryWeight != 0.25 {
		t.Errorf("no-op update changed alias: %+v", a)
	}
	if a.RevisionID == orig.RevisionID {
		t.Error("RevisionID did not change after update")
	}

	// Description only.
	a, err = s.UpdateAlias("f", "live", AliasUpdate{Description: ptr("second")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Description != "second" || a.Version != "1" || a.CanaryVersion != "2" {
		t.Errorf("description update: %+v", a)
	}

	// Version only keeps the canary.
	a, err = s.UpdateAlias("f", "live", AliasUpdate{Version: ptr("3")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != "3" || a.CanaryVersion != "2" || a.Description != "second" {
		t.Errorf("version update: %+v", a)
	}

	// Moving the primary onto the canary is rejected, and leaves the alias.
	if _, err := s.UpdateAlias("f", "live", AliasUpdate{Version: ptr("2")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("primary==canary err = %v, want ErrInvalid", err)
	}
	if got, _ := s.GetAlias("f", "live"); got.Version != "3" {
		t.Errorf("rejected update changed alias: %+v", got)
	}

	// Empty non-nil weights clear the canary.
	empty := map[string]float64{}
	a, err = s.UpdateAlias("f", "live", AliasUpdate{Weights: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if a.CanaryVersion != "" || a.CanaryWeight != 0 {
		t.Errorf("canary not cleared: %+v", a)
	}

	// Setting weights.
	w := map[string]float64{"1": 0.5}
	a, err = s.UpdateAlias("f", "live", AliasUpdate{Weights: &w})
	if err != nil || a.CanaryVersion != "1" || a.CanaryWeight != 0.5 {
		t.Errorf("set weights = %+v, %v", a, err)
	}

	// Pointing at $LATEST with a canary still configured is invalid.
	if _, err := s.UpdateAlias("f", "live", AliasUpdate{Version: ptr(Latest)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("latest with canary err = %v, want ErrInvalid", err)
	}
	// ...but fine when the canary is cleared in the same update.
	if _, err := s.UpdateAlias("f", "live", AliasUpdate{Version: ptr(Latest), Weights: &empty}); err != nil {
		t.Errorf("latest with cleared canary: %v", err)
	}
}

func TestUpdateAliasRevisionPrecondition(t *testing.T) {
	s := aliasFixture(t)

	a, err := s.CreateAlias("f", "live", "1", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateAlias("f", "live", AliasUpdate{RevisionID: "wrong", Description: ptr("x")}); !errors.Is(err, ErrPrecondition) {
		t.Errorf("err = %v, want ErrPrecondition", err)
	}
	if got, _ := s.GetAlias("f", "live"); got.Description != "" {
		t.Error("precondition failure still applied the update")
	}

	a2, err := s.UpdateAlias("f", "live", AliasUpdate{RevisionID: a.RevisionID, Description: ptr("x")})
	if err != nil {
		t.Fatal(err)
	}

	// The old revision is stale now.
	if _, err := s.UpdateAlias("f", "live", AliasUpdate{RevisionID: a.RevisionID}); !errors.Is(err, ErrPrecondition) {
		t.Errorf("stale revision err = %v, want ErrPrecondition", err)
	}
	if _, err := s.UpdateAlias("f", "live", AliasUpdate{RevisionID: a2.RevisionID}); err != nil {
		t.Errorf("current revision: %v", err)
	}
}

func TestResolve(t *testing.T) {
	s := aliasFixture(t)

	if _, err := s.CreateAlias("f", "live", "2", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAlias("f", "dev", Latest, "", nil); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		qualifier string
		want      string
		wantErr   error
	}{
		{"", Latest, nil},
		{Latest, Latest, nil},
		{"1", "1", nil},
		{"3", "3", nil},
		{"9", "", ErrNotFound},
		{"live", "2", nil},
		{"dev", Latest, nil},
		{"nope", "", ErrNotFound},
	}

	for _, tt := range tests {
		got, err := s.Resolve("f", tt.qualifier)
		if !errors.Is(err, tt.wantErr) || got != tt.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q, %v", tt.qualifier, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestResolveWeighted(t *testing.T) {
	s := aliasFixture(t)

	if _, err := s.CreateAlias("f", "canary", "1", "", map[string]float64{"2": 0.1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAlias("f", "all", "1", "", map[string]float64{"2": 1}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		alias string
		draw  float64
		want  string
	}{
		{"canary", 0.09, "2"},
		{"canary", 0.10, "1"},
		{"canary", 0, "2"},
		{"canary", 0.99, "1"},
		{"all", 0, "2"},
		{"all", 0.999999, "2"},
	}

	for _, tt := range tests {
		s.random = func() float64 { return tt.draw }

		got, err := s.Resolve("f", tt.alias)
		if err != nil || got != tt.want {
			t.Errorf("Resolve(%s) with draw %v = %q, %v; want %q", tt.alias, tt.draw, got, err, tt.want)
		}
	}
}

func TestResolveWeightedDistribution(t *testing.T) {
	s := aliasFixture(t)

	if _, err := s.CreateAlias("f", "canary", "1", "", map[string]float64{"2": 0.1}); err != nil {
		t.Fatal(err)
	}

	canary := 0

	for range 10000 {
		got, err := s.Resolve("f", "canary")
		if err != nil {
			t.Fatal(err)
		}
		if got == "2" {
			canary++
		}
	}

	if canary < 800 || canary > 1200 {
		t.Errorf("canary picked %d/10000 times, want 800..1200", canary)
	}
}

func TestFunction(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "f")
	fn.Image = "img"

	v := publish(t, s, fn)

	got, err := s.Function("f", v.Version)
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(s.dir, "f", "1", "code"); got.Dir != want {
		t.Errorf("Dir = %q, want %q", got.Dir, want)
	}
	if got.Version != "1" || got.Go != nil || got.Name != "f" {
		t.Errorf("Function = %+v", got)
	}
	if got.Runtime != fn.Runtime || got.Handler != fn.Handler || got.Backend != fn.Backend || got.Image != "img" {
		t.Errorf("Function fields = %+v", got)
	}
	if got.Manifest == nil || got.Manifest.Timeout != 5 {
		t.Errorf("Manifest = %+v", got.Manifest)
	}

	if _, err := s.Function("f", "7"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown version err = %v, want ErrNotFound", err)
	}
}

func TestPublishGoVersionHasNilGo(t *testing.T) {
	s := NewStore(t.TempDir())
	fn := newFn(t, "g")
	fn.Go = &discovery.GoSource{ModuleDir: fn.Dir, Package: "."}

	build := func(_ context.Context, _ discovery.Function, out string) error {
		return os.WriteFile(out, []byte("x"), 0o755)
	}

	v, err := s.Publish(context.Background(), fn, PublishInput{}, build)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Function("g", v.Version)
	if err != nil || got.Go != nil {
		t.Errorf("Function = %+v, %v; want Go nil", got, err)
	}
}

func TestPublishLastModifiedUsesClock(t *testing.T) {
	s := NewStore(t.TempDir())
	fixed := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	s.now = func() time.Time { return fixed }

	v := publish(t, s, newFn(t, "f"))
	if !v.LastModified.Equal(fixed) {
		t.Errorf("LastModified = %v, want %v", v.LastModified, fixed)
	}
}
