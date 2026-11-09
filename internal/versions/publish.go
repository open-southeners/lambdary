package versions

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/layers"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// snapshotExcludes are never part of a function's frozen code: VCS and cache
// directories, and the manifest, whose settings the snapshot carries resolved.
var snapshotExcludes = map[string]bool{
	".git":        true,
	".lambdary":   true,
	"__pycache__": true,
	".lambda.yml": true,
}

// frozenLayerRef is a version's single layers: entry, relative to its code
// directory.
const frozenLayerRef = "../layers"

// Publish freezes fn, a discovered $LATEST function, as a new version. As on
// AWS, nothing is created when code and configuration match the most recent
// version: that version is returned instead. build compiles a Go function's
// bootstrap, and may be nil when fn is not one.
func (s *Store) Publish(ctx context.Context, fn discovery.Function, in PublishInput, build BootstrapBuild) (Version, error) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	s.mu.RLock()
	st, err := s.load(fn.Name)
	s.mu.RUnlock()
	if err != nil {
		return Version{}, err
	}

	if in.RevisionID != "" && in.RevisionID != st.RevisionID {
		return Version{}, fmt.Errorf("%w: function revision id %s does not match the supplied one", ErrPrecondition, st.RevisionID)
	}

	root := s.functionDir(fn.Name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Version{}, fmt.Errorf("versions: %s: %w", fn.Name, err)
	}

	tmp, err := os.MkdirTemp(root, ".publish-*")
	if err != nil {
		return Version{}, fmt.Errorf("versions: %s: %w", fn.Name, err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // already renamed away on success.

	m, err := s.snapshot(ctx, fn, tmp, build)
	if err != nil {
		return Version{}, fmt.Errorf("versions: %s: %w", fn.Name, err)
	}

	codeSha, codeSize, err := hashTree(tmp)
	if err != nil {
		return Version{}, fmt.Errorf("versions: %s: %w", fn.Name, err)
	}

	if in.CodeSha256 != "" && in.CodeSha256 != codeSha {
		return Version{}, fmt.Errorf("%w: CodeSha256 %s does not match the function's code %s", ErrPrecondition, in.CodeSha256, codeSha)
	}

	configSha, err := hashConfig(fn, m)
	if err != nil {
		return Version{}, fmt.Errorf("versions: %s: %w", fn.Name, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Reloaded under the write lock: alias changes may have landed while the
	// snapshot was being taken.
	st, err = s.load(fn.Name)
	if err != nil {
		return Version{}, err
	}

	if n := len(st.Versions); n > 0 {
		last := st.Versions[n-1]
		if last.CodeSha256 == codeSha && last.ConfigSha256 == configSha {
			return last, nil
		}
	}

	number := st.nextVersion()
	dest := filepath.Join(root, number)

	// A crash between this rename and save leaves an orphan with this number.
	if err := os.RemoveAll(dest); err != nil {
		return Version{}, fmt.Errorf("versions: %s: %w", fn.Name, err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		return Version{}, fmt.Errorf("versions: %s: %w", fn.Name, err)
	}

	v := Version{
		Version:      number,
		Description:  in.Description,
		CodeSha256:   codeSha,
		CodeSize:     codeSize,
		ConfigSha256: configSha,
		RevisionID:   newRevisionID(),
		LastModified: s.now().UTC(),
		Runtime:      fn.Runtime,
		Handler:      fn.Handler,
		Backend:      fn.Backend,
		Image:        fn.Image,
		Manifest:     m,
	}

	st.Versions = append(st.Versions, v)
	st.NextVersion++
	st.RevisionID = newRevisionID()

	if err := s.save(fn.Name, st); err != nil {
		return Version{}, err
	}

	return v, nil
}

// snapshot writes fn's code and layers under dir and returns the manifest the
// version runs with: env_file folded into environment, and layers pointing at
// the frozen copy, so nothing refers back to mutable files.
func (s *Store) snapshot(ctx context.Context, fn discovery.Function, dir string, build BootstrapBuild) (*manifest.Manifest, error) {
	absDir, err := filepath.Abs(fn.Dir)
	if err != nil {
		return nil, err
	}

	m := &manifest.Manifest{}
	if fn.Manifest != nil {
		*m = *fn.Manifest
	}

	code := filepath.Join(dir, "code")

	if err := os.MkdirAll(code, 0o755); err != nil {
		return nil, err
	}

	if isCompiledGo(fn, absDir) {
		// As in a real Lambda zip, a compiled function's code is its binary;
		// its source directory holds nothing it reads at runtime. A
		// local.command runs from the directory, so that case keeps it.
		if m.Local.Command != "" {
			if err := copyCode(absDir, code); err != nil {
				return nil, err
			}
		}

		if build == nil {
			return nil, errors.New("this function's backend cannot build a Go bootstrap")
		}

		if err := build(ctx, fn, filepath.Join(code, "bootstrap")); err != nil {
			return nil, err
		}
	} else if err := copyCode(absDir, code); err != nil {
		return nil, err
	}

	if m.Local.EnvFile != "" {
		if err := foldEnvFile(m, absDir); err != nil {
			return nil, err
		}
	}

	if len(m.Layers) > 0 {
		if err := s.freezeLayers(fn, m, absDir, dir); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// isCompiledGo reports whether fn's bootstrap is compiled from Go source. A
// Dockerfile takes priority, matching the container backend.
func isCompiledGo(fn discovery.Function, absDir string) bool {
	if fn.Go == nil {
		return false
	}

	_, err := os.Stat(filepath.Join(absDir, "Dockerfile"))

	return err != nil
}

// foldEnvFile merges local.env_file into m.Environment, with the manifest
// winning on a conflict, as it does when the backends start $LATEST.
func foldEnvFile(m *manifest.Manifest, absDir string) error {
	path := m.Local.EnvFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(absDir, path)
	}

	env, err := backend.ParseEnvFile(path)
	if err != nil {
		return fmt.Errorf("loading local.env_file: %w", err)
	}

	merged := make(map[string]string, len(env)+len(m.Environment))
	for k, v := range env {
		merged[k] = v
	}
	for k, v := range m.Environment {
		merged[k] = v
	}

	m.Environment = merged
	m.Local.EnvFile = ""

	return nil
}

// freezeLayers stages fn's layers once and moves the result into the
// snapshot, leaving the version with one local layer: the frozen copy.
func (s *Store) freezeLayers(fn discovery.Function, m *manifest.Manifest, absDir, dir string) error {
	refs := make([]manifest.LayerRef, 0, len(m.Layers))

	for _, raw := range m.Layers {
		ref, err := manifest.ParseLayerRef(raw)
		if err != nil {
			return fmt.Errorf("layer %q: %w", raw, err)
		}

		refs = append(refs, ref)
	}

	staged, err := layers.Stage(fn.Name+"@publish", refs, absDir, s.cacheDir)
	if err != nil {
		return err
	}

	if err := os.Rename(staged, filepath.Join(dir, "layers")); err != nil {
		return err
	}

	m.Layers = []string{frozenLayerRef}

	return nil
}

// copyCode copies a function directory into dst, skipping snapshotExcludes.
// Symlinks are recreated rather than followed: node_modules/.bin relies on
// them, and following one could copy far more than the function.
func copyCode(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		if rel == "." {
			return nil
		}

		if snapshotExcludes[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		target := filepath.Join(dst, rel)

		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}

			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return copyFile(path, target)
		default:
			return nil
		}
	})
}

// copyFile copies a regular file, keeping its permission bits.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // already failing.
		return err
	}

	return out.Close()
}

