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

		got := runArgs(fn, "public.ecr.aws/lambda/python:3.13", "/abs/hello", nil)

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

		got := runArgs(fn, "image", "/abs/hello", nil)

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

		got := runArgs(fn, "public.ecr.aws/lambda/nodejs:22", "/abs/minimal", nil)

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

		got := runArgs(fn, "image", "/abs/no-manifest", nil)

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

	t.Run("fileEnv is folded in under manifest environment", func(t *testing.T) {
		fn := discovery.Function{
			Name: "hello",
			Manifest: &manifest.Manifest{
				Environment: map[string]string{"API_KEY": "secret"},
			},
		}

		fileEnv := map[string]string{"API_KEY": "from-file", "EXTRA": "from-file-only"}

		got := runArgs(fn, "image", "/abs/hello", fileEnv)

		want := []string{
			"run", "-d", "--rm",
			"--label", "lambdary=1",
			"--label", "lambdary.function=hello",
			"-p", "127.0.0.1:0:8080",
			"-v", "/abs/hello:/var/task:ro",
			"-e", "API_KEY=secret",
			"-e", "EXTRA=from-file-only",
			"-e", "AWS_LAMBDA_FUNCTION_NAME=hello",
			"image",
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("runArgs() =\n%v\nwant\n%v (explicit environment must win over fileEnv on conflict)", got, want)
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

	t.Run("Dockerfile-marker function with no override falls through to ImageFor", func(t *testing.T) {
		// resolveImage itself no longer special-cases Dockerfile-marker
		// functions — Start builds an image and sets fn.Image before ever
		// calling resolveImage for one (see build_test.go). Called
		// directly with neither an override nor a resolvable runtime, it
		// just surfaces ImageFor's own error.
		fn := discovery.Function{Name: "docker-app", Backend: "container"}

		_, err := resolveImage(fn)
		if err == nil {
			t.Fatal("resolveImage() expected error, got nil")
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
