package manifest

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// maxLayers is AWS Lambda's own limit on the number of layers a function
// may reference. See plans/layers.md Unit A.
const maxLayers = 5

// ErrInvalidLayers indicates a layers entry (or the layers list itself) is
// invalid: more than maxLayers entries, or an entry that isn't a valid
// layer version ARN or local path. See ParseLayerRef for the ARN/local-path
// distinction.
var ErrInvalidLayers = errors.New("layers must contain at most 5 entries, each a valid layer version ARN or local path")

// LayerRefKind distinguishes the two forms a layers: entry can take.
type LayerRefKind string

const (
	// LayerRefKindARN is a layer version ARN, e.g.
	// "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1".
	LayerRefKindARN LayerRefKind = "arn"
	// LayerRefKindPath is a local path (a directory or a .zip file)
	// relative to the function directory. Existence is checked later, at
	// staging time (internal/layers), not here — parsing does no I/O,
	// matching how this package already treats local.env_file.
	LayerRefKindPath LayerRefKind = "path"
)

// LayerRef is a single layers: entry, classified by ParseLayerRef.
type LayerRef struct {
	// Kind is LayerRefKindARN or LayerRefKindPath.
	Kind LayerRefKind
	// Raw is the entry exactly as written in the manifest: the full ARN,
	// or the local path.
	Raw string
	// Region is the ARN's region component (e.g. "eu-west-1"). Only set
	// when Kind is LayerRefKindARN. Unit E uses it to pick the SDK region
	// for GetLayerVersion, since third-party layer ARNs (e.g. Bref's) are
	// region-specific and the ARN's own region wins over the ambient one.
	Region string
}

// layerARNPattern matches a Lambda layer *version* ARN:
//
//	arn:<partition>:lambda:<region>:<account-id>:layer:<name>:<version>
//
// The partition group accepts "aws" and its alternates ("aws-us-gov",
// "aws-cn", ...) — GovCloud and China-region layer ARNs are valid on real
// Lambda, and the SDK resolves the right endpoint from the region string
// alone, so no other field needs to know which partition a ref came from.
// The account id must be exactly 12 digits and the version must be
// numeric. This deliberately rejects versionless layer ARNs (e.g.
// "arn:aws:lambda:eu-west-1:534081306603:layer:php-83"), which
// GetLayerVersion cannot use.
var layerARNPattern = regexp.MustCompile(`^arn:aws(?:-[a-z]+)*:lambda:([a-z0-9-]+):(\d{12}):layer:([A-Za-z0-9_-]+):(\d+)$`)

// ParseLayerRef classifies a single layers: entry as a layer version ARN or
// a local path. It does no I/O: local-path existence is checked later, at
// staging time (internal/layers), not here.
func ParseLayerRef(s string) (LayerRef, error) {
	if s == "" {
		return LayerRef{}, errors.New("layer entry must not be empty")
	}

	if !strings.HasPrefix(s, "arn:") {
		return LayerRef{Kind: LayerRefKindPath, Raw: s}, nil
	}

	m := layerARNPattern.FindStringSubmatch(s)
	if m == nil {
		return LayerRef{}, fmt.Errorf("not a valid layer version ARN (want arn:aws:lambda:<region>:<account-id>:layer:<name>:<version>): %q", s)
	}

	return LayerRef{Kind: LayerRefKindARN, Raw: s, Region: m[1]}, nil
}

// validateLayers checks the rules shared by Manifest.Layers and
// Defaults.Layers: at most maxLayers entries, each parseable by
// ParseLayerRef.
func validateLayers(layers []string) error {
	if len(layers) > maxLayers {
		return fmt.Errorf("%w: got %d entries, want at most %d", ErrInvalidLayers, len(layers), maxLayers)
	}

	for _, entry := range layers {
		if _, err := ParseLayerRef(entry); err != nil {
			return fmt.Errorf("%w: %q: %s", ErrInvalidLayers, entry, err)
		}
	}

	return nil
}
