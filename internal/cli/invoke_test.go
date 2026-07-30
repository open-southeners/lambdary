package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// TestNewInvokeCmd verifies the invoke command is properly configured.
func TestNewInvokeCmd(t *testing.T) {
	cmd := newInvokeCmd()
	if cmd.Use != "invoke <function>" {
		t.Errorf("expected Use 'invoke <function>', got %q", cmd.Use)
	}
	if cmd.RunE == nil {
		t.Errorf("expected RunE to be set")
	}
}

// TestResolveEvent checks event source resolution.
func TestResolveEvent(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		stdin   string
		want    string
		wantErr bool
	}{
		{
			name:   "default",
			source: "",
			want:   "{}",
		},
		{
			name:   "stdin",
			source: "-",
			stdin:  `{"key":"value"}`,
			want:   `{"key":"value"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdinReader := bytes.NewBufferString(tt.stdin)
			got, err := resolveEvent(tt.source, stdinReader)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveEvent error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if string(got) != tt.want {
				t.Errorf("resolveEvent got %q, want %q", string(got), tt.want)
			}
		})
	}
}

// TestDetectServer checks server detection by probing a non-existent server.
func TestDetectServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), serverProbeTimeout)
	defer cancel()

	// Probe a port that's likely not listening
	detected := detectServer(ctx, 59999)
	if detected {
		t.Errorf("detectServer on non-existent server returned true")
	}
}

// TestFunctionTimeout returns the correct timeout for a function.
func TestFunctionTimeout(t *testing.T) {
	// Test with nil manifest (default timeout)
	fn := discovery.Function{
		Manifest: nil,
	}
	timeout := functionTimeout(fn)
	if timeout != defaultInvokeHTTPTimeout {
		t.Errorf("expected default timeout %v, got %v", defaultInvokeHTTPTimeout, timeout)
	}
}

// TestFunctionTimeoutWithManifest tests timeout from manifest.
func TestFunctionTimeoutWithManifest(t *testing.T) {
	fn := discovery.Function{
		Manifest: &manifest.Manifest{
			Timeout: 120,
		},
	}
	timeout := functionTimeout(fn)
	expected := 120 * time.Second
	if timeout != expected {
		t.Errorf("expected timeout %v, got %v", expected, timeout)
	}
}
