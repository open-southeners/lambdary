package container

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

func TestAWSEndpointHost(t *testing.T) {
	aws := backend.AWS{Region: "us-east-1", Port: 8000}

	for _, tc := range []struct {
		name     string
		aws      backend.AWS
		p        Placement
		wantHost string
		wantAdd  bool
	}{
		{"disabled", backend.AWS{}, Placement{}, "", false},
		{"no dev server", backend.AWS{Region: "us-east-1"}, Placement{}, "", false},
		{"lambdary on the host", aws, Placement{}, "host.docker.internal", true},
		{"lambdary in a container", aws, Placement{Direct: true, Network: "n", SelfName: "lambdary-1"}, "lambdary-1", false},
		{"in a container, name unknown", aws, Placement{Direct: true, Network: "n"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, add := awsEndpointHost(tc.aws, tc.p)
			if host != tc.wantHost || add != tc.wantAdd {
				t.Errorf("awsEndpointHost() = %q, %v; want %q, %v", host, add, tc.wantHost, tc.wantAdd)
			}
		})
	}
}

func TestRunArgsAWSEnvironment(t *testing.T) {
	aws := backend.AWS{Region: "eu-south-2", Port: 9000}
	fn := discovery.Function{
		Name: "hello", Dir: "/root/hello",
		Manifest: &manifest.Manifest{Environment: map[string]string{"AWS_REGION": "mine"}},
	}

	fileEnv := aws.UnderEnv(map[string]string{"AWS_ACCESS_KEY_ID": "from-file", "AWS_SESSION_TOKEN": "from-file"}, "host.docker.internal")
	args := runArgs(fn, "img", "/abs/hello", fileEnv, "", "")

	for _, want := range []string{
		"AWS_REGION=mine", // the manifest beats everything
		"AWS_DEFAULT_REGION=eu-south-2",
		"AWS_ACCESS_KEY_ID=from-file", // env_file beats the injection
		"AWS_SECRET_ACCESS_KEY=lambdary",
		"AWS_SESSION_TOKEN=from-file",
		"AWS_ENDPOINT_URL=http://host.docker.internal:9000",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("argv = %v, missing -e %q", args, want)
		}
	}

	if slices.Contains(args, "AWS_REGION=eu-south-2") {
		t.Errorf("argv = %v, injected region overrode the manifest's", args)
	}
}

func TestWithAddHost(t *testing.T) {
	got := withAddHost([]string{"run", "-d", "--label", "x", "img"})
	want := []string{"run", "-d", "--add-host=host.docker.internal:host-gateway", "--label", "x", "img"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("withAddHost() = %v, want %v", got, want)
	}
}

func TestStartAWSEnvironment(t *testing.T) {
	start := func(t *testing.T, aws backend.AWS) []string {
		t.Helper()

		root := t.TempDir()
		fnDir := filepath.Join(root, "fn")
		putFile(t, filepath.Join(fnDir, "bootstrap"), "#!/bin/sh\n")

		hostPort := listenLoopback(t)

		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run":  func(args []string) (string, error) { return "cid\n", nil },
			"port": func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		b := &containerBackend{cli: "docker", runner: r, readyTimeout: 2 * time.Second, cacheDir: filepath.Join(root, ".lambdary"), aws: aws}

		inst, err := b.Start(context.Background(), discovery.Function{Name: "hello", Dir: fnDir, Runtime: "provided.al2023"})
		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		runs := r.callsFor("run")
		if len(runs) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runs))
		}

		return runs[0]
	}

	t.Run("lambdary on the host: gateway name and --add-host", func(t *testing.T) {
		run := start(t, backend.AWS{Region: "us-east-1", Port: 8000})

		for _, want := range []string{"--add-host=host.docker.internal:host-gateway", "AWS_ENDPOINT_URL=http://host.docker.internal:8000", "AWS_REGION=us-east-1"} {
			if !slices.Contains(run, want) {
				t.Errorf("run argv = %v, missing %q", run, want)
			}
		}
	})

	t.Run("disabled: no AWS variables and no --add-host", func(t *testing.T) {
		run := start(t, backend.AWS{})

		for _, a := range run {
			if strings.Contains(a, "host.docker.internal") || strings.HasPrefix(a, "AWS_REGION") || strings.HasPrefix(a, "AWS_ENDPOINT") {
				t.Errorf("run argv = %v, unexpected %q", run, a)
			}
		}
	})
}
