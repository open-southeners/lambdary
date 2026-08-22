package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/layers"
	"github.com/open-southeners/lambdary/internal/lockfile"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// fetchLayer is layers.Fetch, indirected through a package variable purely
// so tests in this package can swap in a fake — counting calls to verify
// "no network on a cache hit", asserting dedupe across functions, or
// returning an error to exercise the fail-safe warn-and-continue path —
// without internal/layers needing any test-only seam of its own. Never
// reassigned outside tests.
var fetchLayer = layers.Fetch

// ensureLayersFetched makes sure every layer version ARN referenced across
// fns is present in internal/layers' on-disk cache before any function
// Starts, per plans/layers.md's Unit E ("eager fetch at the discovery
// choke points"). It lives here — not in internal/layers itself — because
// it needs discovery.Function and lockfile.Lock, which that package
// deliberately stays free of.
//
// Call sites: dev's initial startup and every structural reload (both go
// through buildStack, which is the one place a fresh function set is
// produced), and invoke's standalone mode after its own discovery. Each
// dedupes and fetches once per ARN per call; there is no cross-call cache
// of "already tried this run" beyond what the on-disk cache and lock
// already provide.
//
// Fail-safe contract: a fetch failure is warned on errW and never aborts
// discovery or takes the caller down. The affected function simply fails
// later, at Start, with internal/layers' own "not cached" error — the same
// shape as any other backend-resolution failure (e.g. a bad image pull).
// lock is nil-able, matching every other lock-threading call site in this
// package; a nil lock still fetches, it just can't detect cache corruption
// or record what it fetched.
func ensureLayersFetched(ctx context.Context, fns []discovery.Function, cacheDir string, lock *lockfile.Lock, errW io.Writer) {
	for _, ref := range collectLayerARNRefs(fns) {
		ensureLayerFetched(ctx, ref, cacheDir, lock, errW)
	}
}

// collectLayerARNRefs gathers every LayerRefKindARN ref across fns'
// manifests, deduped by ARN and in first-seen order. Non-ARN entries (local
// paths) and parse errors are skipped: discovery already ran each
// manifest's own Validate, so a parse failure here would mean a bug
// upstream rather than something this helper should itself report.
func collectLayerARNRefs(fns []discovery.Function) []manifest.LayerRef {
	seen := map[string]bool{}

	var refs []manifest.LayerRef

	for _, fn := range fns {
		if fn.Manifest == nil {
			continue
		}

		for _, entry := range fn.Manifest.Layers {
			ref, err := manifest.ParseLayerRef(entry)
			if err != nil || ref.Kind != manifest.LayerRefKindARN {
				continue
			}

			if seen[ref.Raw] {
				continue
			}
			seen[ref.Raw] = true

			refs = append(refs, ref)
		}
	}

	return refs
}

// ensureLayerFetched makes sure ref's layer version is cached, fetching it
// via fetchLayer (layers.Fetch in production) when it isn't — or when the
// cache looks corrupt, see cacheMatchesLock — and records the result in
// lock. See ensureLayersFetched's doc comment for the fail-safe contract
// this upholds.
func ensureLayerFetched(ctx context.Context, ref manifest.LayerRef, cacheDir string, lock *lockfile.Lock, errW io.Writer) {
	cachePath := layers.CachePath(cacheDir, ref)

	if info, err := os.Stat(cachePath); err == nil && info.IsDir() && cacheMatchesLock(cacheDir, ref, lock, errW) {
		return
	}

	sha, err := fetchLayer(ctx, ref, cacheDir)
	if err != nil {
		fmt.Fprintf(errW, "warning: fetching layer %s: %s\n", ref.Raw, err)

		return
	}

	if lock == nil {
		return
	}

	if err := lock.RecordLayer(ref.Raw, sha); err != nil {
		fmt.Fprintf(errW, "warning: recording layer %s in .lambdary/lock: %s\n", ref.Raw, err)
	}
}

// cacheMatchesLock reports whether an existing cache directory at
// layers.CachePath(cacheDir, ref) can be trusted as-is without a re-fetch:
// either lock has no digest recorded for ref (including lock == nil —
// nothing to check against, so a bare existing cache dir is trusted, same
// as internal/layers.Stage's own cache-hit behavior), or its digest matches
// the marker layers.Fetch itself wrote there
// (layers.CachedDigest). A recorded digest with a missing or mismatched
// marker warns on errW and reports false, so the caller re-fetches — layer
// versions are immutable on AWS, so this only ever catches local cache
// corruption.
func cacheMatchesLock(cacheDir string, ref manifest.LayerRef, lock *lockfile.Lock, errW io.Writer) bool {
	if lock == nil {
		return true
	}

	lockDigest, ok := lock.LayerDigest(ref.Raw)
	if !ok {
		return true
	}

	if markerDigest, ok := layers.CachedDigest(cacheDir, ref); ok && markerDigest == lockDigest {
		return true
	}

	fmt.Fprintf(errW, "warning: layer cache for %s looks corrupt (missing or mismatched cache marker) — re-fetching\n", ref.Raw)

	return false
}
