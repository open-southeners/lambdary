package process

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// nodeShimName and pythonShimName are the file names writeShims gives the
// embedded shims on disk, and the names runtimeCommand joins onto shimDir.
const (
	nodeShimName   = "bootstrap.mjs"
	pythonShimName = "bootstrap.py"
)

//go:embed shims/bootstrap.mjs
var nodeShim []byte

//go:embed shims/bootstrap.py
var pythonShim []byte

// shimsHash content-addresses the embedded shims: the directory writeShims
// writes them to (and runtimeCommand later points node/python3 at) changes
// whenever either shim's source changes, so a Lambdary upgrade never serves
// a shim left over from an older version, and two Lambdary builds on the
// same host (e.g. two checkouts) never clobber each other's copy.
var shimsHash = computeShimsHash()

func computeShimsHash() string {
	h := sha256.New()
	h.Write(nodeShim)
	h.Write(pythonShim)

	return hex.EncodeToString(h.Sum(nil))[:12]
}

// writeShims ensures the embedded Node and Python shims exist on disk under
// home/shims/<shimsHash>, writing them only if they aren't already there
// (idempotent: a repeat call for the same Lambdary build is a no-op) and
// returns that directory.
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

	return dir, nil
}

// shimsPresent reports whether both shims already exist under dir.
func shimsPresent(dir string) bool {
	for _, name := range []string{nodeShimName, pythonShimName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}

	return true
}
