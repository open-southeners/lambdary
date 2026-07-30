package container

import (
	"reflect"
	"testing"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

func TestRunArgs(t *testing.T) {
	t.Run("full manifest: env, memory, platform, handler", func(t *testing.T) {
		fn := discovery.Function{
			Name:    "hello",
			Dir:     "/root/hello",
			Handler: "lambda_function.handler",
			Manifest: &manifest.Manifest{
				Timeout:       15,
				Memory:        256,
				Architectures: []string{"arm64"},
				Environment: map[string]string{
					"TABLE_NAME": "local-table",
					"API_KEY":    "secret",
				},
			},
		}

		got := runArgs(fn, "public.ecr.aws/lambda/python:3.13", "/abs/hello")

		want := []string{
			"run", "-d", "--rm",
			"--label", "lambdary=1",
			"--label", "lambdary.function=hello",
			"-p", "127.0.0.1:0:8080",
			"-v", "/abs/hello:/var/task:ro",
			"-e", "API_KEY=secret",
			"-e", "TABLE_NAME=local-table",
			"-e", "AWS_LAMBDA_FUNCTION_NAME=hello",
			"-e", "AWS_LAMBDA_FUNCTION_TIMEOUT=15",
			"--memory", "256m",
			"-e", "AWS_LAMBDA_FUNCTION_MEMORY_SIZE=256",
			"--platform", "linux/arm64",
			"public.ecr.aws/lambda/python:3.13",
			"lambda_function.handler",
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("runArgs() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("x86_64 architecture maps to linux/amd64", func(t *testing.T) {
		fn := discovery.Function{
			Name:     "hello",
			Manifest: &manifest.Manifest{Architectures: []string{"x86_64"}},
		}

		got := runArgs(fn, "image", "/abs/hello")

		want := []string{
			"run", "-d", "--rm",
			"--label", "lambdary=1",
			"--label", "lambdary.function=hello",
			"-p", "127.0.0.1:0:8080",
			"-v", "/abs/hello:/var/task:ro",
			"-e", "AWS_LAMBDA_FUNCTION_NAME=hello",
			"--platform", "linux/amd64",
			"image",
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("runArgs() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("minimal function: no env, memory, architectures, or handler", func(t *testing.T) {
		fn := discovery.Function{
			Name:     "minimal",
			Manifest: &manifest.Manifest{},
		}

		got := runArgs(fn, "public.ecr.aws/lambda/nodejs:22", "/abs/minimal")

		want := []string{
			"run", "-d", "--rm",
			"--label", "lambdary=1",
			"--label", "lambdary.function=minimal",
			"-p", "127.0.0.1:0:8080",
			"-v", "/abs/minimal:/var/task:ro",
			"-e", "AWS_LAMBDA_FUNCTION_NAME=minimal",
			"public.ecr.aws/lambda/nodejs:22",
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("runArgs() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("nil manifest behaves like an empty one", func(t *testing.T) {
		fn := discovery.Function{Name: "no-manifest"}

		got := runArgs(fn, "image", "/abs/no-manifest")

		want := []string{
			"run", "-d", "--rm",
			"--label", "lambdary=1",
			"--label", "lambdary.function=no-manifest",
			"-p", "127.0.0.1:0:8080",
			"-v", "/abs/no-manifest:/var/task:ro",
			"-e", "AWS_LAMBDA_FUNCTION_NAME=no-manifest",
			"image",
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("runArgs() =\n%v\nwant\n%v", got, want)
		}
	})
}

func TestResolveImage(t *testing.T) {
	t.Run("local.image override wins over ImageFor", func(t *testing.T) {
		fn := discovery.Function{Name: "hello", Runtime: "python3.13", Image: "my-registry/custom:tag"}

		got, err := resolveImage(fn)
		if err != nil {
			t.Fatalf("resolveImage() unexpected error: %v", err)
		}
		if got != "my-registry/custom:tag" {
			t.Errorf("resolveImage() = %q, want the local.image override", got)
		}
	})

	t.Run("falls back to backend.ImageFor(runtime)", func(t *testing.T) {
		fn := discovery.Function{Name: "hello", Runtime: "python3.13"}

		got, err := resolveImage(fn)
		if err != nil {
			t.Fatalf("resolveImage() unexpected error: %v", err)
		}
		if got != "public.ecr.aws/lambda/python:3.13" {
			t.Errorf("resolveImage() = %q, want the ImageFor mapping", got)
		}
	})

	t.Run("Dockerfile-marker function errors as not yet supported", func(t *testing.T) {
		fn := discovery.Function{Name: "docker-app", Backend: "container"}

		_, err := resolveImage(fn)
		if err == nil {
			t.Fatal("resolveImage() expected error, got nil")
		}
		if err != ErrDockerfileNotSupported {
			t.Errorf("resolveImage() error = %v, want ErrDockerfileNotSupported", err)
		}
	})

	t.Run("local.image override wins even for a Dockerfile-marker function", func(t *testing.T) {
		fn := discovery.Function{Name: "docker-app", Backend: "container", Image: "my-registry/custom:tag"}

		got, err := resolveImage(fn)
		if err != nil {
			t.Fatalf("resolveImage() unexpected error: %v", err)
		}
		if got != "my-registry/custom:tag" {
			t.Errorf("resolveImage() = %q, want the local.image override", got)
		}
	})
}
