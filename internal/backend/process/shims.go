package process

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// nodeShimName, pythonShimName, and rubyShimName are the file names
// writeShims gives the embedded shims on disk, and the names runtimeCommand
// joins onto shimDir.
const (
	nodeShimName   = "bootstrap.mjs"
	pythonShimName = "bootstrap.py"
	rubyShimName   = "bootstrap.rb"
)

//go:embed shims/bootstrap.mjs
var nodeShim []byte

//go:embed shims/bootstrap.py
var pythonShim []byte

//go:embed shims/bootstrap.rb
var rubyShim []byte

// shimsHash content-addresses the embedded shims: the directory writeShims
// writes them to (and runtimeCommand later points node/python3/ruby at)
// changes whenever any shim's source changes, so a Lambdary upgrade never
// serves a shim left over from an older version, and two Lambdary builds on
// the same host (e.g. two checkouts) never clobber each other's copy.
var shimsHash = computeShimsHash()

func computeShimsHash() string {
	h := sha256.New()
	h.Write(nodeShim)
	h.Write(pythonShim)
	h.Write(rubyShim)

	return hex.EncodeToString(h.Sum(nil))[:12]
}

// writeShims ensures the embedded Node, Python, and Ruby shims exist on
// disk under home/shims/<shimsHash>, writing them only if they aren't
// already there (idempotent: a repeat call for the same Lambdary build is a
// no-op) and returns that directory.
func writeShims(home string) (string, error) {
	dir := filepath.Join(home, "shims", shimsHash)

	if shimsPresent(dir) {
		return dir, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating shims directory: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, nodeShimName), nodeShim, 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", nodeShimName, err)
	}

	if err := os.WriteFile(filepath.Join(dir, pythonShimName), pythonShim, 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", pythonShimName, err)
	}

	if err := os.WriteFile(filepath.Join(dir, rubyShimName), rubyShim, 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", rubyShimName, err)
	}

	return dir, nil
}

// shimsPresent reports whether all three shims already exist under dir.
func shimsPresent(dir string) bool {
	for _, name := range []string{nodeShimName, pythonShimName, rubyShimName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}

	return true
}
