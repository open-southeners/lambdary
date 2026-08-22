package layers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/smithy-go"

	"github.com/open-southeners/lambdary/internal/manifest"
)

// markerFileName is written inside CachePath by a successful Fetch,
// recording the CodeSha256 AWS reported for that layer version. It lets
// later callers (internal/cli's fetch-at-discovery wiring) detect local
// cache corruption — a cache directory present but its marker missing or
// not matching what a lock file recorded — without re-hashing the whole
// extracted tree.
const markerFileName = ".codesha256"

// CachedDigest reads the CodeSha256 marker Fetch left inside
// CachePath(cacheDir, ref), returning it and whether the marker file was
// present and readable. A cache directory with no marker (or one that
// fails to read) reports ("", false); callers treat that as "can't confirm
// this cache entry", not as a hard error.
func CachedDigest(cacheDir string, ref manifest.LayerRef) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(CachePath(cacheDir, ref), markerFileName))
	if err != nil {
		return "", false
	}

	return strings.TrimSpace(string(raw)), true
}

// layerVersionGetter is the single AWS API call Fetch makes
// (GetLayerVersionByArn), abstracted behind a function type so tests can
// inject a fake that returns an httptest server's URL as the download
// location instead of talking to real AWS. sdkGetLayerVersion (built from
// ref's own region — see its doc comment) is the only implementation that
// ever touches the SDK.
type layerVersionGetter func(ctx context.Context, ref manifest.LayerRef) (location, codeSha256 string, err error)

// Fetch resolves a layer version ARN ref against AWS Lambda's
// GetLayerVersion API — the same mechanism SAM CLI uses, which works for
// public third-party layers (e.g. Bref's PHP layers) with no special
// permissions — downloads and verifies its content, and extracts it into
// CachePath(cacheDir, ref) for internal/layers.Stage to find on a later
// call. It returns the verified CodeSha256 on success.
//
// Credentials and ambient region resolve through the SDK's standard
// default chain (config.LoadDefaultConfig): environment variables, shared
// config/credentials files, SSO, and container/EC2/ECS instance roles.
// ref.Region — the ARN's own region component — always wins over whatever
// ambient region that chain would otherwise pick, since third-party layer
// ARNs (Bref's included) are region-specific.
//
// Extraction happens into a temporary sibling directory that is renamed
// into place only once the whole zip has been downloaded, verified, and
// extracted successfully — mirroring internal/lockfile's temp-file-then-
// rename pattern — so a crash or error partway through can never leave a
// partially-extracted directory masquerading as a cache hit at CachePath.
func Fetch(ctx context.Context, ref manifest.LayerRef, cacheDir string) (string, error) {
	return fetch(ctx, sdkGetLayerVersion, ref, cacheDir)
}

// fetch is Fetch's testable core: get replaces the real SDK call in tests.
func fetch(ctx context.Context, get layerVersionGetter, ref manifest.LayerRef, cacheDir string) (string, error) {
	location, codeSha256, err := get(ctx, ref)
	if err != nil {
		return "", classifyFetchErr(ref, err)
	}

	dest := CachePath(cacheDir, ref)
	parent := filepath.Dir(dest)

	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("layers: fetching %s: creating %s: %w", ref.Raw, parent, err)
	}

	zipPath, err := downloadAndVerify(ctx, location, codeSha256, ref, parent)
	if err != nil {
		return "", err
	}
	defer os.Remove(zipPath) //nolint:errcheck // best-effort cleanup of the downloaded zip once extracted

	tmpDir, err := os.MkdirTemp(parent, sanitizeARN(ref.Raw)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("layers: fetching %s: creating temporary extraction directory: %w", ref.Raw, err)
	}

	renamed := false
	defer func() {
		if !renamed {
			os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup after a failed extract/rename
		}
	}()

	if err := extractZip(zipPath, tmpDir); err != nil {
		return "", fmt.Errorf("layers: fetching %s: extracting: %w", ref.Raw, err)
	}

	if err := os.WriteFile(filepath.Join(tmpDir, markerFileName), []byte(codeSha256), 0o644); err != nil {
		return "", fmt.Errorf("layers: fetching %s: writing cache marker: %w", ref.Raw, err)
	}

	// Only reached once the extract above fully succeeded, so a stale or
	// corrupt previous cache directory (the re-fetch-after-corruption case)
	// is replaced atomically alongside the rename below, never left half
	// removed.
	if err := os.RemoveAll(dest); err != nil {
		return "", fmt.Errorf("layers: fetching %s: clearing previous cache directory %s: %w", ref.Raw, dest, err)
	}

	if err := os.Rename(tmpDir, dest); err != nil {
		return "", fmt.Errorf("layers: fetching %s: %w", ref.Raw, err)
	}
	renamed = true

	return codeSha256, nil
}

