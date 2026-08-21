package manifest

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Run("happy path with every field", func(t *testing.T) {
		c, err := LoadConfig("testdata/config_full.yml")
		if err != nil {
			t.Fatalf("LoadConfig() unexpected error: %v", err)
		}

		want := &Config{
			Port: 9000,
			Root: "./functions",
			Defaults: Defaults{
				Runtime:       "nodejs22.x",
				Handler:       "index.handler",
				Timeout:       15,
				Memory:        256,
				Architectures: []string{"arm64"},
				Layers:        []string{"arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1", "../shared-layer"},
				Environment:   map[string]string{"STAGE": "local"},
				URL:           URL{Payload: "1.0"},
				Local: Local{
					Backend: "container",
					Image:   "my-default-image:latest",
					Command: "/usr/local/bin/node",
					EnvFile: ".env.defaults",
				},
			},
		}

		if !reflect.DeepEqual(c, want) {
			t.Errorf("LoadConfig() = %#v, want %#v", c, want)
		}
	})

	t.Run("minimal config falls back to defaults", func(t *testing.T) {
		c, err := LoadConfig("testdata/config_minimal.yml")
		if err != nil {
			t.Fatalf("LoadConfig() unexpected error: %v", err)
		}

		if c.Port != DefaultPort {
			t.Errorf("Port = %d, want %d", c.Port, DefaultPort)
		}
		if c.Root != DefaultRoot {
			t.Errorf("Root = %q, want %q", c.Root, DefaultRoot)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := LoadConfig("testdata/does-not-exist.yml")
		if err == nil {
			t.Fatal("LoadConfig() expected error, got nil")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("LoadConfig() error = %v, want wrapping os.ErrNotExist", err)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		_, err := LoadConfig("testdata/config_unknown_key.yml")
		if err == nil {
			t.Fatal("LoadConfig() expected error, got nil")
		}
	})

	t.Run("port out of range", func(t *testing.T) {
		_, err := LoadConfig("testdata/config_invalid_port.yml")
		if err == nil {
			t.Fatal("LoadConfig() expected error, got nil")
		}
		if !errors.Is(err, ErrInvalidPort) {
			t.Errorf("LoadConfig() error = %v, want wrapping %v", err, ErrInvalidPort)
		}
	})

	t.Run("invalid defaults.local.backend", func(t *testing.T) {
		_, err := LoadConfig("testdata/config_invalid_defaults_backend.yml")
		if err == nil {
			t.Fatal("LoadConfig() expected error, got nil")
		}
		if !errors.Is(err, ErrInvalidBackend) {
			t.Errorf("LoadConfig() error = %v, want wrapping %v", err, ErrInvalidBackend)
		}
	})

	t.Run("invalid defaults.layers", func(t *testing.T) {
		_, err := LoadConfig("testdata/config_invalid_defaults_layers.yml")
		if err == nil {
			t.Fatal("LoadConfig() expected error, got nil")
		}
		if !errors.Is(err, ErrInvalidLayers) {
			t.Errorf("LoadConfig() error = %v, want wrapping %v", err, ErrInvalidLayers)
		}
		if !strings.Contains(err.Error(), "defaults.") {
			t.Errorf("LoadConfig() error = %v, want it prefixed with %q", err, "defaults.")
		}
	})
}

func TestConfigApplyDefaults(t *testing.T) {
	cfg := &Config{
		Defaults: Defaults{
			Runtime:       "nodejs22.x",
			Handler:       "index.handler",
			Timeout:       15,
			Memory:        256,
			Architectures: []string{"arm64"},
			Layers:        []string{"arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1", "../shared-layer"},
			Environment:   map[string]string{"STAGE": "local"},
			URL:           URL{Path: "/default", Payload: "1.0"},
			Local: Local{
				Backend: "container",
				Image:   "default-image:latest",
				Command: "default-command",
				EnvFile: ".env.default",
			},
		},
	}

	t.Run("fills unset fields", func(t *testing.T) {
		m := &Manifest{Name: "function_a"}
		cfg.ApplyDefaults(m)

		want := &Manifest{
			Name:          "function_a",
			Runtime:       "nodejs22.x",
			Handler:       "index.handler",
			Timeout:       15,
			Memory:        256,
			Architectures: []string{"arm64"},
			Layers:        []string{"arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1", "../shared-layer"},
			Environment:   map[string]string{"STAGE": "local"},
			URL:           URL{Path: "/default", Payload: "1.0"},
			Local: Local{
				Backend: "container",
				Image:   "default-image:latest",
				Command: "default-command",
				EnvFile: ".env.default",
			},
		}

		if !reflect.DeepEqual(m, want) {
			t.Errorf("ApplyDefaults() = %#v, want %#v", m, want)
		}
	})

	t.Run("manifest values are not overridden", func(t *testing.T) {
		m := &Manifest{
			Name:          "function_b",
			Runtime:       "python3.13",
			Handler:       "app.handler",
			Timeout:       5,
			Memory:        128,
			Architectures: []string{"x86_64"},
			Layers:        []string{"../own-layer"},
			Environment:   map[string]string{"OWN": "value"},
			URL:           URL{Path: "/function_b", Payload: "2.0"},
			Local: Local{
				Backend: "process",
				Image:   "own-image:latest",
				Command: "own-command",
				EnvFile: ".env.own",
			},
		}
		want := *m // shallow copy: nothing should change.

		cfg.ApplyDefaults(m)

		if !reflect.DeepEqual(m, &want) {
			t.Errorf("ApplyDefaults() changed a fully-set manifest: got %#v, want %#v", m, &want)
		}
	})
}

// TestDefaultingLayering exercises the full three-step chain documented on
// Manifest: Load, then (*Config).ApplyDefaults, then
// (*Manifest).ApplyBuiltinDefaults.
func TestDefaultingLayering(t *testing.T) {
	t.Run("project default payload wins over the built-in", func(t *testing.T) {
		cfg, err := LoadConfig("testdata/config_full.yml")
		if err != nil {
			t.Fatalf("LoadConfig() unexpected error: %v", err)
		}

		m, err := Load("testdata/minimal.yml")
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}

		cfg.ApplyDefaults(m)
		m.ApplyBuiltinDefaults()

		if m.URL.Payload != "1.0" {
			t.Errorf("URL.Payload = %q, want %q (from config defaults, not the %q built-in)", m.URL.Payload, "1.0", DefaultPayload)
		}
	})

	t.Run("explicit manifest payload survives both steps", func(t *testing.T) {
		cfg, err := LoadConfig("testdata/config_full.yml")
		if err != nil {
			t.Fatalf("LoadConfig() unexpected error: %v", err)
		}

		m, err := Load("testdata/full.yml")
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}

		cfg.ApplyDefaults(m)
		m.ApplyBuiltinDefaults()

		if m.URL.Payload != "2.0" {
			t.Errorf("URL.Payload = %q, want %q (the manifest's own value)", m.URL.Payload, "2.0")
		}
	})
}
