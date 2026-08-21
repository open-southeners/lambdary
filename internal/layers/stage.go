package layers

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// copyTree copies the tree rooted at src into dst, creating dst if needed.
// Regular files keep the executable bit (and any other permission bits)
// from their source, so a layer's `bootstrap` or `bin/*` stays runnable
// once staged. Directories are created as needed; symlinks are rejected —
// see extractZip's doc comment for why the same rule applies to zip
// extraction, and dir-copy mirrors it here for consistency between the two
// layer forms.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlinks in layer content are not supported", path)
		}

		target := filepath.Join(dst, rel)

		if d.IsDir() {
			if rel == "." {
				return nil
			}

			return os.MkdirAll(target, 0o755)
		}

		if !d.Type().IsRegular() {
			// Not a file, directory, or symlink (e.g. a device or socket) —
			// nothing a Lambda layer legitimately contains; skip it rather
			// than fail the whole staging pass over it.
			return nil
		}

		return copyFile(path, target)
	})
}

// copyFile copies src to dst, preserving src's permission bits (including
// the executable bit), and creating dst's parent directory if needed.
func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("%s: %w", dst, err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("%s: %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // already failing; original error takes priority

		return fmt.Errorf("%s: copying to %s: %w", src, dst, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("%s: %w", dst, err)
	}

	// dst may already exist from an earlier layer in the merge (later
	// layers overwrite earlier ones' files, per the package doc). O_TRUNC
	// only truncates an existing file's content, not its mode, so the
	// OpenFile perm argument above is silently ignored on overwrite — an
	// explicit Chmod is required for "later layer wins" to also cover mode
	// bits (e.g. an executable bootstrap overwriting a non-executable one).
	// Do not remove this as a "redundant" cleanup.
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("%s: %w", dst, err)
	}

	return nil
}
