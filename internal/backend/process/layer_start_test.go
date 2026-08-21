package process

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// writeLayerFile writes content to path, creating any missing parent
// directories first — layer fixtures need a nested shape (nodejs/node_modules/...)
// that plain writeFile (which assumes the parent already exists) doesn't
// support.
func writeLayerFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// startWithFakeRIE runs b.Start(fn) against the fake RIE, drives one GET
// against the returned instance's InvokeURL (so the fake RIE's record is
// actually written — see fake_rie.py), and returns the recorded argv/cwd/env.
// It stops the instance via t.Cleanup.
func startWithFakeRIE(t *testing.T, b *processBackend, fn discovery.Function, recordPath string) fakeRIERecord {
	t.Helper()

	inst, err := b.Start(context.Background(), fn)
	if err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}
	t.Cleanup(func() {
		inst.Stop(context.Background()) //nolint:errcheck // best-effort cleanup.
	})

	resp, err := http.Get(inst.InvokeURL())
	if err != nil {
		t.Fatalf("GET InvokeURL() unexpected error: %v", err)
	}
	resp.Body.Close() //nolint:errcheck // draining a test response body.

	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("reading fake RIE's recorded argv/env/cwd: %v", err)
	}

	var rec fakeRIERecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshalling fake RIE record: %v", err)
	}

	return rec
}

