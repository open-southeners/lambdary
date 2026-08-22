package layers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/manifest"
)

func TestStage_EmptyRefs(t *testing.T) {
	staging, err := Stage("myfn", nil, "unused-fndir", "unused-cachedir")
	if err != nil {
		t.Fatalf("Stage() unexpected error: %v", err)
	}

	if staging != "" {
		t.Errorf("Stage() = %q, want empty string", staging)
	}
}

func TestStage_MergeOrder(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	writeFile(t, filepath.Join(fnDir, "layer-a", "python", "lib.py"), "from layer A\n", 0o644)
	writeFile(t, filepath.Join(fnDir, "layer-b", "python", "lib.py"), "from layer B\n", 0o644)

	refs := []manifest.LayerRef{
		{Kind: manifest.LayerRefKindPath, Raw: "layer-a"},
		{Kind: manifest.LayerRefKindPath, Raw: "layer-b"},
	}

	staging, err := Stage("myfn", refs, fnDir, cacheDir)
	if err != nil {
		t.Fatalf("Stage() unexpected error: %v", err)
	}

	got := readFile(t, filepath.Join(staging, "python", "lib.py"))
	if want := "from layer B\n"; got != want {
		t.Errorf("staged python/lib.py = %q, want %q (later layer should win)", got, want)
	}
}

func TestStage_MergeOrderModeBits(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	writeFile(t, filepath.Join(fnDir, "layer-a", "bootstrap"), "#!/bin/sh\necho a\n", 0o644)
	writeFile(t, filepath.Join(fnDir, "layer-b", "bootstrap"), "#!/bin/sh\necho b\n", 0o755)

	t.Run("later exec overwrites earlier non-exec", func(t *testing.T) {
		refs := []manifest.LayerRef{
			{Kind: manifest.LayerRefKindPath, Raw: "layer-a"},
			{Kind: manifest.LayerRefKindPath, Raw: "layer-b"},
		}

		staging, err := Stage("myfn-exec", refs, fnDir, cacheDir)
		if err != nil {
			t.Fatalf("Stage() unexpected error: %v", err)
		}

		info, err := os.Stat(filepath.Join(staging, "bootstrap"))
		if err != nil {
			t.Fatalf("stat bootstrap: %v", err)
		}

		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("bootstrap mode = %v, want executable after the later layer overwrites the earlier one's file", info.Mode())
		}
	})

	t.Run("later non-exec overwrites earlier exec", func(t *testing.T) {
		refs := []manifest.LayerRef{
			{Kind: manifest.LayerRefKindPath, Raw: "layer-b"},
			{Kind: manifest.LayerRefKindPath, Raw: "layer-a"},
		}

		staging, err := Stage("myfn-nonexec", refs, fnDir, cacheDir)
		if err != nil {
			t.Fatalf("Stage() unexpected error: %v", err)
		}

		info, err := os.Stat(filepath.Join(staging, "bootstrap"))
		if err != nil {
			t.Fatalf("stat bootstrap: %v", err)
		}

		if info.Mode().Perm()&0o111 != 0 {
			t.Errorf("bootstrap mode = %v, want non-executable after the later layer overwrites the earlier one's file", info.Mode())
		}
	})
}

func TestStage_RelativePathResolvesAgainstFnDir(t *testing.T) {
	root := t.TempDir()
	fnDir := filepath.Join(root, "fn")

	if err := os.MkdirAll(fnDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The layer lives next to fnDir, not under it and not under this
	// package's testdata: resolving "../layer-content" against the test
	// binary's working directory (this package's source dir) would find
	// nothing, so a passing test proves resolution happens against fnDir.
	writeFile(t, filepath.Join(root, "layer-content", "python", "lib.py"), "relative layer content\n", 0o644)

	cacheDir := t.TempDir()
	refs := []manifest.LayerRef{{Kind: manifest.LayerRefKindPath, Raw: "../layer-content"}}

	staging, err := Stage("myfn", refs, fnDir, cacheDir)
	if err != nil {
		t.Fatalf("Stage() unexpected error: %v", err)
	}

	got := readFile(t, filepath.Join(staging, "python", "lib.py"))
	if want := "relative layer content\n"; got != want {
		t.Errorf("staged python/lib.py = %q, want %q", got, want)
	}
}

func TestStage_AbsolutePathRef(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	abs, err := filepath.Abs(filepath.Join("testdata", "sample-layer"))
	if err != nil {
		t.Fatal(err)
	}

	refs := []manifest.LayerRef{{Kind: manifest.LayerRefKindPath, Raw: abs}}

	staging, err := Stage("myfn", refs, fnDir, cacheDir)
	if err != nil {
		t.Fatalf("Stage() unexpected error: %v", err)
	}

	got := readFile(t, filepath.Join(staging, "python", "greeting.txt"))
	if want := "hello from testdata\n"; got != want {
		t.Errorf("staged python/greeting.txt = %q, want %q", got, want)
	}
}

func TestStage_PathRefErrors(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	writeFile(t, filepath.Join(fnDir, "not-a-layer.txt"), "plain file\n", 0o644)

	tests := []struct {
		name string
		raw  string
	}{
		{name: "missing path", raw: "does-not-exist"},
		{name: "non-zip file", raw: "not-a-layer.txt"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refs := []manifest.LayerRef{{Kind: manifest.LayerRefKindPath, Raw: tc.raw}}

			_, err := Stage("myfn", refs, fnDir, cacheDir)
			if err == nil {
				t.Fatal("Stage() expected error, got nil")
			}

			if !strings.Contains(err.Error(), tc.raw) {
				t.Errorf("Stage() error %q does not name the ref %q", err, tc.raw)
			}

			if !strings.Contains(err.Error(), "myfn") {
				t.Errorf("Stage() error %q does not name the function %q", err, "myfn")
			}
		})
	}
}

