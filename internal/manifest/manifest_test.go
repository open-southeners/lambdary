package manifest

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Run("happy path with every field", func(t *testing.T) {
		m, err := Load("testdata/full.yml")
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}

		want := &Manifest{
			Name:          "function_a",
			Runtime:       "nodejs22.x",
			Handler:       "index.handler",
			Timeout:       30,
			Memory:        512,
			Architectures: []string{"arm64"},
			Environment:   map[string]string{"TABLE_NAME": "local-table"},
			URL:           URL{Path: "/function_a", Payload: "2.0"},
			Local: Local{
				Backend: "auto",
				Image:   "my-custom-image:latest",
				Command: "/usr/local/bin/node",
				EnvFile: ".env",
			},
		}

		if !reflect.DeepEqual(m, want) {
			t.Errorf("Load() = %#v, want %#v", m, want)
		}
	})

	t.Run("minimal manifest with only comments", func(t *testing.T) {
		m, err := Load("testdata/minimal.yml")
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}

		want := &Manifest{}
		if !reflect.DeepEqual(m, want) {
			t.Errorf("Load() = %#v, want %#v", m, want)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		m, err := Load("testdata/empty.yml")
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}

		want := &Manifest{}
		if !reflect.DeepEqual(m, want) {
			t.Errorf("Load() = %#v, want %#v", m, want)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := Load("testdata/does-not-exist.yml")
		if err == nil {
			t.Fatal("Load() expected error, got nil")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Load() error = %v, want wrapping os.ErrNotExist", err)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		_, err := Load("testdata/unknown_key.yml")
		if err == nil {
			t.Fatal("Load() expected error, got nil")
		}
	})

	validationCases := []struct {
		name    string
		path    string
		wantErr error
	}{
		{"invalid backend", "testdata/invalid_backend.yml", ErrInvalidBackend},
		{"negative timeout", "testdata/invalid_timeout.yml", ErrInvalidTimeout},
		{"negative memory", "testdata/invalid_memory.yml", ErrInvalidMemory},
		{"url path without leading slash", "testdata/invalid_url_path.yml", ErrInvalidURLPath},
	}

	for _, tc := range validationCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.path)
			if err == nil {
				t.Fatalf("Load(%q) expected error, got nil", tc.path)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Load(%q) error = %v, want wrapping %v", tc.path, err, tc.wantErr)
			}
		})
	}
}

func TestManifestApplyBuiltinDefaults(t *testing.T) {
	t.Run("fills empty payload", func(t *testing.T) {
		m := &Manifest{Name: "function_a"}
		m.ApplyBuiltinDefaults()

		if m.URL.Payload != DefaultPayload {
			t.Errorf("URL.Payload = %q, want %q", m.URL.Payload, DefaultPayload)
		}
	})

	t.Run("explicit payload survives", func(t *testing.T) {
		m := &Manifest{Name: "function_a", URL: URL{Payload: "1.0"}}
		m.ApplyBuiltinDefaults()

		if m.URL.Payload != "1.0" {
			t.Errorf("URL.Payload = %q, want %q", m.URL.Payload, "1.0")
		}
	})
}
