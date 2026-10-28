package process

import (
	"testing"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// TestSupportsGoFunction covers the Go-build carve-out: a provided.*
// function with a GoSource has its bootstrap built on the host at Start, so
// the layers + no local ./bootstrap fallback to the container backend does
// not apply.
func TestSupportsGoFunction(t *testing.T) {
	goSrc := &discovery.GoSource{ModuleDir: "/src/mod", Package: "./cmd/ping"}
	withLayers := &manifest.Manifest{Layers: []string{"arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1"}}

	t.Run("Go function with layers and no bootstrap is supported", func(t *testing.T) {
		fn := discovery.Function{Runtime: "provided.al2023", Dir: t.TempDir(), Manifest: withLayers, Go: goSrc}

		if !Supports(fn) {
			t.Error("Supports() = false, want true for a Go-built function")
		}
		if RequiresLayerBootstrap(fn) {
			t.Error("RequiresLayerBootstrap() = true, want false for a Go-built function")
		}
	})

	t.Run("Go function without layers is supported", func(t *testing.T) {
		fn := discovery.Function{Runtime: "provided.al2023", Dir: t.TempDir(), Manifest: &manifest.Manifest{}, Go: goSrc}

		if !Supports(fn) {
			t.Error("Supports() = false, want true")
		}
	})

	t.Run("same function without a GoSource keeps the carve-out", func(t *testing.T) {
		fn := discovery.Function{Runtime: "provided.al2023", Dir: t.TempDir(), Manifest: withLayers}

		if Supports(fn) {
			t.Error("Supports() = true, want false (no GoSource, layers, no bootstrap)")
		}
		if !RequiresLayerBootstrap(fn) {
			t.Error("RequiresLayerBootstrap() = false, want true")
		}
	})
}