// downloadAndVerify downloads location (the presigned S3 URL
// GetLayerVersion returned — already signed, so a plain GET needs no
// further signing) into a temp file under parentDir, verifying its SHA-256
// against wantCodeSha256 (base64-encoded, AWS's own encoding for
// Content.CodeSha256) as it streams. It returns the temp file's path on
// success; the caller is responsible for removing it.
func downloadAndVerify(ctx context.Context, location, wantCodeSha256 string, ref manifest.LayerRef, parentDir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return "", fmt.Errorf("layers: fetching %s: building download request: %w", ref.Raw, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("layers: fetching %s: downloading content: %w", ref.Raw, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("layers: fetching %s: downloading content: server responded %d", ref.Raw, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(parentDir, "download-*.zip")
	if err != nil {
		return "", fmt.Errorf("layers: fetching %s: creating download temp file: %w", ref.Raw, err)
	}
	tmpPath := tmp.Name()

	hasher := sha256.New()

	if _, err := io.Copy(tmp, io.TeeReader(resp.Body, hasher)); err != nil {
		tmp.Close() //nolint:errcheck // already failing; original error takes priority
		os.Remove(tmpPath)

		return "", fmt.Errorf("layers: fetching %s: downloading content: %w", ref.Raw, err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)

		return "", fmt.Errorf("layers: fetching %s: %w", ref.Raw, err)
	}

	got := base64.StdEncoding.EncodeToString(hasher.Sum(nil))
	if got != wantCodeSha256 {
		os.Remove(tmpPath)

		return "", fmt.Errorf("layers: fetching %s: downloaded content's CodeSha256 %s does not match what AWS reported (%s) — the download may be corrupt or truncated", ref.Raw, got, wantCodeSha256)
	}

	return tmpPath, nil
}

// classifyFetchErr wraps a GetLayerVersionByArn error with an actionable
// message per plans/layers.md's Unit E, when the error matches one of the
// two cases worth calling out specifically; anything else is wrapped
// plainly.
func classifyFetchErr(ref manifest.LayerRef, err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && strings.Contains(apiErr.ErrorCode(), "AccessDenied") {
		return fmt.Errorf("layers: fetching %s: access denied — this layer may be private to another AWS account; check that your credentials are authorized for lambda:GetLayerVersion on it: %w", ref.Raw, err)
	}

	// The SDK doesn't fail fast when no credentials are configured at all —
	// it defers to request-signing time, where a failed credential
	// resolution surfaces as this wrapped message from
	// aws/signer/v4.SigningError.
	if strings.Contains(err.Error(), "failed to retrieve credentials") {
		return fmt.Errorf("layers: fetching %s: no AWS credentials found for region %s — the SDK checked its standard chain (env vars, shared config/credentials files, SSO, container/EC2/ECS instance role): %w", ref.Raw, ref.Region, err)
	}

	return fmt.Errorf("layers: fetching %s: %w", ref.Raw, err)
}

// sdkGetLayerVersion is Fetch's production layerVersionGetter: it builds an
// SDK client whose region is ref.Region (the ARN's own region, which wins
// over ambient config — see Fetch's doc comment) and calls
// GetLayerVersionByArn, the right call for a full layer *version* ARN
// (rather than GetLayerVersion, which takes a layer name and a separate
// version number).
func sdkGetLayerVersion(ctx context.Context, ref manifest.LayerRef) (string, string, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(ref.Region))
	if err != nil {
		return "", "", fmt.Errorf("loading AWS SDK configuration: %w", err)
	}

	client := lambda.NewFromConfig(cfg)

	out, err := client.GetLayerVersionByArn(ctx, &lambda.GetLayerVersionByArnInput{
		Arn: aws.String(ref.Raw),
	})
	if err != nil {
		return "", "", err
	}

	if out.Content == nil || out.Content.Location == nil || out.Content.CodeSha256 == nil {
		return "", "", fmt.Errorf("GetLayerVersionByArn returned no downloadable content for %s", ref.Raw)
	}

	return aws.ToString(out.Content.Location), aws.ToString(out.Content.CodeSha256), nil
}
