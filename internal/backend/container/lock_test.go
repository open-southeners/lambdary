package container

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
)

// fakeLock is a test-local Lock double, recording every ImageDigest lookup
// and RecordImage call so tests can assert both the digest-used-when-
// present behaviour and the record-after-start flow.
type fakeLock struct {
	mu sync.Mutex

	digests map[string]string

	digestQueries []string
	recorded      []struct{ tag, digest string }
	recordErr     error
}

func (f *fakeLock) ImageDigest(tag string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.digestQueries = append(f.digestQueries, tag)

	digest, ok := f.digests[tag]

	return digest, ok
}

func (f *fakeLock) RecordImage(tag, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.recorded = append(f.recorded, struct{ tag, digest string }{tag, digest})

	return f.recordErr
}

func TestContainerBackendStartDigestPinning(t *testing.T) {
	t.Run("digest present in lock: run argv uses <repo>@sha256:..., no re-recording", func(t *testing.T) {
		hostPort := listenLoopback(t)

		lock := &fakeLock{digests: map[string]string{
			"public.ecr.aws/lambda/python:3.13": "sha256:deadbeef",
		}}

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "container123\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second, lock: lock}

		fn := discovery.Function{
			Name:    "hello",
			Dir:     t.TempDir(),
			Handler: "lambda_function.handler",
			Runtime: "python3.13",
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}

		argv := runCalls[0]
		wantImage := "public.ecr.aws/lambda/python@sha256:deadbeef"
		found := false
		for _, a := range argv {
			if a == wantImage {
				found = true
			}
		}
		if !found {
			t.Errorf("run argv = %v, want it to contain the digest ref %q", argv, wantImage)
		}

		if calls := r.callsFor("inspect"); len(calls) != 0 {
			t.Errorf("inspect calls = %d, want 0 (already pinned, nothing to resolve)", len(calls))
		}

		if len(lock.recorded) != 0 {
			t.Errorf("recorded = %v, want no re-recording of an already-pinned tag", lock.recorded)
		}
	})

	t.Run("no digest in lock: run argv uses the plain tag, then records after success", func(t *testing.T) {
		hostPort := listenLoopback(t)

		lock := &fakeLock{digests: map[string]string{}}

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":     func(args []string) (string, error) { return "container123\n", nil },
			"port":    func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop":    func(args []string) (string, error) { return "", nil },
			"inspect": func(args []string) (string, error) { return "public.ecr.aws/lambda/python@sha256:cafef00d\n", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second, lock: lock}

		fn := discovery.Function{
			Name:    "hello",
			Dir:     t.TempDir(),
			Handler: "lambda_function.handler",
			Runtime: "python3.13",
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		argv := runCalls[0]
		wantImage := "public.ecr.aws/lambda/python:3.13"
		found := false
		for _, a := range argv {
			if a == wantImage {
				found = true
			}
		}
		if !found {
			t.Errorf("run argv = %v, want it to contain the unpinned tag %q", argv, wantImage)
		}

		inspectCalls := r.callsFor("inspect")
		if len(inspectCalls) != 1 {
			t.Fatalf("inspect calls = %d, want 1", len(inspectCalls))
		}
		if inspectCalls[0][len(inspectCalls[0])-1] != "public.ecr.aws/lambda/python:3.13" {
			t.Errorf("inspect argv = %v, want it to inspect the tag just started", inspectCalls[0])
		}

		if len(lock.recorded) != 1 {
			t.Fatalf("recorded = %v, want exactly one RecordImage call", lock.recorded)
		}
		if got := lock.recorded[0]; got.tag != "public.ecr.aws/lambda/python:3.13" || got.digest != "sha256:cafef00d" {
			t.Errorf("recorded = %+v, want tag=public.ecr.aws/lambda/python:3.13 digest=sha256:cafef00d", got)
		}
	})

	t.Run("inspect yields no RepoDigests: nothing recorded, Start still succeeds", func(t *testing.T) {
		hostPort := listenLoopback(t)

		lock := &fakeLock{digests: map[string]string{}}

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":     func(args []string) (string, error) { return "container123\n", nil },
			"port":    func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop":    func(args []string) (string, error) { return "", nil },
			"inspect": func(args []string) (string, error) { return "\n", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second, lock: lock}

		fn := discovery.Function{
			Name:    "hello",
			Dir:     t.TempDir(),
			Handler: "lambda_function.handler",
			Runtime: "python3.13",
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		if len(lock.recorded) != 0 {
			t.Errorf("recorded = %v, want none when inspect has no RepoDigests entry", lock.recorded)
		}
	})

	t.Run("build tags are never pinned or recorded", func(t *testing.T) {
		dir := t.TempDir()
		writeDockerfile(t, dir)

		hostPort := listenLoopback(t)

		lock := &fakeLock{digests: map[string]string{
			"lambdary/docker-app:local": "sha256:shouldnotbeused",
		}}

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"build":   func(args []string) (string, error) { return "Successfully built abc123\n", nil },
			"run":     func(args []string) (string, error) { return "container123\n", nil },
			"port":    func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop":    func(args []string) (string, error) { return "", nil },
			"inspect": func(args []string) (string, error) { return "should-not-be-called@sha256:x\n", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second, lock: lock}

		fn := discovery.Function{Name: "docker-app", Dir: dir, Backend: "container"}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		if argv := runCalls[0]; argv[len(argv)-1] != "lambdary/docker-app:local" {
			t.Errorf("run argv last element = %q, want the plain build tag, not a digest ref", argv[len(argv)-1])
		}

		if calls := r.callsFor("inspect"); len(calls) != 0 {
			t.Errorf("inspect calls = %d, want 0 for a build tag", len(calls))
		}
		if len(lock.recorded) != 0 {
			t.Errorf("recorded = %v, want none for a build tag", lock.recorded)
		}
	})

	t.Run("local.image override is never pinned or recorded", func(t *testing.T) {
		hostPort := listenLoopback(t)

		lock := &fakeLock{digests: map[string]string{
			"my-registry/custom:tag": "sha256:shouldnotbeused",
		}}

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":     func(args []string) (string, error) { return "container123\n", nil },
			"port":    func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop":    func(args []string) (string, error) { return "", nil },
			"inspect": func(args []string) (string, error) { return "should-not-be-called@sha256:x\n", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second, lock: lock}

		fn := discovery.Function{
			Name:    "hello",
			Dir:     t.TempDir(),
			Backend: "container",
			Image:   "my-registry/custom:tag",
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		runCalls := r.callsFor("run")
		if len(runCalls) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runCalls))
		}
		if argv := runCalls[0]; argv[len(argv)-1] != "my-registry/custom:tag" {
			t.Errorf("run argv last element = %q, want the plain override, not a digest ref", argv[len(argv)-1])
		}

		if calls := r.callsFor("inspect"); len(calls) != 0 {
			t.Errorf("inspect calls = %d, want 0 for a local.image override", len(calls))
		}
		if len(lock.recorded) != 0 {
			t.Errorf("recorded = %v, want none for a local.image override", lock.recorded)
		}
	})

	t.Run("nil lock: zero behaviour change from before Unit C", func(t *testing.T) {
		hostPort := listenLoopback(t)

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "container123\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second}

		fn := discovery.Function{
			Name:    "hello",
			Dir:     t.TempDir(),
			Handler: "lambda_function.handler",
			Runtime: "python3.13",
		}

		inst, err := b.Start(context.Background(), fn)
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		if calls := r.callsFor("inspect"); len(calls) != 0 {
			t.Errorf("inspect calls = %d, want 0 with no lock configured", len(calls))
		}
	})
}

