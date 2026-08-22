package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/layers"
	"github.com/open-southeners/lambdary/internal/lockfile"
	"github.com/open-southeners/lambdary/internal/manifest"
)

const testLayerARN = "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1"
const testLayerARN2 = "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:2"

// stubFetchLayer swaps the package's fetchLayer seam for calls, restoring
// the real layers.Fetch on test cleanup, and returns a *int counting calls.
func stubFetchLayer(t *testing.T, fn func(ctx context.Context, ref manifest.LayerRef, cacheDir string) (string, error)) *int {
	t.Helper()

	calls := 0
	fetchLayer = func(ctx context.Context, ref manifest.LayerRef, cacheDir string) (string, error) {
		calls++
		return fn(ctx, ref, cacheDir)
	}
	t.Cleanup(func() { fetchLayer = layers.Fetch })

	return &calls
}

// fnWithLayerARNs builds a discovery.Function whose manifest lists arns
// under layers:.
func fnWithLayerARNs(name string, arns ...string) discovery.Function {
	return discovery.Function{Name: name, Manifest: &manifest.Manifest{Layers: arns}}
}

func mustRef(t *testing.T, arn string) manifest.LayerRef {
	t.Helper()

	ref, err := manifest.ParseLayerRef(arn)
	if err != nil {
		t.Fatalf("ParseLayerRef(%q): %v", arn, err)
	}

	return ref
}

func TestEnsureLayersFetched_CacheHitNoNetwork(t *testing.T) {
	cacheDir := t.TempDir()
	ref := mustRef(t, testLayerARN)

	if err := os.MkdirAll(layers.CachePath(cacheDir, ref), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	calls := stubFetchLayer(t, func(context.Context, manifest.LayerRef, string) (string, error) {
		return "", errors.New("fetchLayer must not be called on a cache hit")
	})

	var errW bytes.Buffer
	ensureLayersFetched(context.Background(), []discovery.Function{fnWithLayerARNs("fn", testLayerARN)}, cacheDir, nil, &errW)

	if *calls != 0 {
		t.Errorf("fetchLayer call count = %d, want 0 (cache hit needs no network)", *calls)
	}

	if errW.Len() != 0 {
		t.Errorf("errW = %q, want empty (no warnings on a clean cache hit)", errW.String())
	}
}

func TestEnsureLayersFetched_CorruptionRefetch(t *testing.T) {
	cacheDir := t.TempDir()
	ref := mustRef(t, testLayerARN)

	if err := os.MkdirAll(layers.CachePath(cacheDir, ref), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// No marker file written inside the cache dir: it looks corrupt against
	// a lock digest that says otherwise.

	lock, err := lockfile.Load(t.TempDir())
	if err != nil {
		t.Fatalf("lockfile.Load: %v", err)
	}
	if err := lock.RecordLayer(testLayerARN, "sha-recorded-in-lock"); err != nil {
		t.Fatalf("RecordLayer: %v", err)
	}

	calls := stubFetchLayer(t, func(context.Context, manifest.LayerRef, string) (string, error) {
		return "sha-recorded-in-lock", nil
	})

	var errW bytes.Buffer
	ensureLayersFetched(context.Background(), []discovery.Function{fnWithLayerARNs("fn", testLayerARN)}, cacheDir, lock, &errW)

	if *calls != 1 {
		t.Errorf("fetchLayer call count = %d, want 1 (marker missing should force a re-fetch)", *calls)
	}

	if !strings.Contains(errW.String(), "corrupt") {
		t.Errorf("errW = %q, want a corruption warning", errW.String())
	}
}

func TestEnsureLayersFetched_FetchFailureWarnsAndContinues(t *testing.T) {
	cacheDir := t.TempDir()

	stubFetchLayer(t, func(_ context.Context, ref manifest.LayerRef, _ string) (string, error) {
		return "", errors.New("no credentials configured")
	})

	var errW bytes.Buffer

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ensureLayersFetched panicked: %v", r)
			}
		}()

		ensureLayersFetched(context.Background(), []discovery.Function{fnWithLayerARNs("fn", testLayerARN)}, cacheDir, nil, &errW)
	}()

	if !strings.Contains(errW.String(), testLayerARN) {
		t.Errorf("errW = %q, want it to name the ARN %q", errW.String(), testLayerARN)
	}

	if !strings.Contains(errW.String(), "no credentials configured") {
		t.Errorf("errW = %q, want it to include the underlying fetch error", errW.String())
	}

	if _, err := os.Stat(layers.CachePath(cacheDir, mustRef(t, testLayerARN))); !os.IsNotExist(err) {
		t.Errorf("CachePath exists after a failed fetch, want absent (err=%v)", err)
	}
}

