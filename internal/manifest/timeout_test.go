package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadManifestYAML writes content to a .lambda.yml in a fresh temp dir and
// loads it.
func loadManifestYAML(t *testing.T, content string) (*Manifest, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".lambda.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}

	return Load(path)
}

func TestTimeoutParsing(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantSet bool
		want    int
	}{
		{"unset", "runtime: nodejs22.x\n", false, DefaultTimeout},
		{"explicit zero", "timeout: 0\n", true, 0},
		{"value", "timeout: 30\n", true, 30},
		{"above the AWS maximum", "timeout: 1800\n", true, 1800},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := loadManifestYAML(t, tc.yaml)
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}

			if (m.Timeout != nil) != tc.wantSet {
				t.Errorf("Timeout set = %v, want %v", m.Timeout != nil, tc.wantSet)
			}

			if got := m.TimeoutSeconds(); got != tc.want {
				t.Errorf("TimeoutSeconds() = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("negative is rejected", func(t *testing.T) {
		if _, err := loadManifestYAML(t, "timeout: -1\n"); !errors.Is(err, ErrInvalidTimeout) {
			t.Errorf("Load() error = %v, want ErrInvalidTimeout", err)
		}
	})

	t.Run("negative default is rejected", func(t *testing.T) {
		path := writeConfig(t, "defaults:\n  timeout: -1\n")
		if _, err := LoadConfig(path); !errors.Is(err, ErrInvalidTimeout) {
			t.Errorf("LoadConfig() error = %v, want ErrInvalidTimeout", err)
		}
	})
}

func TestTimeoutInheritance(t *testing.T) {
	load := func(t *testing.T, yaml string) *Config {
		t.Helper()

		cfg, err := LoadConfig(writeConfig(t, yaml))
		if err != nil {
			t.Fatalf("LoadConfig() unexpected error: %v", err)
		}

		return cfg
	}

	t.Run("unset everywhere stays unset and runs as 900", func(t *testing.T) {
		m := &Manifest{}
		load(t, "port: 8000\n").ApplyDefaults(m)

		if m.Timeout != nil {
			t.Errorf("Timeout = %d, want unset", *m.Timeout)
		}
		if got := m.TimeoutSeconds(); got != 900 {
			t.Errorf("TimeoutSeconds() = %d, want 900", got)
		}
	})

	t.Run("an unset function inherits the default, including 0", func(t *testing.T) {
		m := &Manifest{}
		load(t, "defaults:\n  timeout: 0\n").ApplyDefaults(m)

		if m.Timeout == nil || *m.Timeout != 0 {
			t.Errorf("Timeout = %v, want explicit 0 inherited", m.Timeout)
		}
	})

	t.Run("an explicit function 0 beats a non-zero default", func(t *testing.T) {
		m := &Manifest{Timeout: new(0)}
		load(t, "defaults:\n  timeout: 15\n").ApplyDefaults(m)

		if m.Timeout == nil || *m.Timeout != 0 {
			t.Errorf("Timeout = %v, want the function's own 0", m.Timeout)
		}
	})

	t.Run("an explicit override 0 beats the function manifest", func(t *testing.T) {
		m := &Manifest{Timeout: new(30)}
		m.ApplyOverrides(Manifest{Timeout: new(0)})

		if m.Timeout == nil || *m.Timeout != 0 {
			t.Errorf("Timeout = %v, want the override's 0", m.Timeout)
		}
	})

	t.Run("an unset override leaves the function manifest alone", func(t *testing.T) {
		m := &Manifest{Timeout: new(30)}
		m.ApplyOverrides(Manifest{})

		if m.Timeout == nil || *m.Timeout != 30 {
			t.Errorf("Timeout = %v, want 30", m.Timeout)
		}
	})
}

func TestRuntimeTimeoutSeconds(t *testing.T) {
	tests := []struct {
		name string
		m    *Manifest
		want int
	}{
		{"nil manifest", nil, 900},
		{"unset", &Manifest{}, 900},
		{"zero maps to no limit", &Manifest{Timeout: new(0)}, NoLimitRuntimeTimeout},
		{"value", &Manifest{Timeout: new(42)}, 42},
		{"above the AWS maximum", &Manifest{Timeout: new(1800)}, 1800},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.RuntimeTimeoutSeconds(); got != tc.want {
				t.Errorf("RuntimeTimeoutSeconds() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestTimeoutWarning(t *testing.T) {
	tests := []struct {
		name string
		m    *Manifest
		want string
	}{
		{"nil manifest", nil, ""},
		{"unset", &Manifest{}, ""},
		{"one second", &Manifest{Timeout: new(1)}, ""},
		{"the AWS maximum", &Manifest{Timeout: new(900)}, ""},
		{
			"zero",
			&Manifest{Timeout: new(0)},
			"function api: timeout 0 (no limit) is outside AWS Lambda's 1-900 second range; set it to 900 or less before deploying",
		},
		{
			"above the AWS maximum",
			&Manifest{Timeout: new(1800)},
			"function api: timeout 1800s is outside AWS Lambda's 1-900 second range; set it to 900 or less before deploying",
		},
		{
			"one above the AWS maximum",
			&Manifest{Timeout: new(901)},
			"function api: timeout 901s is outside AWS Lambda's 1-900 second range; set it to 900 or less before deploying",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.m.TimeoutWarning("api")
			if got != tc.want {
				t.Errorf("TimeoutWarning() = %q, want %q", got, tc.want)
			}

			if got != "" && !strings.Contains(got, "1-900") {
				t.Errorf("TimeoutWarning() = %q, want it to name the 1-900 range", got)
			}
		})
	}
}
