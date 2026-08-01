package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/discovery"
)

// TestNewInitCmd verifies the init command is properly configured.
func TestNewInitCmd(t *testing.T) {
	cmd := newInitCmd()
	if cmd.Use != "init <name>" {
		t.Errorf("expected Use 'init <name>', got %q", cmd.Use)
	}
	if cmd.RunE == nil {
		t.Errorf("expected RunE to be set")
	}
	if got := cmd.Flags().Lookup("runtime").DefValue; got != defaultInitRuntime {
		t.Errorf("--runtime default = %q, want %q", got, defaultInitRuntime)
	}
}

// TestRunInitScaffolds covers every supported --runtime: the expected files
// exist with the expected key content (and, for provided.al2023, the
// bootstrap's exec bit), and the scaffolded function is actually found by
// discovery.Discover with the right Name/Runtime/Route — proving the
// scaffold is discoverable, not just written to disk.
func TestRunInitScaffolds(t *testing.T) {
	tests := []struct {
		name        string
		runtimeVal  string
		wantRuntime string
		wantFiles   map[string]string // file -> substring the content must contain
	}{
		{
			name:        "nodejs22.x",
			runtimeVal:  "nodejs22.x",
			wantRuntime: "nodejs22.x",
			wantFiles: map[string]string{
				"handler.mjs": `export const handler = async (event) => ({ message: "Hello from nodejs22.x-fn!" });`,
				".lambda.yml": "runtime: nodejs22.x\nhandler: handler.handler\ntimeout: 30\n",
			},
		},
		{
			name:        "python3.13",
			runtimeVal:  "python3.13",
			wantRuntime: "python3.13",
			wantFiles: map[string]string{
				"lambda_function.py": "def handler(event, context):\n    return {\"message\": \"Hello from python3.13-fn!\"}\n",
				".lambda.yml":        "runtime: python3.13\nhandler: lambda_function.handler\ntimeout: 30\n",
			},
		},
		{
			name:        "provided.al2023",
			runtimeVal:  "provided.al2023",
			wantRuntime: "provided.al2023",
			wantFiles: map[string]string{
				"bootstrap":   "AWS_LAMBDA_RUNTIME_API",
				".lambda.yml": "runtime: provided.al2023\n",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			fnName := tt.runtimeVal + "-fn"

			var out bytes.Buffer
			if err := runInit(&out, root, fnName, tt.runtimeVal); err != nil {
				t.Fatalf("runInit(%q) error = %v", tt.runtimeVal, err)
			}

			dir := filepath.Join(root, fnName)

			for file, wantSubstr := range tt.wantFiles {
				path := filepath.Join(dir, file)

				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading %s: %v", path, err)
				}
				if !strings.Contains(string(content), wantSubstr) {
					t.Errorf("%s content = %q, want it to contain %q", path, content, wantSubstr)
				}
			}

			if tt.runtimeVal == "provided.al2023" {
				info, err := os.Stat(filepath.Join(dir, "bootstrap"))
				if err != nil {
					t.Fatalf("stat bootstrap: %v", err)
				}
				if info.Mode()&0o111 == 0 {
					t.Errorf("bootstrap mode = %v, want executable bit set", info.Mode())
				}
			}

			out.Reset()

			fns, err := discovery.Discover(root, nil)
			if err != nil {
				t.Fatalf("discovery.Discover(%q) error = %v", root, err)
			}

			var found *discovery.Function
			for i := range fns {
				if fns[i].Name == fnName {
					found = &fns[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("discovery.Discover did not find scaffolded function %q, got %+v", fnName, fns)
			}
			if found.Runtime != tt.wantRuntime {
				t.Errorf("discovered Runtime = %q, want %q", found.Runtime, tt.wantRuntime)
			}
			if want := "/" + fnName; found.Route != want {
				t.Errorf("discovered Route = %q, want %q", found.Route, want)
			}
		})
	}
}

// TestRunInitDefaultRuntime checks that an empty/omitted --runtime value
// falls through the command's own flag default (nodejs22.x) rather than
// runInit's own default, since runInit always takes an explicit runtime
// value from the flag.
func TestRunInitDefaultRuntime(t *testing.T) {
	root := t.TempDir()

	var out bytes.Buffer
	if err := runInit(&out, root, "default-fn", defaultInitRuntime); err != nil {
		t.Fatalf("runInit error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "default-fn", "handler.mjs")); err != nil {
		t.Errorf("expected handler.mjs for default runtime: %v", err)
	}
}

// TestRunInitExistingDirRefused checks that init refuses to touch a
// directory that already exists: the error names the path and the
// directory is left exactly as it was (no scaffold files added).
func TestRunInitExistingDirRefused(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "already-here")

	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	marker := filepath.Join(dir, "keep-me.txt")
	if err := os.WriteFile(marker, []byte("untouched"), 0o644); err != nil {
		t.Fatalf("writing marker file: %v", err)
	}

	var out bytes.Buffer
	err := runInit(&out, root, "already-here", defaultInitRuntime)
	if err == nil {
		t.Fatal("runInit on an existing dir: want error, got nil")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error = %q, want it to mention %q", err.Error(), dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir after refusal: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "keep-me.txt" {
		t.Errorf("dir contents after refusal = %v, want only the pre-existing keep-me.txt", entries)
	}

	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "untouched" {
		t.Errorf("marker file changed: content=%q err=%v", content, err)
	}
}

// TestValidateInitRuntimeUnknown checks that an unsupported --runtime value
// errors listing every supported id.
func TestValidateInitRuntimeUnknown(t *testing.T) {
	err := validateInitRuntime("dotnet8")
	if err == nil {
		t.Fatal("validateInitRuntime(\"dotnet8\"): want error, got nil")
	}

	for _, r := range supportedInitRuntimes {
		if !strings.Contains(err.Error(), r) {
			t.Errorf("error %q missing supported runtime %q", err.Error(), r)
		}
	}
}

// TestRunInitUnknownRuntime checks the same validation surfaces from
// runInit itself, before anything is written to disk.
func TestRunInitUnknownRuntime(t *testing.T) {
	root := t.TempDir()

	var out bytes.Buffer
	err := runInit(&out, root, "some-fn", "dotnet8")
	if err == nil {
		t.Fatal("runInit with unknown runtime: want error, got nil")
	}

	if _, statErr := os.Stat(filepath.Join(root, "some-fn")); !os.IsNotExist(statErr) {
		t.Errorf("expected no directory created for a rejected runtime, stat err = %v", statErr)
	}
}

// TestPrintInitNextSteps checks the printed next-steps output names the
// created path, a "lambdary dev" hint, and the function's local URL.
func TestPrintInitNextSteps(t *testing.T) {
	var out bytes.Buffer
	printInitNextSteps(&out, "/root/my-fn", "my-fn", 8000)

	got := out.String()
	for _, want := range []string{"/root/my-fn", "lambdary dev", "http://127.0.0.1:8000/my-fn"} {
		if !strings.Contains(got, want) {
			t.Errorf("next-steps output = %q, want it to contain %q", got, want)
		}
	}
}
