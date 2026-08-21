// Package layers resolves a function's `layers:` manifest entries
// (internal/manifest.LayerRef, produced by manifest.ParseLayerRef) into a
// single merged staging directory, per plans/layers.md's Unit B. It is pure
// filesystem work: no AWS SDK calls and no network access live here — a
// layer version ARN that isn't already in the cache is a Stage error naming
// Unit E's fetch step, which this package deliberately does not implement.
// Backend wiring (mounting the staging dir at `/opt` in the container
// backend, or exporting it via runtime search-path env vars in the process
// backend) is Units C and D; this package only produces the directory they
// consume.
//
// # Staging is rebuilt on every call
//
// Stage always removes and recreates its staging directory before
// resolving refs into it, even if a previous call already staged the same
// function. This is deliberate, not wasteful: it's what makes a function
// restart pick up edits to a local layer directory (per DESIGN.md's
// "Layers" section, hot-reloading layer content mid-run is out of scope —
// only a restart re-stages). Copying local files is cheap, so paying that
// cost on every Start is the simplest way to keep staging always correct.
//
// # Merge order
//
// Refs are resolved in the order they appear in the manifest's `layers:`
// list, each one copied into the same staging directory, later refs
// overwriting files earlier refs already placed — AWS's own documented
// layer-merge semantics (the last layer in the list wins on conflicts).
package layers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-southeners/lambdary/internal/manifest"
)

// Stage resolves refs (a function's parsed `layers:` entries, in manifest
// order) into a single staging directory for fnName and returns its path.
//
// Relative LayerRefKindPath entries resolve against fnDir (the function's
// directory); absolute paths are used as-is. LayerRefKindARN entries are
// looked up only at CachePath(cacheDir, ref) — Stage never fetches; a cache
// miss is an error naming the ARN, per the package doc.
//
// len(refs) == 0 returns ("", nil): callers skip all layer wiring, which
// keeps functions without a `layers:` key behaviorally unchanged.
func Stage(fnName string, refs []manifest.LayerRef, fnDir, cacheDir string) (string, error) {
	if len(refs) == 0 {
		return "", nil
	}

	staging := stagingDir(cacheDir, fnName)

	if err := os.RemoveAll(staging); err != nil {
		return "", fmt.Errorf("layers: %s: clearing staging directory %s: %w", fnName, staging, err)
	}

	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", fmt.Errorf("layers: %s: creating staging directory %s: %w", fnName, staging, err)
	}

	for _, ref := range refs {
		if err := resolveInto(fnName, ref, fnDir, cacheDir, staging); err != nil {
			return "", err
		}
	}

	return staging, nil
}

// stagingDir returns the per-function staging directory Stage rebuilds on
// every call: <cacheDir>/staging/<fnName>/.
func stagingDir(cacheDir, fnName string) string {
	return filepath.Join(cacheDir, "staging", fnName)
}

// CachePath returns the directory a layer version ARN's fetched content is
// expected to live in: <cacheDir>/layers/<sanitized-arn>/. This is the
// contract shared between this package (which only reads it, on a
// LayerRefKindARN ref, and errors if it's missing — see plans/layers.md
// Unit B) and Unit E's fetcher (which must write the extracted layer
// content there, and nowhere else, for Stage to find it). ref is normally
// LayerRefKindARN; CachePath sanitizes ref.Raw regardless of Kind.
func CachePath(cacheDir string, ref manifest.LayerRef) string {
	return filepath.Join(cacheDir, "layers", sanitizeARN(ref.Raw))
}

// arnSanitizer replaces characters that are safe in an ARN but not
// universally safe as a single path segment on darwin/linux. Layer version
// ARNs (arn:aws:lambda:<region>:<account>:layer:<name>:<version>) never
// contain a "/", but the replacement is defensive rather than relying on
// that. The mapping only needs to be deterministic, not reversible.
var arnSanitizer = strings.NewReplacer(":", "_", "/", "_")

// sanitizeARN turns arn into a filesystem-safe directory name.
func sanitizeARN(arn string) string {
	return arnSanitizer.Replace(arn)
}

// resolveInto resolves a single ref into staging, dispatching on its Kind.
func resolveInto(fnName string, ref manifest.LayerRef, fnDir, cacheDir, staging string) error {
	switch ref.Kind {
	case manifest.LayerRefKindARN:
		return resolveARN(fnName, ref, cacheDir, staging)
	case manifest.LayerRefKindPath:
		return resolvePath(fnName, ref, fnDir, staging)
	default:
		return fmt.Errorf("layers: %s: layer %q: unknown ref kind %q", fnName, ref.Raw, ref.Kind)
	}
}

// resolveARN merges the cached content for an ARN ref into staging. It
// never fetches: a cache miss is a clear error naming the ARN, per the
// package doc, so this package stays buildable and testable with zero AWS
// dependency (plans/layers.md Unit E adds fetching).
func resolveARN(fnName string, ref manifest.LayerRef, cacheDir, staging string) error {
	src := CachePath(cacheDir, ref)

	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("layers: %s: layer %s not cached and ARN fetching is not implemented yet (see plans/layers.md Unit E)", fnName, ref.Raw)
	}

	if err := copyTree(src, staging); err != nil {
		return fmt.Errorf("layers: %s: layer %s: %w", fnName, ref.Raw, err)
	}

	return nil
}

// resolvePath merges a local path ref (a directory of already-extracted
// layer content, or a .zip archive) into staging. Existence and shape are
// checked here, at staging time, matching manifest.ParseLayerRef's promise
// to do no I/O of its own.
func resolvePath(fnName string, ref manifest.LayerRef, fnDir, staging string) error {
	src := ref.Raw
	if !filepath.IsAbs(src) {
		src = filepath.Join(fnDir, src)
	}

	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("layers: %s: layer %q: resolving %s: %w", fnName, ref.Raw, src, err)
	}

	if info.IsDir() {
		if err := copyTree(src, staging); err != nil {
			return fmt.Errorf("layers: %s: layer %q: %w", fnName, ref.Raw, err)
		}

		return nil
	}

	if strings.EqualFold(filepath.Ext(src), ".zip") {
		if err := extractZip(src, staging); err != nil {
			return fmt.Errorf("layers: %s: layer %q: %w", fnName, ref.Raw, err)
		}

		return nil
	}

	return fmt.Errorf("layers: %s: layer %q: %s is neither a directory nor a .zip file", fnName, ref.Raw, src)
}
