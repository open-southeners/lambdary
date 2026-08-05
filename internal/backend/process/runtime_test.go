package process

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

func TestRuntimeCommand(t *testing.T) {
	t.Run("nodejs* runs the node shim with the handler as argv", func(t *testing.T) {
		fn := discovery.Function{Name: "hello", Runtime: "nodejs22.x", Handler: "index.handler", Manifest: &manifest.Manifest{}}

		name, args, err := runtimeCommand(fn, "/abs/hello", "/shims/abc123")
		if err != nil {
			t.Fatalf("runtimeCommand() unexpected error: %v", err)
		}

		wantName, wantArgs := "node", []string{"/shims/abc123/bootstrap.mjs", "index.handler"}
		if name != wantName || !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("runtimeCommand() = (%q, %v), want (%q, %v)", name, args, wantName, wantArgs)
		}
	})

	t.Run("python* runs the python shim with the handler as argv", func(t *testing.T) {
		fn := discovery.Function{Name: "hello", Runtime: "python3.13", Handler: "app.handler", Manifest: &manifest.Manifest{}}

		name, args, err := runtimeCommand(fn, "/abs/hello", "/shims/abc123")
		if err != nil {
			t.Fatalf("runtimeCommand() unexpected error: %v", err)
		}

		wantName, wantArgs := "python3", []string{"/shims/abc123/bootstrap.py", "app.handler"}
		if name != wantName || !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("runtimeCommand() = (%q, %v), want (%q, %v)", name, args, wantName, wantArgs)
		}
	})

	t.Run("ruby* runs the ruby shim with the handler as argv", func(t *testing.T) {
		fn := discovery.Function{Name: "hello", Runtime: "ruby3.3", Handler: "function.handler", Manifest: &manifest.Manifest{}}

		name, args, err := runtimeCommand(fn, "/abs/hello", "/shims/abc123")
		if err != nil {
			t.Fatalf("runtimeCommand() unexpected error: %v", err)
		}

		wantName, wantArgs := "ruby", []string{"/shims/abc123/bootstrap.rb", "function.handler"}
		if name != wantName || !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("runtimeCommand() = (%q, %v), want (%q, %v)", name, args, wantName, wantArgs)
		}
	})

	t.Run("provided.* runs the function's own executable bootstrap", func(t *testing.T) {
		dir := t.TempDir()
		writeExecutable(t, filepath.Join(dir, "bootstrap"), "#!/bin/sh\necho hi\n")

		fn := discovery.Function{Name: "custom", Runtime: "provided.al2023", Manifest: &manifest.Manifest{}}

		name, args, err := runtimeCommand(fn, dir, "/shims/abc123")
		if err != nil {
			t.Fatalf("runtimeCommand() unexpected error: %v", err)
		}

		if name != "./bootstrap" || len(args) != 0 {
			t.Errorf("runtimeCommand() = (%q, %v), want (\"./bootstrap\", [])", name, args)
		}
	})

	t.Run("provided.* with no bootstrap file errors", func(t *testing.T) {
		fn := discovery.Function{Name: "custom", Runtime: "provided.al2023", Manifest: &manifest.Manifest{}}

		_, _, err := runtimeCommand(fn, t.TempDir(), "/shims/abc123")
		if !errors.Is(err, ErrBootstrapMissing) {
			t.Errorf("runtimeCommand() error = %v, want wrapping ErrBootstrapMissing", err)
		}
	})

	t.Run("provided.* with a non-executable bootstrap file errors", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "bootstrap"), []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
			t.Fatalf("writing bootstrap: %v", err)
		}

		fn := discovery.Function{Name: "custom", Runtime: "provided.al2023", Manifest: &manifest.Manifest{}}

		_, _, err := runtimeCommand(fn, dir, "/shims/abc123")
		if !errors.Is(err, ErrBootstrapMissing) {
			t.Errorf("runtimeCommand() error = %v, want wrapping ErrBootstrapMissing", err)
		}
	})

	t.Run("unsupported runtime family errors naming the runtime", func(t *testing.T) {
		fn := discovery.Function{Name: "hello", Runtime: "java21", Manifest: &manifest.Manifest{}}

		_, _, err := runtimeCommand(fn, t.TempDir(), "/shims/abc123")
		if !errors.Is(err, ErrRuntimeNotSupported) {
			t.Errorf("runtimeCommand() error = %v, want wrapping ErrRuntimeNotSupported", err)
		}
		if err != nil && !strings.Contains(err.Error(), "java21") {
			t.Errorf("runtimeCommand() error = %v, want it to name the runtime", err)
		}
	})

	t.Run("local.command override runs via sh -c, taking priority over the runtime family", func(t *testing.T) {
		fn := discovery.Function{
			Name:    "hello",
			Runtime: "nodejs22.x",
			Manifest: &manifest.Manifest{
				Local: manifest.Local{Command: "./my-custom-runtime --flag"},
			},
		}

		name, args, err := runtimeCommand(fn, "/abs/hello", "/shims/abc123")
		if err != nil {
			t.Fatalf("runtimeCommand() unexpected error: %v", err)
		}

		wantArgs := []string{"-c", "./my-custom-runtime --flag"}
		if name != "sh" || !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("runtimeCommand() = (%q, %v), want (\"sh\", %v)", name, args, wantArgs)
		}
	})

	t.Run("nil manifest behaves like an empty one", func(t *testing.T) {
		fn := discovery.Function{Name: "hello", Runtime: "nodejs22.x", Handler: "index.handler"}

		name, args, err := runtimeCommand(fn, "/abs/hello", "/shims/abc123")
		if err != nil {
			t.Fatalf("runtimeCommand() unexpected error: %v", err)
		}
		if name != "node" || !reflect.DeepEqual(args, []string{"/shims/abc123/bootstrap.mjs", "index.handler"}) {
			t.Errorf("runtimeCommand() = (%q, %v), unexpected result for a nil manifest", name, args)
		}
	})
}

