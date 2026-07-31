package process

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteShims(t *testing.T) {
	home := t.TempDir()

	dir, err := writeShims(home)
	if err != nil {
		t.Fatalf("writeShims() unexpected error: %v", err)
	}

	wantDir := filepath.Join(home, "shims", shimsHash)
	if dir != wantDir {
		t.Errorf("writeShims() dir = %q, want %q (content-addressed by the embedded shims' hash)", dir, wantDir)
	}

	nodeContent, err := os.ReadFile(filepath.Join(dir, nodeShimName))
	if err != nil {
		t.Fatalf("reading written node shim: %v", err)
	}
	if string(nodeContent) != string(nodeShim) {
		t.Error("written node shim content does not match the embedded shim")
	}

	pythonContent, err := os.ReadFile(filepath.Join(dir, pythonShimName))
	if err != nil {
		t.Fatalf("reading written python shim: %v", err)
	}
	if string(pythonContent) != string(pythonShim) {
		t.Error("written python shim content does not match the embedded shim")
	}
}

func TestWriteShimsIsIdempotent(t *testing.T) {
	home := t.TempDir()

	dir1, err := writeShims(home)
	if err != nil {
		t.Fatalf("first writeShims() unexpected error: %v", err)
	}

	// Tamper with one shim's mtime-sensitive content marker: writeShims
	// must treat "both files already present" as done and not rewrite
	// them, so overwrite one with different content and confirm a second
	// call leaves it alone.
	marker := filepath.Join(dir1, nodeShimName)
	if err := os.WriteFile(marker, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tampering with written shim: %v", err)
	}

	dir2, err := writeShims(home)
	if err != nil {
		t.Fatalf("second writeShims() unexpected error: %v", err)
	}
	if dir2 != dir1 {
		t.Fatalf("writeShims() dir changed between calls: %q then %q", dir1, dir2)
	}

	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("reading tampered shim: %v", err)
	}
	if string(content) != "tampered" {
		t.Error("writeShims() rewrote an already-present shim instead of being a no-op")
	}
}
