package container

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/gobuild"
	"github.com/open-southeners/lambdary/internal/manifest"
)

func TestPlatformOf(t *testing.T) {
	tests := []struct {
		name string
		fn   discovery.Function
		want string
		arch string
	}{
		{"nil manifest defaults to x86_64", discovery.Function{}, "linux/amd64", "amd64"},
		{"empty architectures default to x86_64", discovery.Function{Manifest: &manifest.Manifest{}}, "linux/amd64", "amd64"},
		{"x86_64", discovery.Function{Manifest: &manifest.Manifest{Architectures: []string{"x86_64"}}}, "linux/amd64", "amd64"},
		{"arm64", discovery.Function{Manifest: &manifest.Manifest{Architectures: []string{"arm64"}}}, "linux/arm64", "arm64"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := platformOf(tc.fn); got != tc.want {
				t.Errorf("platformOf() = %q, want %q", got, tc.want)
			}

			// The Go bootstrap arch must derive from the same setting.
			if got := goArch(tc.fn); got != tc.arch {
				t.Errorf("goArch() = %q, want %q", got, tc.arch)
			}

			if got := gobuild.ArchFor([]string{archOf(tc.fn)}); got != tc.arch {
				t.Errorf("gobuild.ArchFor(archOf()) = %q, want %q", got, tc.arch)
			}
		})
	}
}

func TestRunArgsAlwaysPinsPlatform(t *testing.T) {
	for arch, want := range map[string]string{"": "linux/amd64", "x86_64": "linux/amd64", "arm64": "linux/arm64"} {
		fn := discovery.Function{Name: "f", Runtime: "nodejs22.x", Handler: "h", Manifest: &manifest.Manifest{}}
		if arch != "" {
			fn.Manifest.Architectures = []string{arch}
		}

		args := runArgs(fn, "image", "/abs/f", nil, "", "")

		idx := indexOf(args, "--platform")
		if idx == -1 || args[idx+1] != want {
			t.Errorf("arch %q: argv = %v, want --platform %s", arch, args, want)
		}

		if idx > indexOf(args, "image") {
			t.Errorf("arch %q: --platform must come before the image: %v", arch, args)
		}
	}
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}

	return -1
}

func TestPlatformMatches(t *testing.T) {
	tests := []struct {
		have, want string
		ok         bool
	}{
		{"linux/amd64", "linux/amd64", true},
		{"linux/arm64", "linux/amd64", false},
		{"linux/arm64/v8", "linux/arm64", true},
		{"linux/arm", "linux/arm64", false},
		{"windows/amd64", "linux/amd64", false},
	}

	for _, tc := range tests {
		if got := platformMatches(tc.have, tc.want); got != tc.ok {
			t.Errorf("platformMatches(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}
}

func TestParseImagePlatform(t *testing.T) {
	if got, err := parseImagePlatform("linux/arm64\n"); err != nil || got != "linux/arm64" {
		t.Errorf("parseImagePlatform() = %q, %v", got, err)
	}

	for _, bad := range []string{"", "\n", "/", "linux/", "arm64"} {
		if _, err := parseImagePlatform(bad); err == nil {
			t.Errorf("parseImagePlatform(%q) expected an error", bad)
		}
	}
}

func TestEnsureImage(t *testing.T) {
	const ref = "public.ecr.aws/lambda/provided:al2023"

	newBackend := func(cached string, pullErr error) (*containerBackend, *fakeRunner) {
		pulled := false

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"image": func(args []string) (string, error) {
				if cached == "" && !pulled {
					return "Error: No such image", errors.New("exit status 1")
				}

				if pulled {
					return "linux/amd64\n", nil
				}

				return cached + "\n", nil
			},
			"pull": func(args []string) (string, error) {
				pulled = pullErr == nil
				return "", pullErr
			},
		}}

		return &containerBackend{cli: "docker", runner: r}, r
	}

	t.Run("matching cached image is not pulled", func(t *testing.T) {
		b, r := newBackend("linux/amd64", nil)

		if err := b.ensureImage(context.Background(), ref, "linux/amd64"); err != nil {
			t.Fatalf("ensureImage() unexpected error: %v", err)
		}

		if calls := r.callsFor("pull"); len(calls) != 0 {
			t.Errorf("pull calls = %v, want none", calls)
		}
	})

	t.Run("cached image of the wrong platform is re-pulled with --platform", func(t *testing.T) {
		b, r := newBackend("linux/arm64", nil)

		if err := b.ensureImage(context.Background(), ref, "linux/amd64"); err != nil {
			t.Fatalf("ensureImage() unexpected error: %v", err)
		}

		calls := r.callsFor("pull")
		if len(calls) != 1 || strings.Join(calls[0], " ") != "pull --platform linux/amd64 "+ref {
			t.Errorf("pull calls = %v, want one `pull --platform linux/amd64 %s`", calls, ref)
		}
	})

	t.Run("missing image is pulled with --platform", func(t *testing.T) {
		b, r := newBackend("", nil)

		if err := b.ensureImage(context.Background(), ref, "linux/arm64"); err == nil {
			// The fake reports amd64 after the pull, i.e. a definite mismatch.
			t.Fatal("ensureImage() expected an error when the pulled image reports another platform")
		}

		if calls := r.callsFor("pull"); len(calls) != 1 || calls[0][2] != "linux/arm64" {
			t.Errorf("pull calls = %v, want one with --platform linux/arm64", calls)
		}
	})

	t.Run("pull failure is reported", func(t *testing.T) {
		b, _ := newBackend("linux/arm64", errors.New("boom"))

		err := b.ensureImage(context.Background(), ref, "linux/amd64")
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("ensureImage() error = %v, want it to wrap the pull failure", err)
		}
	})
}