func TestStage_ARNCacheHit(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	ref := manifest.LayerRef{
		Kind:   manifest.LayerRefKindARN,
		Raw:    "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1",
		Region: "eu-west-1",
	}

	writeFile(t, filepath.Join(CachePath(cacheDir, ref), "python", "cached.py"), "cached content\n", 0o644)

	staging, err := Stage("myfn", []manifest.LayerRef{ref}, fnDir, cacheDir)
	if err != nil {
		t.Fatalf("Stage() unexpected error: %v", err)
	}

	got := readFile(t, filepath.Join(staging, "python", "cached.py"))
	if want := "cached content\n"; got != want {
		t.Errorf("staged python/cached.py = %q, want %q", got, want)
	}
}

// TestStage_ARNCacheHitMarkerNotStaged is a regression test: Fetch writes
// its own markerFileName (".codesha256") at the ARN cache dir's root (see
// fetch.go) so internal/cli can later detect cache corruption, but that
// marker is not part of the layer's own content and must never leak into a
// function's staged /opt — real Lambda has no such file there.
func TestStage_ARNCacheHitMarkerNotStaged(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	ref := manifest.LayerRef{
		Kind:   manifest.LayerRefKindARN,
		Raw:    "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1",
		Region: "eu-west-1",
	}

	writeFile(t, filepath.Join(CachePath(cacheDir, ref), "python", "cached.py"), "cached content\n", 0o644)
	writeFile(t, filepath.Join(CachePath(cacheDir, ref), markerFileName), "the-codesha256\n", 0o644)

	staging, err := Stage("myfn", []manifest.LayerRef{ref}, fnDir, cacheDir)
	if err != nil {
		t.Fatalf("Stage() unexpected error: %v", err)
	}

	got := readFile(t, filepath.Join(staging, "python", "cached.py"))
	if want := "cached content\n"; got != want {
		t.Errorf("staged python/cached.py = %q, want %q", got, want)
	}

	if _, err := os.Stat(filepath.Join(staging, markerFileName)); !os.IsNotExist(err) {
		t.Errorf("staged %s exists, want it stripped from staging (err=%v)", markerFileName, err)
	}

	// The cache dir itself must be untouched: later Stage/CachedDigest
	// callers still need to find the marker there.
	if got, ok := CachedDigest(cacheDir, ref); !ok || got != "the-codesha256" {
		t.Errorf("CachedDigest() = %q, %v, want %q, true (cache dir's own marker must survive staging)", got, ok, "the-codesha256")
	}
}

func TestStage_ARNCacheMiss(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	arn := "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1"
	refs := []manifest.LayerRef{{Kind: manifest.LayerRefKindARN, Raw: arn, Region: "eu-west-1"}}

	_, err := Stage("myfn", refs, fnDir, cacheDir)
	if err == nil {
		t.Fatal("Stage() expected error for an uncached ARN, got nil")
	}

	if !strings.Contains(err.Error(), arn) {
		t.Errorf("Stage() error %q does not name the ARN %q", err, arn)
	}
}