// TestProcessBackendStartLayers is the Start-level env-assertion round trip
// plans/layers.md Unit D asks for per family: it stages a real local-dir
// layer via layers.Stage (not a fake) and asserts the resulting env the RIE
// would see, using the same fake-RIE harness TestProcessBackendStart uses.
// It stops short of driving the real node/python3/ruby shim against staged
// content (which shim_contract_test.go's harness supports but would need
// per-family test fixtures and interpreters on PATH here too); asserting
// the env Start assembles is the documented fallback when the full
// shim-import round trip would be heavier than the env contract itself
// warrants.
func TestProcessBackendStartLayers(t *testing.T) {
	t.Run("nodejs: NODE_PATH gets both entries, PATH gains staging/bin", func(t *testing.T) {
		riePath := fakeRIEPath(t)
		fnDir := t.TempDir()
		cacheDir := t.TempDir()
		recordPath := filepath.Join(t.TempDir(), "record.json")
		layerDir := t.TempDir()

		writeLayerFile(t, filepath.Join(layerDir, "nodejs", "node_modules", "shared.mjs"), "export const x = 1;\n")

		b := &processBackend{riePath: riePath, home: t.TempDir(), readyTimeout: 5 * time.Second, cacheDir: cacheDir}
		fn := discovery.Function{
			Name:    "hello",
			Dir:     fnDir,
			Runtime: "nodejs22.x",
			Handler: "index.handler",
			Manifest: &manifest.Manifest{
				Layers:      []string{layerDir},
				Environment: map[string]string{"FAKE_RIE_RECORD": recordPath},
			},
		}

		rec := startWithFakeRIE(t, b, fn, recordPath)

		staging := filepath.Join(cacheDir, "staging", "hello")
		wantNodePath := filepath.Join(staging, "nodejs", "node_modules") + string(os.PathListSeparator) + filepath.Join(staging, "nodejs", "node22", "node_modules")

		if rec.Env["NODE_PATH"] != wantNodePath {
			t.Errorf("env NODE_PATH = %q, want %q", rec.Env["NODE_PATH"], wantNodePath)
		}
		if !strings.HasSuffix(rec.Env["PATH"], string(os.PathListSeparator)+filepath.Join(staging, "bin")) {
			t.Errorf("env PATH = %q, want it to end with the host PATH plus %s", rec.Env["PATH"], filepath.Join(staging, "bin"))
		}

		if _, err := os.Stat(filepath.Join(staging, "nodejs", "node_modules", "shared.mjs")); err != nil {
			t.Errorf("staged layer content missing: %v", err)
		}
	})

	t.Run("python: PYTHONPATH gets both entries", func(t *testing.T) {
		riePath := fakeRIEPath(t)
		fnDir := t.TempDir()
		cacheDir := t.TempDir()
		recordPath := filepath.Join(t.TempDir(), "record.json")
		layerDir := t.TempDir()

		writeLayerFile(t, filepath.Join(layerDir, "python", "shared.py"), "x = 1\n")

		b := &processBackend{riePath: riePath, home: t.TempDir(), readyTimeout: 5 * time.Second, cacheDir: cacheDir}
		fn := discovery.Function{
			Name:    "hello",
			Dir:     fnDir,
			Runtime: "python3.13",
			Handler: "app.handler",
			Manifest: &manifest.Manifest{
				Layers:      []string{layerDir},
				Environment: map[string]string{"FAKE_RIE_RECORD": recordPath},
			},
		}

		rec := startWithFakeRIE(t, b, fn, recordPath)

		staging := filepath.Join(cacheDir, "staging", "hello")
		wantPythonPath := filepath.Join(staging, "python") + string(os.PathListSeparator) + filepath.Join(staging, "python", "lib", "python3.13", "site-packages")

		if rec.Env["PYTHONPATH"] != wantPythonPath {
			t.Errorf("env PYTHONPATH = %q, want %q", rec.Env["PYTHONPATH"], wantPythonPath)
		}
	})

	t.Run("ruby: RUBYLIB and GEM_PATH are both set", func(t *testing.T) {
		riePath := fakeRIEPath(t)
		fnDir := t.TempDir()
		cacheDir := t.TempDir()
		recordPath := filepath.Join(t.TempDir(), "record.json")
		layerDir := t.TempDir()

		writeLayerFile(t, filepath.Join(layerDir, "ruby", "lib", "shared.rb"), "X = 1\n")

		b := &processBackend{riePath: riePath, home: t.TempDir(), readyTimeout: 5 * time.Second, cacheDir: cacheDir}
		fn := discovery.Function{
			Name:    "hello",
			Dir:     fnDir,
			Runtime: "ruby3.3",
			Handler: "function.handler",
			Manifest: &manifest.Manifest{
				Layers:      []string{layerDir},
				Environment: map[string]string{"FAKE_RIE_RECORD": recordPath},
			},
		}

		rec := startWithFakeRIE(t, b, fn, recordPath)

		staging := filepath.Join(cacheDir, "staging", "hello")
		wantRubylib := filepath.Join(staging, "ruby", "lib")
		wantGemPath := filepath.Join(staging, "ruby", "gems", "3.3.0")

		if rec.Env["RUBYLIB"] != wantRubylib {
			t.Errorf("env RUBYLIB = %q, want %q", rec.Env["RUBYLIB"], wantRubylib)
		}
		if rec.Env["GEM_PATH"] != wantGemPath {
			t.Errorf("env GEM_PATH = %q, want %q", rec.Env["GEM_PATH"], wantGemPath)
		}
	})

	t.Run("no layers configured: env is untouched, no staging directory is created", func(t *testing.T) {
		riePath := fakeRIEPath(t)
		fnDir := t.TempDir()
		cacheDir := t.TempDir()
		recordPath := filepath.Join(t.TempDir(), "record.json")

		b := &processBackend{riePath: riePath, home: t.TempDir(), readyTimeout: 5 * time.Second, cacheDir: cacheDir}
		fn := discovery.Function{
			Name:    "hello",
			Dir:     fnDir,
			Runtime: "nodejs22.x",
			Handler: "index.handler",
			Manifest: &manifest.Manifest{
				Environment: map[string]string{"FAKE_RIE_RECORD": recordPath},
			},
		}

		rec := startWithFakeRIE(t, b, fn, recordPath)

		if _, ok := rec.Env["NODE_PATH"]; ok {
			t.Errorf("env NODE_PATH = %q, want it unset when the function has no layers:", rec.Env["NODE_PATH"])
		}

		if _, err := os.Stat(filepath.Join(cacheDir, "staging", "hello")); !os.IsNotExist(err) {
			t.Errorf("staging directory exists at %s, want no staging when the function has no layers:", filepath.Join(cacheDir, "staging", "hello"))
		}
	})
}