func TestSupports(t *testing.T) {
	tests := []struct {
		name string
		fn   discovery.Function
		want bool
	}{
		{"nodejs* is supported", discovery.Function{Runtime: "nodejs22.x"}, true},
		{"python* is supported", discovery.Function{Runtime: "python3.13"}, true},
		{"ruby* is supported", discovery.Function{Runtime: "ruby3.3"}, true},
		{"provided.* is supported even without a bootstrap file", discovery.Function{Runtime: "provided.al2023"}, true},
		{"java is not supported", discovery.Function{Runtime: "java21"}, false},
		{"dotnet is not supported", discovery.Function{Runtime: "dotnet8"}, false},
		{"empty runtime with no local.command is not supported", discovery.Function{}, false},
		{
			"local.command wins regardless of runtime",
			discovery.Function{Runtime: "java21", Manifest: &manifest.Manifest{Local: manifest.Local{Command: "./my-custom-runtime"}}},
			true,
		},
		{
			"local.command with no runtime set at all is still supported",
			discovery.Function{Manifest: &manifest.Manifest{Local: manifest.Local{Command: "./my-custom-runtime"}}},
			true,
		},
		{
			"nil manifest with an unsupported runtime behaves like an empty one",
			discovery.Function{Runtime: "java21", Manifest: nil},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Supports(tt.fn); got != tt.want {
				t.Errorf("Supports(%+v) = %v, want %v", tt.fn, got, tt.want)
			}
		})
	}
}

