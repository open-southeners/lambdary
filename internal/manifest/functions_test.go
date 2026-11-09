package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeConfig writes content to a lambdary.yml in a fresh temp dir and
// returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "lambdary.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}

	return path
}

func TestLoadConfigFunctions(t *testing.T) {
	t.Run("functions and build parse", func(t *testing.T) {
		path := writeConfig(t, `
functions:
  ping:
    go: ./cmd/ping
  notify:
    go: ./cmd/notify
    timeout: 30
    url: { path: /hooks/notify }
  legacy:
    path: ./legacy-node
    runtime: nodejs22.x
build:
  go:
    tags: [netgo, osusergo]
    ldflags: "-X main.version=dev"
`)

		c, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() unexpected error: %v", err)
		}

		want := map[string]FunctionEntry{
			"ping":   {Go: "./cmd/ping"},
			"notify": {Go: "./cmd/notify", Manifest: Manifest{Timeout: 30, URL: URL{Path: "/hooks/notify"}}},
			"legacy": {Path: "./legacy-node", Manifest: Manifest{Runtime: "nodejs22.x"}},
		}
		if !reflect.DeepEqual(c.Functions, want) {
			t.Errorf("Functions = %#v, want %#v", c.Functions, want)
		}

		wantBuild := Build{Go: GoBuild{Tags: []string{"netgo", "osusergo"}, LDFlags: "-X main.version=dev"}}
		if !reflect.DeepEqual(c.Build, wantBuild) {
			t.Errorf("Build = %#v, want %#v", c.Build, wantBuild)
		}
	})

	t.Run("no functions block leaves Functions empty", func(t *testing.T) {
		c, err := LoadConfig(writeConfig(t, "port: 9000\n"))
		if err != nil {
			t.Fatalf("LoadConfig() unexpected error: %v", err)
		}
		if len(c.Functions) != 0 {
			t.Errorf("Functions = %#v, want empty", c.Functions)
		}
	})

	errCases := []struct {
		name    string
		content string
		want    error
	}{
		{
			"neither path nor go",
			"functions:\n  a:\n    timeout: 5\n",
			ErrFunctionSource,
		},
		{
			"both path and go",
			"functions:\n  a:\n    path: ./a\n    go: ./cmd/a\n",
			ErrFunctionSource,
		},
		{
			"empty entry",
			"functions:\n  a: {}\n",
			ErrFunctionSource,
		},
		{
			"inline name rejected",
			"functions:\n  a:\n    go: ./cmd/a\n    name: other\n",
			ErrFunctionName,
		},
		{
			"key with slash",
			"functions:\n  \"a/b\":\n    go: ./cmd/a\n",
			ErrFunctionName,
		},
		{
			"key with whitespace",
			"functions:\n  \"a b\":\n    go: ./cmd/a\n",
			ErrFunctionName,
		},
		{
			"empty key",
			"functions:\n  \"\":\n    go: ./cmd/a\n",
			ErrFunctionName,
		},
		{
			"negative inline timeout",
			"functions:\n  a:\n    go: ./cmd/a\n    timeout: -1\n",
			ErrInvalidTimeout,
		},
	}

	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.content))
			if err == nil {
				t.Fatal("LoadConfig() expected error, got nil")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("LoadConfig() error = %v, want wrapping %v", err, tc.want)
			}
		})
	}

	t.Run("unknown key inside an entry is rejected", func(t *testing.T) {
		_, err := LoadConfig(writeConfig(t, "functions:\n  a:\n    go: ./cmd/a\n    bogus: 1\n"))
		if err == nil {
			t.Fatal("LoadConfig() expected error, got nil")
		}
	})
}

func TestApplyOverrides(t *testing.T) {
	base := func() Manifest {
		return Manifest{
			Name:          "base",
			Runtime:       "nodejs22.x",
			Handler:       "index.handler",
			Timeout:       10,
			Memory:        128,
			Architectures: []string{"x86_64"},
			Layers:        []string{"../layer"},
			Environment:   map[string]string{"A": "1"},
			URL:           URL{Path: "/base", Payload: "1.0", InvokeMode: "BUFFERED"},
			Local:         Local{Backend: "process", Image: "img", Command: "cmd", EnvFile: ".env"},
		}
	}

	t.Run("empty override changes nothing", func(t *testing.T) {
		m := base()
		m.ApplyOverrides(Manifest{})

		if want := base(); !reflect.DeepEqual(m, want) {
			t.Errorf("m = %#v, want %#v", m, want)
		}
	})

	t.Run("every set field replaces", func(t *testing.T) {
		m := base()
		o := Manifest{
			Name:          "o",
			Runtime:       "python3.13",
			Handler:       "app.handler",
			Timeout:       99,
			Memory:        512,
			Architectures: []string{"arm64"},
			Layers:        []string{"../other"},
			Environment:   map[string]string{"B": "2"},
			URL:           URL{Path: "/o", Payload: "2.0", InvokeMode: "RESPONSE_STREAM"},
			Local:         Local{Backend: "container", Image: "oimg", Command: "ocmd", EnvFile: ".env.o"},
		}
		m.ApplyOverrides(o)

		if !reflect.DeepEqual(m, o) {
			t.Errorf("m = %#v, want %#v", m, o)
		}
	})

	t.Run("nested structs merge field by field", func(t *testing.T) {
		m := base()
		m.ApplyOverrides(Manifest{
			URL:   URL{Path: "/new"},
			Local: Local{Image: "newimg"},
		})

		if m.URL.Path != "/new" || m.URL.Payload != "1.0" || m.URL.InvokeMode != "BUFFERED" {
			t.Errorf("URL = %#v, want path replaced, payload and invoke mode kept", m.URL)
		}
		if m.Local.Image != "newimg" || m.Local.Backend != "process" || m.Local.Command != "cmd" || m.Local.EnvFile != ".env" {
			t.Errorf("Local = %#v, want image replaced, rest kept", m.Local)
		}
	})

	t.Run("single scalar override keeps the rest", func(t *testing.T) {
		m := base()
		m.ApplyOverrides(Manifest{Timeout: 5})

		want := base()
		want.Timeout = 5
		if !reflect.DeepEqual(m, want) {
			t.Errorf("m = %#v, want %#v", m, want)
		}
	})
}
