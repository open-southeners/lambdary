// Package lockfile implements `.lambdary/lock`, a versioned YAML record of
// the container image digests and RIE version a project last ran with, per
// DESIGN.md's "Version pinning" decisions-log entry and
// plans/m5-extras.md's Unit C. Its whole point is reproducible-but-optional
// pinning: a project that has never run has no lock (nothing pinned, every
// image resolved by tag as before); the first successful run records
// digests as it goes.
//
// The file is intentionally forgiving — Load never fails a caller's flow.
// A missing lock is simply an empty one; a corrupt lock is also treated as
// empty, with the parse error returned so the caller can log a one-line
// warning and carry on unpinned, per the package's "never block a run"
// contract. Concurrent writers (e.g. two functions starting at once under
// `lambdary dev`) are serialized through an internal mutex, and every write
// is atomic (temp file + rename) so a crash mid-write can never leave a
// half-written lock behind.
package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// schemaVersion is the `version:` field Load/Save write, letting a future
// format change detect and migrate older lock files instead of silently
// misreading them.
const schemaVersion = 1

// dirName and fileName locate the lock file at
// <dir>/dirName/fileName — <config root>/.lambdary/lock, per
// plans/m5-extras.md's Unit C ("the dir containing lambdary.yml / the
// resolved root").
const (
	dirName  = ".lambdary"
	fileName = "lock"
)

// data is the on-disk shape of a lock file, per plans/m5-extras.md's Unit
// C: `version: 1`, `rie: v1.35`, `images: {<tag-ref>: <sha256 digest>}`.
// Field order matches declaration order under yaml.v3's Marshal, so a
// freshly written lock reads version/rie/images top to bottom.
type data struct {
	Version int               `yaml:"version"`
	RIE     string            `yaml:"rie,omitempty"`
	Images  map[string]string `yaml:"images,omitempty"`
}

// Lock is one project's `.lambdary/lock`, loaded (or freshly initialized)
// by Load. All methods are safe for concurrent use; every mutating method
// (RecordImage, SetRIE) saves before returning, per plans/m5-extras.md's
// Unit C preference for auto-save-on-record over a separate explicit-Save
// call from every writer.
type Lock struct {
	mu sync.Mutex

	dir  string
	data data
}

// Load reads <dir>/.lambdary/lock, returning a *Lock that is always usable
// even on error:
//
//   - No file yet: an empty Lock, nil error — the common case for a
//     project's first run.
//   - A file that exists but fails to parse (or read): an empty Lock plus
//     the error, so the caller can log a warning ("ignoring corrupt
//     .lambdary/lock: <err>") and proceed unpinned rather than fail the
//     run — Load itself never returns a nil *Lock.
func Load(dir string) (*Lock, error) {
	l := &Lock{dir: dir, data: data{Version: schemaVersion, Images: map[string]string{}}}

	path := l.path()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}

		return l, fmt.Errorf("lockfile: reading %s: %w", path, err)
	}

	var d data
	if err := yaml.Unmarshal(raw, &d); err != nil {
		return l, fmt.Errorf("lockfile: parsing %s: %w", path, err)
	}

	if d.Version == 0 {
		d.Version = schemaVersion
	}

	if d.Images == nil {
		d.Images = map[string]string{}
	}

	l.data = d

	return l, nil
}

// path returns the lock file's full path: <dir>/.lambdary/lock.
func (l *Lock) path() string {
	return filepath.Join(l.dir, dirName, fileName)
}

// ImageDigest returns the digest (e.g. "sha256:abcdef...") pinned for tag,
// and whether one was found.
func (l *Lock) ImageDigest(tag string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	digest, ok := l.data.Images[tag]

	return digest, ok
}

// RecordImage pins tag to digest and saves the lock. Per the package doc's
// "never block a run" contract, callers (see internal/backend/container)
// treat a non-nil error as best-effort-failed — worth a debug note, never
// a reason to undo an otherwise-successful start.
func (l *Lock) RecordImage(tag, digest string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.data.Images == nil {
		l.data.Images = map[string]string{}
	}

	l.data.Images[tag] = digest

	return l.saveLocked()
}

// SetRIE records the RIE version the process backend resolved (see
// internal/rie.Version) and saves the lock. Informational only — the
// process backend already pins the RIE it runs by the Version constant
// regardless of what's recorded here; the lock just documents it
// per-project, per DESIGN.md's version-pinning decision.
func (l *Lock) SetRIE(version string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.data.RIE = version

	return l.saveLocked()
}

// Save writes the lock's current in-memory state to disk. RecordImage and
// SetRIE already call it, so most callers never need to — it's exported
// mainly so tests (and any future writer that wants to batch several
// mutations before one save) can call it directly.
func (l *Lock) Save() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.saveLocked()
}

// saveLocked writes l.data to l.path() atomically (temp file in the same
// directory, then rename), creating the .lambdary directory if needed. It
// must be called with l.mu held.
func (l *Lock) saveLocked() error {
	path := l.path()
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("lockfile: creating %s: %w", dir, err)
	}

	if l.data.Version == 0 {
		l.data.Version = schemaVersion
	}

	out, err := yaml.Marshal(l.data)
	if err != nil {
		return fmt.Errorf("lockfile: encoding %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, "lock-*.tmp")
	if err != nil {
		return fmt.Errorf("lockfile: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(out); err != nil {
		tmp.Close() //nolint:errcheck // already failing; original error takes priority
		os.Remove(tmpPath)

		return fmt.Errorf("lockfile: writing %s: %w", tmpPath, err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)

		return fmt.Errorf("lockfile: closing %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of the temp file after a failed rename

		return fmt.Errorf("lockfile: renaming %s to %s: %w", tmpPath, path, err)
	}

	return nil
}
