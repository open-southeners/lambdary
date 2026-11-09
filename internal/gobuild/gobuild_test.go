package gobuild

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
)

// recordingRunner is a backend.Runner test double that records every Run
// call and returns a scripted result.
type recordingRunner struct {
	mu    sync.Mutex
	names []string
	args  [][]string

	out []byte
	err error
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.names = append(r.names, name)
	r.args = append(r.args, append([]string(nil), args...))

	return r.out, r.err
}

func (r *recordingRunner) Start(_ context.Context, name string, _ ...string) (backend.Cmd, error) {
	return nil, fmt.Errorf("unexpected Start(%s)", name)
}

func TestBuildArgv(t *testing.T) {
	cases := []struct {
		name string
		src  discovery.GoSource
		want []string
	}{
		{
			name: "no tags, no ldflags",
			src:  discovery.GoSource{ModuleDir: "/mod", Package: "./cmd/ping"},
			want: []string{
				"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "go", "-C", "/mod", "build",
				"-tags", "lambda.norpc", "-o", "OUT", "./cmd/ping",
			},
		},
		{
			name: "tags appended after lambda.norpc",
			src:  discovery.GoSource{ModuleDir: "/mod", Package: ".", Tags: []string{"netgo", "osusergo"}},
			want: []string{
				"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "go", "-C", "/mod", "build",
				"-tags", "lambda.norpc,netgo,osusergo", "-o", "OUT", ".",
			},
		},
		{
			name: "ldflags included only when set",
			src:  discovery.GoSource{ModuleDir: "/mod", Package: ".", LDFlags: "-X main.version=dev"},
			want: []string{
				"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "go", "-C", "/mod", "build",
				"-tags", "lambda.norpc", "-ldflags", "-X main.version=dev", "-o", "OUT", ".",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "nested", "linux_arm64", "bootstrap")
			r := &recordingRunner{}

			if err := Build(context.Background(), r, tc.src, "linux", "arm64", out); err != nil {
				t.Fatalf("Build() unexpected error: %v", err)
			}

			if len(r.args) != 1 {
				t.Fatalf("Run calls = %d, want 1", len(r.args))
			}
			if r.names[0] != "env" {
				t.Errorf("command = %q, want env", r.names[0])
			}

			want := append([]string(nil), tc.want...)
			for i, a := range want {
				if a == "OUT" {
					want[i] = out
				}
			}
			if !reflect.DeepEqual(r.args[0], want) {
				t.Errorf("argv = %v, want %v", r.args[0], want)
			}

			if info, err := os.Stat(filepath.Dir(out)); err != nil || !info.IsDir() {
				t.Errorf("output parent dir not created: %v", err)
			}
		})
	}
}

// TestBuildRelativeOutput covers a cache dir under a relative --root: `go -C`
// would otherwise resolve -o against the module and write somewhere the caller
// never looks.
func TestBuildRelativeOutput(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	runner := &recordingRunner{}

	src := discovery.GoSource{ModuleDir: filepath.Join(tmp, "functions"), Package: "./cmd/ping"}
	if err := Build(context.Background(), runner, src, "linux", "amd64", "functions/.lambdary/bootstrap"); err != nil {
		t.Fatalf("Build() unexpected error: %v", err)
	}

	args := runner.args[0]

	for i, arg := range args {
		if arg != "-o" {
			continue
		}

		// t.Chdir resolves symlinks in the temp path on some platforms.
		if got, want := args[i+1], "functions/.lambdary/bootstrap"; !filepath.IsAbs(got) || !strings.HasSuffix(got, want) {
			t.Errorf("-o %s, want an absolute path ending in %s", got, want)
		}

		return
	}

	t.Fatalf("no -o in %v", args)
}

func TestBuildFailure(t *testing.T) {
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf("line%02d", i))
		if i%10 == 0 {
			lines = append(lines, "", "   ")
		}
	}

	r := &recordingRunner{out: []byte(strings.Join(lines, "\n") + "\n"), err: errors.New("exit status 1")}
	src := discovery.GoSource{ModuleDir: "/mod", Package: "./cmd/broken"}

	err := Build(context.Background(), r, src, "linux", "amd64", filepath.Join(t.TempDir(), "bootstrap"))
	if err == nil {
		t.Fatal("Build() expected error, got nil")
	}

	msg := err.Error()
	if !strings.Contains(msg, "./cmd/broken") {
		t.Errorf("error = %q, want it to mention the package", msg)
	}
	if !strings.Contains(msg, "build output:") {
		t.Errorf("error = %q, want a \"build output:\" line", msg)
	}
	if !strings.Contains(msg, "line30") || !strings.Contains(msg, "line11") {
		t.Errorf("error = %q, want the last 20 non-blank lines (line11..line30)", msg)
	}
	if strings.Contains(msg, "line10\n") || strings.Contains(msg, "line01") {
		t.Errorf("error = %q, want earlier lines trimmed", msg)
	}
}

func TestOutputPath(t *testing.T) {
	got := OutputPath("/proj/.lambdary", "ping", "linux", "arm64")
	want := filepath.Join("/proj/.lambdary", "build", "ping", "linux_arm64", "bootstrap")
	if got != want {
		t.Errorf("OutputPath() = %q, want %q", got, want)
	}
}

func TestArchFor(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"arm64", []string{"arm64"}, "arm64"},
		{"x86_64", []string{"x86_64"}, "amd64"},
		{"first entry wins", []string{"arm64", "x86_64"}, "arm64"},
		{"empty falls back to host", nil, runtime.GOARCH},
		{"unknown falls back to host", []string{"riscv"}, runtime.GOARCH},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArchFor(tc.in); got != tc.want {
				t.Errorf("ArchFor(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildRealToolchain(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real go build under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}

	mod := t.TempDir()
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module example.com/tiny\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mod, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("writing main.go: %v", err)
	}

	out := OutputPath(t.TempDir(), "tiny", runtime.GOOS, runtime.GOARCH)
	src := discovery.GoSource{ModuleDir: mod, Package: "."}

	if err := Build(context.Background(), backend.ExecRunner{}, src, runtime.GOOS, runtime.GOARCH, out); err != nil {
		t.Fatalf("Build() unexpected error: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("Stat(%s): %v", out, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("output mode = %v, want executable", info.Mode())
	}
}
