package layers

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/manifest"
)

// zipTestEntry describes one entry to write with buildZip.
type zipTestEntry struct {
	name    string
	content []byte
	mode    os.FileMode
}

// buildZip writes a .zip archive at path containing entries, with mode bits
// set explicitly via zip.FileHeader.SetMode so exec-bit and symlink tests
// have full control over what a real layer zip's central directory would
// record — deliberately not relying on archive/zip's defaults, which leave
// mode bits unset.
func buildZip(t *testing.T, path string, entries []zipTestEntry) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	defer f.Close()

	w := zip.NewWriter(f)

	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		hdr.SetMode(e.mode)

		fw, err := w.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("adding entry %q: %v", e.name, err)
		}

		if _, err := fw.Write(e.content); err != nil {
			t.Fatalf("writing entry %q: %v", e.name, err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatalf("closing zip writer for %s: %v", path, err)
	}
}

func TestStage_ZipExecBitPreserved(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	zipPath := filepath.Join(fnDir, "layer.zip")
	buildZip(t, zipPath, []zipTestEntry{
		{name: "bootstrap", content: []byte("#!/bin/sh\necho hi\n"), mode: 0o755},
		{name: "bin/", mode: os.ModeDir | 0o755},
		{name: "bin/tool", content: []byte("#!/bin/sh\n"), mode: 0o755},
		{name: "python/lib.py", content: []byte("# not executable\n"), mode: 0o644},
	})

	refs := []manifest.LayerRef{{Kind: manifest.LayerRefKindPath, Raw: "layer.zip"}}

	staging, err := Stage("myfn", refs, fnDir, cacheDir)
	if err != nil {
		t.Fatalf("Stage() unexpected error: %v", err)
	}

	for _, name := range []string{"bootstrap", filepath.Join("bin", "tool")} {
		info, err := os.Stat(filepath.Join(staging, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}

		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s: exec bit not preserved, mode = %v", name, info.Mode())
		}
	}

	if info, err := os.Stat(filepath.Join(staging, "python", "lib.py")); err != nil {
		t.Fatalf("stat python/lib.py: %v", err)
	} else if info.Mode().Perm()&0o111 != 0 {
		t.Errorf("python/lib.py: unexpectedly executable, mode = %v", info.Mode())
	}

	if info, err := os.Stat(filepath.Join(staging, "bin")); err != nil || !info.IsDir() {
		t.Errorf("bin/ directory entry was not created (err=%v)", err)
	}
}

func TestStage_ZipMergeOrderModeBits(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	zipA := filepath.Join(fnDir, "layer-a.zip")
	buildZip(t, zipA, []zipTestEntry{
		{name: "bootstrap", content: []byte("#!/bin/sh\necho a\n"), mode: 0o644},
	})

	zipB := filepath.Join(fnDir, "layer-b.zip")
	buildZip(t, zipB, []zipTestEntry{
		{name: "bootstrap", content: []byte("#!/bin/sh\necho b\n"), mode: 0o755},
	})

	refs := []manifest.LayerRef{
		{Kind: manifest.LayerRefKindPath, Raw: "layer-a.zip"},
		{Kind: manifest.LayerRefKindPath, Raw: "layer-b.zip"},
	}

	staging, err := Stage("myfn", refs, fnDir, cacheDir)
	if err != nil {
		t.Fatalf("Stage() unexpected error: %v", err)
	}

	info, err := os.Stat(filepath.Join(staging, "bootstrap"))
	if err != nil {
		t.Fatalf("stat bootstrap: %v", err)
	}

	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("bootstrap mode = %v, want executable after the later zip layer overwrites the earlier one's file", info.Mode())
	}

	if got, want := readFile(t, filepath.Join(staging, "bootstrap")), "#!/bin/sh\necho b\n"; got != want {
		t.Errorf("bootstrap content = %q, want %q (later layer's content)", got, want)
	}
}

func TestStage_ZipSlipRejected(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	zipPath := filepath.Join(fnDir, "evil.zip")
	buildZip(t, zipPath, []zipTestEntry{
		{name: "../evil", content: []byte("pwned\n"), mode: 0o644},
	})

	refs := []manifest.LayerRef{{Kind: manifest.LayerRefKindPath, Raw: "evil.zip"}}

	_, err := Stage("myfn", refs, fnDir, cacheDir)
	if err == nil {
		t.Fatal("Stage() expected a zip-slip error, got nil")
	}

	// Nothing should have escaped the intended per-function staging
	// directory, not even into a sibling under cacheDir/staging.
	if _, statErr := os.Stat(filepath.Join(cacheDir, "staging", "evil")); !os.IsNotExist(statErr) {
		t.Errorf("zip-slip entry escaped the staging directory")
	}
}

func TestStage_ZipSymlinkRejected(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	zipPath := filepath.Join(fnDir, "symlink.zip")
	buildZip(t, zipPath, []zipTestEntry{
		{name: "link", content: []byte("/etc/passwd"), mode: os.ModeSymlink | 0o777},
	})

	refs := []manifest.LayerRef{{Kind: manifest.LayerRefKindPath, Raw: "symlink.zip"}}

	_, err := Stage("myfn", refs, fnDir, cacheDir)
	if err == nil {
		t.Fatal("Stage() expected an error for a symlink zip entry, got nil")
	}

	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("Stage() error %q does not mention symlinks", err)
	}
}
