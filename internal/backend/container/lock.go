package container

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/open-southeners/lambdary/internal/backend"
)

// Lock is the subset of *internal/lockfile.Lock's API the container
// backend needs to pin and record image digests, per
// plans/m5-extras.md's Unit C. It's declared locally (rather than the
// container package importing internal/lockfile's concrete type in New's
// signature) so New keeps its existing zero-dependency shape and any type
// satisfying this small interface — real or fake — can be passed to
// NewWithLock.
type Lock interface {
	// ImageDigest returns the digest pinned for tag, and whether one was
	// found.
	ImageDigest(tag string) (string, bool)
	// RecordImage pins tag to digest, persisting the change. A non-nil
	// error is best-effort-failed from Start's point of view — see
	// recordDigest.
	RecordImage(tag, digest string) error
}

// NewWithLock is New plus an optional image-digest lock: when lock is
// non-nil, Start pins/records registry image digests through it per
// plans/m5-extras.md's Unit C (see the pinnable/usedDigest handling in
// Start); a nil lock is exactly New's existing, unpinned behaviour, so
// every pre-Unit-C call site keeps compiling and behaving unchanged.
func NewWithLock(cli string, runner backend.Runner, lock Lock) backend.Backend {
	return &containerBackend{cli: cli, runner: runner, lock: lock}
}

// digestRef renders image pinned to digest as a `<repo>@<digest>` reference
// Docker accepts in place of a tag.
func digestRef(image, digest string) string {
	return repoWithoutTag(image) + "@" + digest
}

// repoWithoutTag strips a trailing ":<tag>" from image. Only the final
// path segment (after the last "/") is checked for a colon, so a registry
// host:port (e.g. "localhost:5000/app:latest") isn't mistaken for a tag —
// image with no tag at all is returned unchanged.
func repoWithoutTag(image string) string {
	slash := strings.LastIndex(image, "/")
	segment := image[slash+1:]

	idx := strings.LastIndex(segment, ":")
	if idx == -1 {
		return image
	}

	return image[:slash+1+idx]
}

// parseRepoDigest extracts the "sha256:..." digest from a `docker inspect
// --format {{index .RepoDigests 0}}` output line shaped
// "<repo>@sha256:<hex>", reporting false for blank input or anything not
// shaped that way (e.g. an image with no RepoDigests entry, which the
// template renders as an empty string — a locally built image that was
// never pulled/pushed has no repo digest to pin).
func parseRepoDigest(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}

	idx := strings.LastIndex(s, "@")
	if idx == -1 || idx == len(s)-1 {
		return "", false
	}

	digest := s[idx+1:]
	if !strings.HasPrefix(digest, "sha256:") {
		return "", false
	}

	return digest, true
}

// recordDigest resolves image's repo digest via `<cli> inspect` and records
// it in b.lock, best-effort: any failure — the inspect command erroring, or
// the image having no RepoDigests entry at all — is noted on stderr and
// otherwise ignored. Start's own success must never be undone by a
// lock-recording failure, per plans/m5-extras.md's Unit C ("lockfile is
// best-effort and NEVER blocks a run").
func (b *containerBackend) recordDigest(ctx context.Context, image string) {
	out, err := b.runner.Run(ctx, b.cli, "inspect", "--format", "{{index .RepoDigests 0}}", image)
	if err != nil {
		fmt.Fprintf(os.Stderr, "container: resolving digest for %s: %s\n", image, err)
		return
	}

	digest, ok := parseRepoDigest(string(out))
	if !ok {
		return
	}

	if err := b.lock.RecordImage(image, digest); err != nil {
		fmt.Fprintf(os.Stderr, "container: recording digest for %s in .lambdary/lock: %s\n", image, err)
	}
}
