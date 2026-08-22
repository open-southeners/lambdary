package layers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/manifest"
)

// testRef is a layer version ARN ref good enough for fetch's tests: only
// Raw and Region are ever read (region only by sdkGetLayerVersion, which
// these tests never call).
var testRef = manifest.LayerRef{
	Kind:   manifest.LayerRefKindARN,
	Raw:    "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1",
	Region: "eu-west-1",
}

// serveZip starts an httptest server that serves body at "/", returning its
// URL — a stand-in for GetLayerVersion's presigned Content.Location.
func serveZip(t *testing.T, body []byte) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body) //nolint:errcheck // test server; nothing to do about a write error
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

// zipBytes builds a .zip archive in a temp file via the package's existing
// buildZip test helper and returns its contents, so fetch's tests can serve
// it over HTTP without needing a second zip-building helper.
func zipBytes(t *testing.T, entries []zipTestEntry) []byte {
	t.Helper()

	path := filepath.Join(t.TempDir(), "layer.zip")
	buildZip(t, path, entries)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading built zip: %v", err)
	}

	return data
}

func codeSha256Of(body []byte) string {
	sum := sha256.Sum256(body)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func TestFetch_HappyPath(t *testing.T) {
	cacheDir := t.TempDir()

	body := zipBytes(t, []zipTestEntry{
		{name: "bootstrap", content: []byte("#!/bin/sh\necho hi\n"), mode: 0o755},
		{name: "python/lib.py", content: []byte("value = 1\n"), mode: 0o644},
	})
	wantSha := codeSha256Of(body)
	location := serveZip(t, body)

	get := func(_ context.Context, ref manifest.LayerRef) (string, string, error) {
		return location, wantSha, nil
	}

	gotSha, err := fetch(context.Background(), get, testRef, cacheDir)
	if err != nil {
		t.Fatalf("fetch() unexpected error: %v", err)
	}

	if gotSha != wantSha {
		t.Errorf("fetch() sha = %q, want %q", gotSha, wantSha)
	}

	dest := CachePath(cacheDir, testRef)

	if got := readFile(t, filepath.Join(dest, "bootstrap")); got != "#!/bin/sh\necho hi\n" {
		t.Errorf("extracted bootstrap content = %q", got)
	}

	if got, ok := CachedDigest(cacheDir, testRef); !ok || got != wantSha {
		t.Errorf("CachedDigest() = %q, %v, want %q, true", got, ok, wantSha)
	}

	// No stray temp directories should be left behind alongside the cache
	// entry once fetch has renamed its work into place.
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Dir(dest), err)
	}

	for _, e := range entries {
		if e.Name() != filepath.Base(dest) {
			t.Errorf("stray entry left behind in cache dir: %s", e.Name())
		}
	}
}

func TestFetch_ShaMismatchLeavesNoCacheDir(t *testing.T) {
	cacheDir := t.TempDir()

	body := zipBytes(t, []zipTestEntry{{name: "bootstrap", content: []byte("hi\n"), mode: 0o755}})
	location := serveZip(t, body)

	get := func(_ context.Context, ref manifest.LayerRef) (string, string, error) {
		return location, "not-the-real-sha256==", nil
	}

	if _, err := fetch(context.Background(), get, testRef, cacheDir); err == nil {
		t.Fatal("fetch() expected error on CodeSha256 mismatch, got nil")
	}

	if _, err := os.Stat(CachePath(cacheDir, testRef)); !os.IsNotExist(err) {
		t.Errorf("CachePath() = exists after a sha mismatch, want absent (err=%v)", err)
	}
}

func TestFetch_DownloadForbidden(t *testing.T) {
	cacheDir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	get := func(_ context.Context, ref manifest.LayerRef) (string, string, error) {
		return srv.URL, "irrelevant", nil
	}

	_, err := fetch(context.Background(), get, testRef, cacheDir)
	if err == nil {
		t.Fatal("fetch() expected error on a 403 download, got nil")
	}

	if !strings.Contains(err.Error(), "403") {
		t.Errorf("fetch() error = %q, want it to mention the 403 status", err)
	}

	if _, err := os.Stat(CachePath(cacheDir, testRef)); !os.IsNotExist(err) {
		t.Errorf("CachePath() = exists after a failed download, want absent (err=%v)", err)
	}
}

func TestFetch_BadZipLeavesNoCacheDir(t *testing.T) {
	cacheDir := t.TempDir()

	// Not a real zip archive: extractZip will fail on it, simulating a
	// corrupt/truncated download that nonetheless passed the sha check
	// (e.g. AWS reported the sha of genuinely non-zip content).
	body := []byte("this is not a zip file")
	wantSha := codeSha256Of(body)
	location := serveZip(t, body)

	get := func(_ context.Context, ref manifest.LayerRef) (string, string, error) {
		return location, wantSha, nil
	}

	if _, err := fetch(context.Background(), get, testRef, cacheDir); err == nil {
		t.Fatal("fetch() expected an extraction error, got nil")
	}

	if _, err := os.Stat(CachePath(cacheDir, testRef)); !os.IsNotExist(err) {
		t.Errorf("CachePath() = exists after a failed extract, want absent (err=%v)", err)
	}

	// No stray temp extraction directory should survive the failure either.
	entries, err := os.ReadDir(filepath.Join(cacheDir, "layers"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading cache layers dir: %v", err)
	}

	for _, e := range entries {
		t.Errorf("stray entry left behind after failed extract: %s", e.Name())
	}
}

func TestFetch_GetLayerVersionErrorIsWrapped(t *testing.T) {
	cacheDir := t.TempDir()

	wantErr := errors.New("boom")
	get := func(_ context.Context, ref manifest.LayerRef) (string, string, error) {
		return "", "", wantErr
	}

	_, err := fetch(context.Background(), get, testRef, cacheDir)
	if err == nil {
		t.Fatal("fetch() expected error, got nil")
	}

	if !errors.Is(err, wantErr) {
		t.Errorf("fetch() error = %v, want it to wrap %v", err, wantErr)
	}

	if !strings.Contains(err.Error(), testRef.Raw) {
		t.Errorf("fetch() error = %q, want it to name the ARN %q", err, testRef.Raw)
	}
}

func TestClassifyFetchErr_CredentialsFailure(t *testing.T) {
	err := classifyFetchErr(testRef, errors.New("operation error Lambda: GetLayerVersionByArn, failed to retrieve credentials: some detail"))

	if !strings.Contains(err.Error(), "no AWS credentials found") {
		t.Errorf("classifyFetchErr() = %q, want it to mention missing credentials", err)
	}

	if !strings.Contains(err.Error(), testRef.Region) {
		t.Errorf("classifyFetchErr() = %q, want it to mention the region %q", err, testRef.Region)
	}
}

func TestCachedDigest_MissingMarker(t *testing.T) {
	cacheDir := t.TempDir()

	if _, ok := CachedDigest(cacheDir, testRef); ok {
		t.Error("CachedDigest() on an unfetched ref = found, want not found")
	}
}
