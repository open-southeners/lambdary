package manifest

import (
	"testing"
)

func TestParseLayerRef(t *testing.T) {
	tests := []struct {
		name       string
		entry      string
		wantKind   LayerRefKind
		wantRegion string
		wantErr    bool
	}{
		{
			name:       "valid layer version ARN",
			entry:      "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1",
			wantKind:   LayerRefKindARN,
			wantRegion: "eu-west-1",
		},
		{
			name:     "local directory path",
			entry:    "../shared-layer",
			wantKind: LayerRefKindPath,
		},
		{
			name:     "local zip path",
			entry:    "./vendor/layer.zip",
			wantKind: LayerRefKindPath,
		},
		{
			name:    "empty string",
			entry:   "",
			wantErr: true,
		},
		{
			name:    "versionless layer ARN",
			entry:   "arn:aws:lambda:eu-west-1:534081306603:layer:php-83",
			wantErr: true,
		},
		{
			name:    "non-numeric version",
			entry:   "arn:aws:lambda:eu-west-1:534081306603:layer:php-83:latest",
			wantErr: true,
		},
		{
			name:    "wrong service ARN",
			entry:   "arn:aws:s3:::bucket",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := ParseLayerRef(tc.entry)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseLayerRef(%q) expected error, got nil", tc.entry)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseLayerRef(%q) unexpected error: %v", tc.entry, err)
			}

			if ref.Kind != tc.wantKind {
				t.Errorf("ParseLayerRef(%q).Kind = %q, want %q", tc.entry, ref.Kind, tc.wantKind)
			}
			if ref.Raw != tc.entry {
				t.Errorf("ParseLayerRef(%q).Raw = %q, want %q", tc.entry, ref.Raw, tc.entry)
			}
			if ref.Region != tc.wantRegion {
				t.Errorf("ParseLayerRef(%q).Region = %q, want %q", tc.entry, ref.Region, tc.wantRegion)
			}
		})
	}
}

func TestValidateLayers(t *testing.T) {
	t.Run("at most 5 entries", func(t *testing.T) {
		layers := []string{"a", "b", "c", "d", "e", "f"}
		if err := validateLayers(layers); err == nil {
			t.Fatal("validateLayers() expected error for 6 entries, got nil")
		}
	})

	t.Run("5 entries is fine", func(t *testing.T) {
		layers := []string{"a", "b", "c", "d", "e"}
		if err := validateLayers(layers); err != nil {
			t.Errorf("validateLayers() unexpected error: %v", err)
		}
	})

	t.Run("no entries is fine", func(t *testing.T) {
		if err := validateLayers(nil); err != nil {
			t.Errorf("validateLayers() unexpected error: %v", err)
		}
	})
}