func TestEnsureLayersFetched_DedupeAcrossFunctions(t *testing.T) {
	cacheDir := t.TempDir()

	calls := stubFetchLayer(t, func(_ context.Context, ref manifest.LayerRef, cacheDir string) (string, error) {
		if err := os.MkdirAll(layers.CachePath(cacheDir, ref), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		return "sha", nil
	})

	fns := []discovery.Function{
		fnWithLayerARNs("fn-a", testLayerARN, testLayerARN2),
		fnWithLayerARNs("fn-b", testLayerARN),
	}

	var errW bytes.Buffer
	ensureLayersFetched(context.Background(), fns, cacheDir, nil, &errW)

	if *calls != 2 {
		t.Errorf("fetchLayer call count = %d, want 2 (one per distinct ARN, deduped across functions)", *calls)
	}
}

func TestEnsureLayersFetched_NilLock(t *testing.T) {
	cacheDir := t.TempDir()

	calls := stubFetchLayer(t, func(_ context.Context, ref manifest.LayerRef, cacheDir string) (string, error) {
		if err := os.MkdirAll(layers.CachePath(cacheDir, ref), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		return "sha", nil
	})

	var errW bytes.Buffer

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ensureLayersFetched panicked with a nil lock: %v", r)
			}
		}()

		ensureLayersFetched(context.Background(), []discovery.Function{fnWithLayerARNs("fn", testLayerARN)}, cacheDir, nil, &errW)
	}()

	if *calls != 1 {
		t.Errorf("fetchLayer call count = %d, want 1", *calls)
	}

	if errW.Len() != 0 {
		t.Errorf("errW = %q, want empty on a successful fetch with a nil lock", errW.String())
	}
}

func TestCollectLayerARNRefs(t *testing.T) {
	fns := []discovery.Function{
		{Name: "no-manifest"},
		fnWithLayerARNs("with-arn-and-path", testLayerARN),
		{Name: "with-local-path", Manifest: &manifest.Manifest{Layers: []string{"../shared-layer"}}},
		fnWithLayerARNs("duplicate-arn", testLayerARN),
	}
	// mix a local path into one of the ARN-bearing functions too.
	fns[1].Manifest.Layers = append(fns[1].Manifest.Layers, "./vendor/layer.zip")

	refs := collectLayerARNRefs(fns)

	if len(refs) != 1 {
		t.Fatalf("collectLayerARNRefs() = %d refs, want 1 (deduped, local paths excluded): %+v", len(refs), refs)
	}

	if refs[0].Raw != testLayerARN {
		t.Errorf("collectLayerARNRefs()[0].Raw = %q, want %q", refs[0].Raw, testLayerARN)
	}
}

func TestCacheMatchesLock_NoDigestRecordedTrustsExistingCache(t *testing.T) {
	cacheDir := t.TempDir()

	lock, err := lockfile.Load(t.TempDir())
	if err != nil {
		t.Fatalf("lockfile.Load: %v", err)
	}

	var errW bytes.Buffer
	if !cacheMatchesLock(cacheDir, mustRef(t, testLayerARN), lock, &errW) {
		t.Error("cacheMatchesLock() = false, want true when lock has no digest recorded for this ARN")
	}

	if errW.Len() != 0 {
		t.Errorf("errW = %q, want empty", errW.String())
	}
}

func TestCacheMatchesLock_MatchingMarker(t *testing.T) {
	cacheDir := t.TempDir()
	ref := mustRef(t, testLayerARN)

	if err := os.MkdirAll(layers.CachePath(cacheDir, ref), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layers.CachePath(cacheDir, ref), ".codesha256"), []byte("the-sha"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	lock, err := lockfile.Load(t.TempDir())
	if err != nil {
		t.Fatalf("lockfile.Load: %v", err)
	}
	if err := lock.RecordLayer(testLayerARN, "the-sha"); err != nil {
		t.Fatalf("RecordLayer: %v", err)
	}

	var errW bytes.Buffer
	if !cacheMatchesLock(cacheDir, ref, lock, &errW) {
		t.Error("cacheMatchesLock() = false, want true when the marker matches the lock digest")
	}
}