func TestRieArgs(t *testing.T) {
	got := rieArgs(54321, 54322, "node", []string{"/shims/x/bootstrap.mjs", "index.handler"})

	want := []string{
		"--runtime-interface-emulator-address", "127.0.0.1:54321",
		"--runtime-api-address", "127.0.0.1:54322",
		"node", "/shims/x/bootstrap.mjs", "index.handler",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("rieArgs() =\n%v\nwant\n%v", got, want)
	}

	t.Run("both address flags are present and distinct, neither is 9001", func(t *testing.T) {
		invokeIdx, rapiIdx := -1, -1
		for i, a := range got {
			if a == "--runtime-interface-emulator-address" {
				invokeIdx = i
			}
			if a == "--runtime-api-address" {
				rapiIdx = i
			}
		}

		if invokeIdx == -1 || rapiIdx == -1 {
			t.Fatalf("rieArgs() = %v, want both address flags present", got)
		}
		if got[invokeIdx+1] == got[rapiIdx+1] {
			t.Errorf("rieArgs() gave the same address to both flags: %v", got)
		}
		if got[invokeIdx+1] == "127.0.0.1:9001" || got[rapiIdx+1] == "127.0.0.1:9001" {
			t.Errorf("rieArgs() used the reserved port 9001: %v", got)
		}
	})

	t.Run("bootstrap-style command with no extra args", func(t *testing.T) {
		got := rieArgs(1, 2, "./bootstrap", nil)
		want := []string{
			"--runtime-interface-emulator-address", "127.0.0.1:1",
			"--runtime-api-address", "127.0.0.1:2",
			"./bootstrap",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rieArgs() =\n%v\nwant\n%v", got, want)
		}
	})
}

func TestManifestEnv(t *testing.T) {
	t.Run("full manifest: environment, timeout, memory", func(t *testing.T) {
		fn := discovery.Function{
			Name: "hello",
			Manifest: &manifest.Manifest{
				Timeout: 15,
				Memory:  256,
				Environment: map[string]string{
					"TABLE_NAME": "local-table",
					"API_KEY":    "secret",
				},
			},
		}

		got := manifestEnv(fn, nil)
		want := []string{
			"API_KEY=secret",
			"TABLE_NAME=local-table",
			"AWS_LAMBDA_FUNCTION_NAME=hello",
			"AWS_LAMBDA_FUNCTION_TIMEOUT=15",
			"AWS_LAMBDA_FUNCTION_MEMORY_SIZE=256",
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("manifestEnv() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("minimal manifest: only the function name", func(t *testing.T) {
		fn := discovery.Function{Name: "minimal", Manifest: &manifest.Manifest{}}

		got := manifestEnv(fn, nil)
		want := []string{"AWS_LAMBDA_FUNCTION_NAME=minimal"}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("manifestEnv() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("nil manifest behaves like an empty one", func(t *testing.T) {
		fn := discovery.Function{Name: "no-manifest"}

		got := manifestEnv(fn, nil)
		want := []string{"AWS_LAMBDA_FUNCTION_NAME=no-manifest"}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("manifestEnv() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("fileEnv is folded in under manifest environment", func(t *testing.T) {
		fn := discovery.Function{
			Name: "hello",
			Manifest: &manifest.Manifest{
				Environment: map[string]string{"API_KEY": "secret"},
			},
		}

		fileEnv := map[string]string{"API_KEY": "from-file", "EXTRA": "from-file-only"}

		got := manifestEnv(fn, fileEnv)
		want := []string{
			"API_KEY=secret",
			"EXTRA=from-file-only",
			"AWS_LAMBDA_FUNCTION_NAME=hello",
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("manifestEnv() =\n%v\nwant\n%v (explicit environment must win over fileEnv on conflict)", got, want)
		}
	})
}

func TestEnvMerge(t *testing.T) {
	t.Run("manifest environment wins over fileEnv on key conflict", func(t *testing.T) {
		got := envMerge(
			map[string]string{"A": "file", "B": "file-only"},
			map[string]string{"A": "manifest"},
		)
		want := map[string]string{"A": "manifest", "B": "file-only"}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("envMerge() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("empty fileEnv returns the manifest environment unchanged", func(t *testing.T) {
		manifestEnv := map[string]string{"A": "manifest"}

		got := envMerge(nil, manifestEnv)
		if !reflect.DeepEqual(got, manifestEnv) {
			t.Errorf("envMerge() =\n%v\nwant\n%v", got, manifestEnv)
		}
	})
}

// writeExecutable writes content to path with the executable bit set,
// failing the test on any error.
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
