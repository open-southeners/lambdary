package lockfile

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestLoadMissingIsEmptyNoError covers the common case: a project that has
// never run has no lock file at all, and Load must not treat that as an
// error.
func TestLoadMissingIsEmptyNoError(t *testing.T) {
	dir := t.TempDir()

	l, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if _, ok := l.ImageDigest("anything"); ok {
		t.Errorf("ImageDigest() on a fresh lock = found, want not found")
	}
}

// TestLoadCorruptToleratedWithError covers Load's "never block a run"
// contract: a corrupt lock file still yields a usable (empty) *Lock, with
// the parse error returned for the caller to log as a warning.
func TestLoadCorruptToleratedWithError(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, dirName), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, dirName, fileName), []byte("not: [valid yaml"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l, err := Load(dir)
	if err == nil {
		t.Fatal("Load() expected error for corrupt lock file, got nil")
	}

	if l == nil {
		t.Fatal("Load() returned nil *Lock on a corrupt file, want a usable empty one")
	}

	if _, ok := l.ImageDigest("anything"); ok {
		t.Errorf("ImageDigest() on a corrupt-then-tolerated lock = found, want not found")
	}
}

// TestRecordImageRoundTrip covers RecordImage's auto-save: a fresh Lock
// records a digest, and a second Load from the same dir sees it.
func TestRecordImageRoundTrip(t *testing.T) {
	dir := t.TempDir()

	l, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	const tag = "public.ecr.aws/lambda/python:3.13"
	const digest = "sha256:abc123"

	if err := l.RecordImage(tag, digest); err != nil {
		t.Fatalf("RecordImage() unexpected error: %v", err)
	}

	got, ok := l.ImageDigest(tag)
	if !ok || got != digest {
		t.Errorf("ImageDigest(%q) = %q, %v, want %q, true", tag, got, ok, digest)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() (reload) unexpected error: %v", err)
	}

	got, ok = reloaded.ImageDigest(tag)
	if !ok || got != digest {
		t.Errorf("reloaded ImageDigest(%q) = %q, %v, want %q, true", tag, got, ok, digest)
	}
}

// TestRecordLayerRoundTrip covers RecordLayer's auto-save, mirroring
// TestRecordImageRoundTrip for the layers: map plans/layers.md's Unit E
// adds.
func TestRecordLayerRoundTrip(t *testing.T) {
	dir := t.TempDir()

	l, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	const arn = "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1"
	const digest = "sha256base64=="

	if err := l.RecordLayer(arn, digest); err != nil {
		t.Fatalf("RecordLayer() unexpected error: %v", err)
	}

	got, ok := l.LayerDigest(arn)
	if !ok || got != digest {
		t.Errorf("LayerDigest(%q) = %q, %v, want %q, true", arn, got, ok, digest)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() (reload) unexpected error: %v", err)
	}

	got, ok = reloaded.LayerDigest(arn)
	if !ok || got != digest {
		t.Errorf("reloaded LayerDigest(%q) = %q, %v, want %q, true", arn, got, ok, digest)
	}
}

// TestLoadWithoutLayersKey covers the additive-field contract: a lock file
// written before Unit E (no `layers:` key at all) still loads cleanly, with
// LayerDigest simply reporting "not found" rather than erroring.
func TestLoadWithoutLayersKey(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, dirName), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	pre := "version: 1\nrie: v1.35\nimages:\n  public.ecr.aws/lambda/nodejs:22: sha256:deadbeef\n"
	if err := os.WriteFile(filepath.Join(dir, dirName, fileName), []byte(pre), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if _, ok := l.LayerDigest("arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1"); ok {
		t.Error("LayerDigest() on a pre-Unit-E lock = found, want not found")
	}

	got, ok := l.ImageDigest("public.ecr.aws/lambda/nodejs:22")
	if !ok || got != "sha256:deadbeef" {
		t.Errorf("ImageDigest() = %q, %v, want %q, true (pre-existing images: untouched)", got, ok, "sha256:deadbeef")
	}
}

// TestSetRIERoundTrip covers SetRIE's auto-save the same way.
func TestSetRIERoundTrip(t *testing.T) {
	dir := t.TempDir()

	l, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if err := l.SetRIE("v1.35"); err != nil {
		t.Fatalf("SetRIE() unexpected error: %v", err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() (reload) unexpected error: %v", err)
	}

	if reloaded.data.RIE != "v1.35" {
		t.Errorf("reloaded rie = %q, want %q", reloaded.data.RIE, "v1.35")
	}
}

// TestSaveWritesVersionedYAML checks the on-disk shape: version/rie/images/
// layers keys, per plans/m5-extras.md's Unit C format and plans/layers.md's
// Unit E addition.
func TestSaveWritesVersionedYAML(t *testing.T) {
	dir := t.TempDir()

	l, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if err := l.SetRIE("v1.35"); err != nil {
		t.Fatalf("SetRIE() unexpected error: %v", err)
	}

	if err := l.RecordImage("public.ecr.aws/lambda/nodejs:22", "sha256:deadbeef"); err != nil {
		t.Fatalf("RecordImage() unexpected error: %v", err)
	}

	if err := l.RecordLayer("arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1", "sha256base64=="); err != nil {
		t.Fatalf("RecordLayer() unexpected error: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, dirName, fileName))
	if err != nil {
		t.Fatalf("reading lock file: %v", err)
	}

	got := string(raw)
	for _, want := range []string{
		"version: 1", "rie: v1.35", "images:", "public.ecr.aws/lambda/nodejs:22: sha256:deadbeef",
		"layers:", "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1: sha256base64==",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lock file content = %q, want it to contain %q", got, want)
		}
	}
}

// TestConcurrentRecordImage exercises the internal mutex: many goroutines
// calling RecordImage on the same *Lock concurrently must never race
// (run with -race) and every recorded image must survive to the final
// save.
func TestConcurrentRecordImage(t *testing.T) {
	dir := t.TempDir()

	l, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	const n = 20

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			tag := filepath.Join("registry.example.com/fn", string(rune('a'+i)))
			if err := l.RecordImage(tag, "sha256:"+string(rune('a'+i))); err != nil {
				t.Errorf("RecordImage() unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() (reload) unexpected error: %v", err)
	}

	if got := len(reloaded.data.Images); got != n {
		t.Errorf("reloaded images count = %d, want %d", got, n)
	}
}