// hashTree returns the base64 SHA-256 AWS's CodeSha256 is spelled in, and the
// byte size, of every file under dir. The hash covers relative paths, the
// executable bit, symlink targets and contents, walked in sorted order, so it
// is independent of where the tree lives.
func hashTree(dir string) (string, int64, error) {
	var paths []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			paths = append(paths, path)
		}

		return nil
	})
	if err != nil {
		return "", 0, err
	}

	sort.Strings(paths)

	h := sha256.New()

	var size int64

	for _, path := range paths {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return "", 0, err
		}

		info, err := os.Lstat(path)
		if err != nil {
			return "", 0, err
		}

		fmt.Fprintf(h, "%s\x00%t\x00", filepath.ToSlash(rel), info.Mode()&0o111 != 0)

		if info.Mode()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return "", 0, err
			}

			fmt.Fprintf(h, "%s\x00", link)

			continue
		}

		if !info.Mode().IsRegular() {
			continue
		}

		f, err := os.Open(path)
		if err != nil {
			return "", 0, err
		}

		n, err := io.Copy(h, f)
		f.Close() //nolint:errcheck // read-only.
		if err != nil {
			return "", 0, err
		}

		size += n
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil)), size, nil
}

// hashConfig hashes everything about fn besides its code that a version
// freezes. encoding/json sorts map keys, so equal configs hash equally.
func hashConfig(fn discovery.Function, m *manifest.Manifest) (string, error) {
	data, err := json.Marshal(struct {
		Runtime, Handler, Backend, Image string
		Manifest                         *manifest.Manifest
	}{fn.Runtime, fn.Handler, fn.Backend, fn.Image, m})
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return base64.StdEncoding.EncodeToString(sum[:]), nil
}