func TestStage_MixedRefs(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	writeFile(t, filepath.Join(fnDir, "dir-layer", "python", "from_dir.py"), "from dir layer\n", 0o644)

	zipPath := filepath.Join(fnDir, "zip-layer.zip")
	buildZip(t, zipPath, []zipTestEntry{
		{name: "python/from_zip.py", content: []byte("from zip layer\n"), mode: 0o644},
	})

	arnRef := manifest.LayerRef{
		Kind:   manifest.LayerRefKindARN,
		Raw:    "arn:aws:lambda:eu-west-1:534081306603:layer:shared:2",
		Region: "eu-west-1",
	}
	writeFile(t, filepath.Join(CachePath(cacheDir, arnRef), "python", "from_arn.py"), "from arn layer\n", 0o644)

	refs := []manifest.LayerRef{
		{Kind: manifest.LayerRefKindPath, Raw: "dir-layer"},
		{Kind: manifest.LayerRefKindPath, Raw: "zip-layer.zip"},
		arnRef,
	}

	staging, err := Stage("myfn", refs, fnDir, cacheDir)
	if err != nil {
		t.Fatalf("Stage() unexpected error: %v", err)
	}

	want := map[string]string{
		filepath.Join("python", "from_dir.py"): "from dir layer\n",
		filepath.Join("python", "from_zip.py"): "from zip layer\n",
		filepath.Join("python", "from_arn.py"): "from arn layer\n",
	}

	for rel, wantContent := range want {
		got := readFile(t, filepath.Join(staging, rel))
		if got != wantContent {
			t.Errorf("staged %s = %q, want %q", rel, got, wantContent)
		}
	}
}

func TestStage_Restage(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	layerDir := filepath.Join(fnDir, "layer")
	writeFile(t, filepath.Join(layerDir, "keep.py"), "v1\n", 0o644)
	writeFile(t, filepath.Join(layerDir, "removed.py"), "will be removed\n", 0o644)

	refs := []manifest.LayerRef{{Kind: manifest.LayerRefKindPath, Raw: "layer"}}

	staging1, err := Stage("myfn", refs, fnDir, cacheDir)
	if err != nil {
		t.Fatalf("Stage() first call unexpected error: %v", err)
	}

	if got := readFile(t, filepath.Join(staging1, "removed.py")); got != "will be removed\n" {
		t.Fatalf("staged removed.py before edit = %q", got)
	}

	if err := os.WriteFile(filepath.Join(layerDir, "keep.py"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(layerDir, "removed.py")); err != nil {
		t.Fatal(err)
	}

	staging2, err := Stage("myfn", refs, fnDir, cacheDir)
	if err != nil {
		t.Fatalf("Stage() second call unexpected error: %v", err)
	}

	if staging2 != staging1 {
		t.Fatalf("Stage() staging path changed between calls: %q vs %q", staging1, staging2)
	}

	if got := readFile(t, filepath.Join(staging2, "keep.py")); got != "v2\n" {
		t.Errorf("Stage() did not pick up the edited source file: got %q, want %q", got, "v2\n")
	}

	if _, err := os.Stat(filepath.Join(staging2, "removed.py")); !os.IsNotExist(err) {
		t.Errorf("Stage() retained a file that was deleted from the source layer")
	}
}

func TestStage_DirSymlinkRejected(t *testing.T) {
	fnDir := t.TempDir()
	cacheDir := t.TempDir()

	layerDir := filepath.Join(fnDir, "layer")
	if err := os.MkdirAll(layerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("/etc", filepath.Join(layerDir, "link")); err != nil {
		t.Fatal(err)
	}

	refs := []manifest.LayerRef{{Kind: manifest.LayerRefKindPath, Raw: "layer"}}

	_, err := Stage("myfn", refs, fnDir, cacheDir)
	if err == nil {
		t.Fatal("Stage() expected an error for a symlink in a dir layer, got nil")
	}

	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("Stage() error %q does not mention symlinks", err)
	}
}

func TestCachePath(t *testing.T) {
	ref := manifest.LayerRef{
		Kind: manifest.LayerRefKindARN,
		Raw:  "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1",
	}

	got := CachePath("/cache", ref)
	want := filepath.Join("/cache", "layers", "arn_aws_lambda_eu-west-1_534081306603_layer_php-83_1")

	if got != want {
		t.Errorf("CachePath() = %q, want %q", got, want)
	}

	// Deterministic: calling it again with an equivalent ref yields the
	// same path.
	if got2 := CachePath("/cache", ref); got2 != got {
		t.Errorf("CachePath() not deterministic: %q vs %q", got, got2)
	}
}

// writeFile writes content to path, creating any missing parent
// directories, and fails the test on error.
func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// readFile reads path and fails the test on error.
func readFile(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return string(b)
}