func TestNewWithLockUsableAsBackend(t *testing.T) {
	// NewWithLock must return a value satisfying backend.Backend (compile-
	// time check exercised at runtime too), exactly like New.
	var lock fakeLock

	b := NewWithLock("docker", &fakeRunner{}, &lock, t.TempDir())
	if b == nil {
		t.Fatal("NewWithLock() returned nil")
	}
}

func TestDigestRef(t *testing.T) {
	tests := []struct {
		name   string
		image  string
		digest string
		want   string
	}{
		{"tagged image", "public.ecr.aws/lambda/python:3.13", "sha256:abc", "public.ecr.aws/lambda/python@sha256:abc"},
		{"registry with port, no tag", "localhost:5000/app", "sha256:abc", "localhost:5000/app@sha256:abc"},
		{"registry with port and tag", "localhost:5000/app:latest", "sha256:abc", "localhost:5000/app@sha256:abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := digestRef(tt.image, tt.digest); got != tt.want {
				t.Errorf("digestRef(%q, %q) = %q, want %q", tt.image, tt.digest, got, tt.want)
			}
		})
	}
}

func TestParseRepoDigest(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantDigest string
		wantOK     bool
	}{
		{"well-formed", "public.ecr.aws/lambda/python@sha256:abcdef\n", "sha256:abcdef", true},
		{"blank", "\n", "", false},
		{"blank no newline", "", "", false},
		{"no @", "public.ecr.aws/lambda/python", "", false},
		{"trailing @", "public.ecr.aws/lambda/python@", "", false},
		{"not sha256", "public.ecr.aws/lambda/python@md5:abcdef", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRepoDigest(tt.in)
			if got != tt.wantDigest || ok != tt.wantOK {
				t.Errorf("parseRepoDigest(%q) = %q, %v, want %q, %v", tt.in, got, ok, tt.wantDigest, tt.wantOK)
			}
		})
	}
}