func TestStartEnsuresPlatformImageBeforeRun(t *testing.T) {
	hostPort := listenLoopback(t)

	pull, image := cachedThenPulled("linux/amd64", "linux/arm64")

	r := &fakeRunner{respond: map[string]func(args []string) (string, error){
		"image": image,
		"pull":  pull,
		"run":   func(args []string) (string, error) { return "c1\n", nil },
		"port":  func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
	}}

	b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second}

	fn := discovery.Function{
		Name: "f", Dir: t.TempDir(), Runtime: "nodejs22.x", Handler: "index.handler",
		Manifest: &manifest.Manifest{Architectures: []string{"arm64"}},
	}

	inst, err := b.Start(context.Background(), fn)
	if err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}
	defer inst.Stop(context.Background())

	pulls := r.callsFor("pull")
	if len(pulls) != 1 || pulls[0][2] != "linux/arm64" {
		t.Errorf("pull calls = %v, want one with --platform linux/arm64 (cached amd64 image must not be reused)", pulls)
	}

	run := strings.Join(r.callsFor("run")[0], " ")
	if !strings.Contains(run, "--platform linux/arm64") {
		t.Errorf("run argv = %s, want --platform linux/arm64", run)
	}
}

// cachedThenPulled returns fake `pull` and `image` responders for a registry
// image cached as cached until pulled, after which it is pulledAs.
func cachedThenPulled(cached, pulledAs string) (pull, image func(args []string) (string, error)) {
	pulled := false

	pull = func(args []string) (string, error) {
		pulled = true
		return "", nil
	}

	image = func(args []string) (string, error) {
		if pulled {
			return pulledAs + "\n", nil
		}

		return cached + "\n", nil
	}

	return pull, image
}

