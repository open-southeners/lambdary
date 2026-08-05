package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/process"
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

// TestNeedsContainerFallback covers the gate invokeStandalone (and
// perFunctionBackend.resolve) checks before falling back to the container
// backend: only a process-kind resolution of a runtime process.Supports
// rejects needs the fallback.
func TestNeedsContainerFallback(t *testing.T) {
	tests := []struct {
		name string
		kind string
		fn   discovery.Function
		want bool
	}{
		{"process kind, unsupported runtime (java21) needs fallback", backendKindProcess, discovery.Function{Runtime: "java21"}, true},
		{"process kind, supported runtime (nodejs) does not need fallback", backendKindProcess, discovery.Function{Runtime: "nodejs22.x"}, false},
		{
			"process kind, local.command function does not need fallback regardless of runtime",
			backendKindProcess,
			discovery.Function{Runtime: "java21", Manifest: &manifest.Manifest{Local: manifest.Local{Command: "./my-custom-runtime"}}},
			false,
		},
		{"container kind never needs fallback, even for an unsupported runtime", backendKindContainer, discovery.Function{Runtime: "java21"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsContainerFallback(tt.kind, tt.fn); got != tt.want {
				t.Errorf("needsContainerFallback(%q, %+v) = %v, want %v", tt.kind, tt.fn, got, tt.want)
			}
		})
	}
}

// TestFallbackToContainerBackendStandalonePath covers invokeStandalone's use
// of the shared fallbackToContainerBackend helper (the same one
// perFunctionBackend.fallbackToContainer uses for dev): a successful
// container resolution returns it and fires notify exactly once; a failed
// one returns a combined error keeping process.ErrRuntimeNotSupported
// errors.Is-matchable alongside the container failure.
func TestFallbackToContainerBackendStandalonePath(t *testing.T) {
	fn := discovery.Function{Name: "legacy", Runtime: "java21"}

	t.Run("success returns the container backend and notifies once", func(t *testing.T) {
		containerB := &fakeManagerBackend{name: "container"}
		notified := 0

		got, err := fallbackToContainerBackend(context.Background(), fn, func(context.Context) (backend.Backend, error) {
			return containerB, nil
		}, func() {
			notified++
		})
		if err != nil {
			t.Fatalf("fallbackToContainerBackend() unexpected error: %v", err)
		}
		if got != containerB {
			t.Errorf("fallbackToContainerBackend() = %v, want the container backend", got)
		}
		if notified != 1 {
			t.Errorf("notify called %d times, want 1", notified)
		}
	})

	t.Run("container resolution failure combines both facts", func(t *testing.T) {
		containerErr := errors.New("no usable container runtime")

		_, err := fallbackToContainerBackend(context.Background(), fn, func(context.Context) (backend.Backend, error) {
			return nil, containerErr
		}, func() {
			t.Error("notify should not run when container resolution fails")
		})
		if !errors.Is(err, process.ErrRuntimeNotSupported) {
			t.Errorf("fallbackToContainerBackend() error = %v, want errors.Is process.ErrRuntimeNotSupported", err)
		}
		if !errors.Is(err, containerErr) {
			t.Errorf("fallbackToContainerBackend() error = %v, want errors.Is the container failure %v", err, containerErr)
		}
	})
}

// TestEffectiveInvokeBackendMode covers invoke standalone mode's
// per-function precedence: an explicit local.backend hint always wins over
// --backend, mirroring resolveManagerBackend's rule for `dev`.
func TestEffectiveInvokeBackendMode(t *testing.T) {
	tests := []struct {
		name        string
		fnBackend   string
		backendFlag string
		want        string
	}{
		{"auto hint defers to flag", "auto", "container", "container"},
		{"unset hint defers to flag", "", "process", "process"},
		{"explicit container hint wins over flag", "container", "process", "container"},
		{"explicit process hint wins over flag", "process", "container", "process"},
		{"explicit hint matching flag is a no-op", "container", "container", "container"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fn := discovery.Function{Backend: tt.fnBackend}
			if got := effectiveInvokeBackendMode(fn, tt.backendFlag); got != tt.want {
				t.Errorf("effectiveInvokeBackendMode(Backend=%q, flag=%q) = %q, want %q", tt.fnBackend, tt.backendFlag, got, tt.want)
			}
		})
	}
}
