package layers

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// defaultFilePerm is used for a zip entry whose header carries no unix
// mode bits (e.g. an archive written by tooling that only ever calls
// zip.Writer.Create, which leaves CreatorVersion at 0). Without this
// fallback such an entry's fs.FileMode.Perm() is 0, which would stage an
// unreadable file.
const defaultFilePerm = 0o644

// extractZip extracts the .zip archive at zipPath into target, which must
// already exist. It is safe against zip-slip (an entry whose cleaned path
// would land outside target is rejected) and rejects symlink entries
// outright: a symlink inside an untrusted layer archive could point
// anywhere on the host filesystem, and real-world AWS layer zips (the ones
// this package targets — see plans/layers.md) have no legitimate need for
// one. If that assumption ever proves wrong, revisit this rejection rather
// than silently following symlinks. Regular files keep the permission bits
// (including the executable bit) recorded in the zip, so a layer's
// `bootstrap` or `bin/*` stays runnable once staged.
func extractZip(zipPath, target string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", zipPath, err)
	}
	defer r.Close()

	for _, f := range r.File {
		if err := extractZipEntry(f, target); err != nil {
			return fmt.Errorf("%s: %w", zipPath, err)
		}
	}

	return nil
}

// extractZipEntry extracts a single zip entry into target, enforcing the
// zip-slip and symlink rules documented on extractZip.
func extractZipEntry(f *zip.File, target string) error {
	if f.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("entry %q: symlinks in layer zips are not supported", f.Name)
	}

	cleaned := filepath.Clean(f.Name)
	if cleaned == "." {
		return nil
	}

	dest := filepath.Join(target, cleaned)

	if !isWithin(target, dest) {
		return fmt.Errorf("entry %q escapes the extraction target", f.Name)
	}

	if f.FileInfo().IsDir() {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return fmt.Errorf("entry %q: %w", f.Name, err)
		}

		return nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("entry %q: %w", f.Name, err)
	}

	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("entry %q: %w", f.Name, err)
	}
	defer rc.Close()

	perm := f.Mode().Perm()
	if perm == 0 {
		perm = defaultFilePerm
	}

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return fmt.Errorf("entry %q: %w", f.Name, err)
	}

	if _, err := io.Copy(out, rc); err != nil {
		out.Close() //nolint:errcheck // already failing; original error takes priority

		return fmt.Errorf("entry %q: %w", f.Name, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("entry %q: %w", f.Name, err)
	}

	// dest may already exist from an earlier layer in the merge (later
	// layers overwrite earlier ones' files, per the package doc). O_TRUNC
	// only truncates an existing file's content, not its mode, so the
	// OpenFile perm argument above is silently ignored on overwrite — an
	// explicit Chmod is required for "later layer wins" to also cover mode
	// bits (e.g. an executable bootstrap overwriting a non-executable one).
	// Do not remove this as a "redundant" cleanup.
	if err := os.Chmod(dest, perm); err != nil {
		return fmt.Errorf("entry %q: %w", f.Name, err)
	}

	return nil
}

// isWithin reports whether dest is target itself or a descendant of it,
// guarding against zip-slip entries (e.g. "../evil") that would otherwise
// write outside the extraction directory.
func isWithin(target, dest string) bool {
	rel, err := filepath.Rel(target, dest)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