func TestStartReportsExitedContainer(t *testing.T) {
	pull, image := cachedThenPulled("linux/amd64", "linux/arm64")

	r := &fakeRunner{respond: map[string]func(args []string) (string, error){
		"image": image,
		"pull":  pull,
		"run":   func(args []string) (string, error) { return "c1\n", nil },
		"port":  func(args []string) (string, error) { return "", errors.New("no public port '8080/tcp' published") },
		"inspect": func(args []string) (string, error) {
			return `[{"State":{"Status":"exited","Running":false,"ExitCode":255}}]`, nil
		},
		"logs": func(args []string) (string, error) { return "exec /var/runtime/bootstrap: exec format error\n", nil },
	}}

	b := &containerBackend{cli: "docker", runner: r, readyTimeout: time.Second}

	fn := discovery.Function{
		Name: "ping", Dir: t.TempDir(), Runtime: "nodejs22.x", Handler: "h",
		Manifest: &manifest.Manifest{Architectures: []string{"arm64"}},
	}

	_, err := b.Start(context.Background(), fn)
	if !errors.Is(err, backend.ErrStartFailed) {
		t.Fatalf("Start() error = %v, want errors.Is(err, backend.ErrStartFailed)", err)
	}

	want := "function ping: container exited with code 255: exec /var/runtime/bootstrap: exec format error"
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Start() error = %q, want prefix %q", err, want)
	}

	hint := "function ping targets arm64 but this Docker host cannot run arm64 images (install qemu/binfmt or set architectures: [x86_64])"
	if !strings.Contains(err.Error(), hint) {
		t.Errorf("Start() error = %q, want the arch hint %q", err, hint)
	}

	var sf *backend.StartFailedError
	if !errors.As(err, &sf) || sf.Function != "ping" {
		t.Errorf("errors.As(StartFailedError) = %v", sf)
	}

	if calls := r.callsFor("rm"); len(calls) != 1 {
		t.Errorf("rm calls = %d, want 1 (the exited container must be removed)", len(calls))
	}
}

func TestStartReadinessWaitStopsWhenContainerExits(t *testing.T) {
	r := &fakeRunner{respond: map[string]func(args []string) (string, error){
		"image": func(args []string) (string, error) { return "linux/amd64\n", nil },
		"run":   func(args []string) (string, error) { return "c1\n", nil },
		"port":  func(args []string) (string, error) { return "127.0.0.1:1\n", nil },
		"inspect": func(args []string) (string, error) {
			return `[{"State":{"Status":"exited","Running":false,"ExitCode":2}}]`, nil
		},
		"logs": func(args []string) (string, error) { return "line1\nline2\n", nil },
	}}

	b := &containerBackend{cli: "docker", runner: r, readyTimeout: 30 * time.Second}

	fn := discovery.Function{Name: "f", Dir: t.TempDir(), Runtime: "nodejs22.x", Handler: "h", Manifest: &manifest.Manifest{}}

	start := time.Now()
	_, err := b.Start(context.Background(), fn)

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Start() took %s, want it to stop waiting once the container exited", elapsed)
	}

	if !errors.Is(err, backend.ErrStartFailed) || !strings.Contains(err.Error(), "exited with code 2") || !strings.Contains(err.Error(), "line2") {
		t.Errorf("Start() error = %v, want an ErrStartFailed with exit code 2 and logs", err)
	}
}

func TestStartExitedContainerIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped: -short")
	}

	var runner backend.ExecRunner

	if _, err := runner.Run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("integration test skipped: docker daemon unreachable: %v", err)
	}

	const image = "public.ecr.aws/lambda/provided:al2023"

	// Without a handler argument this image's entrypoint exits at once; only
	// run against a locally cached copy so the test never pulls.
	if out, err := runner.Run(context.Background(), "docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", image); err != nil || !platformMatches(lastLine(string(out)), "linux/amd64") {
		t.Skipf("integration test skipped: %s not cached for linux/amd64", image)
	}

	b := &containerBackend{cli: "docker", runner: runner, readyTimeout: 20 * time.Second}

	fn := discovery.Function{
		Name: "lambdary-container-it-exits", Dir: t.TempDir(), Backend: "container", Image: image,
		Manifest: &manifest.Manifest{Architectures: []string{"x86_64"}},
	}

	_, err := b.Start(context.Background(), fn)
	if !errors.Is(err, backend.ErrStartFailed) {
		t.Fatalf("Start() error = %v, want backend.ErrStartFailed", err)
	}

	if !strings.Contains(err.Error(), "container exited with code") || !strings.Contains(err.Error(), "entrypoint requires the handler name") {
		t.Errorf("Start() error = %q, want the exit code and the container's output", err)
	}
}
