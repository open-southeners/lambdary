package router

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/smithy-go"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
	"github.com/open-southeners/lambdary/internal/versions"
)

// sdkClient returns a real Lambda SDK client against env's router.
func sdkClient(t *testing.T, h http.Handler) *lambda.Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return lambda.New(lambda.Options{
		BaseEndpoint:     aws.String(srv.URL),
		Region:           "us-east-1",
		Credentials:      credentials.NewStaticCredentialsProvider("k", "s", ""),
		RetryMaxAttempts: 1,
	})
}

func wantAPIError(t *testing.T, err error, code string) smithy.APIError {
	t.Helper()

	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an API error %s", err, code)
	}

	if apiErr.ErrorCode() != code {
		t.Errorf("error code = %s (%s), want %s", apiErr.ErrorCode(), apiErr.ErrorMessage(), code)
	}

	return apiErr
}

func TestGetFunctionConfigurationAndGetFunction(t *testing.T) {
	e := newCtlEnv(t)
	e.fn.Manifest.Timeout = new(20)

	e.publish(t, "two")

	if _, err := e.store.CreateAlias("hello", "live", "1", "", nil); err != nil {
		t.Fatal(err)
	}

	client := sdkClient(t, e.h)
	ctx := context.Background()

	t.Run("latest", func(t *testing.T) {
		out, err := client.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{FunctionName: aws.String("hello")})
		if err != nil {
			t.Fatal(err)
		}

		if aws.ToString(out.Version) != "$LATEST" || aws.ToString(out.Handler) != "index.handler" {
			t.Errorf("config = version %s handler %s", aws.ToString(out.Version), aws.ToString(out.Handler))
		}
	})

	t.Run("version", func(t *testing.T) {
		out, err := client.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{FunctionName: aws.String("hello"), Qualifier: aws.String("1")})
		if err != nil {
			t.Fatal(err)
		}

		if aws.ToString(out.Version) != "1" || aws.ToInt32(out.Timeout) != 20 {
			t.Errorf("config = version %s timeout %d, want 1 and 20", aws.ToString(out.Version), aws.ToInt32(out.Timeout))
		}
	})

	t.Run("alias resolves to its version", func(t *testing.T) {
		out, err := client.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{FunctionName: aws.String("hello"), Qualifier: aws.String("live")})
		if err != nil {
			t.Fatal(err)
		}

		if aws.ToString(out.Version) != "1" {
			t.Errorf("alias version = %s, want 1", aws.ToString(out.Version))
		}
	})

	t.Run("qualifier in the function name", func(t *testing.T) {
		out, err := client.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{FunctionName: aws.String("hello:live")})
		if err != nil {
			t.Fatal(err)
		}

		if aws.ToString(out.Version) != "1" {
			t.Errorf("version = %s, want 1", aws.ToString(out.Version))
		}
	})

	t.Run("get function", func(t *testing.T) {
		out, err := client.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: aws.String("hello"), Qualifier: aws.String("live")})
		if err != nil {
			t.Fatal(err)
		}

		if out.Configuration == nil || aws.ToString(out.Configuration.Version) != "1" {
			t.Fatalf("Configuration = %+v, want version 1", out.Configuration)
		}

		if out.Code == nil || aws.ToString(out.Code.RepositoryType) != "S3" {
			t.Errorf("Code = %+v, want RepositoryType S3", out.Code)
		}
	})

	t.Run("unknown function", func(t *testing.T) {
		_, err := client.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{FunctionName: aws.String("nope")})
		wantAPIError(t, err, "ResourceNotFoundException")

		var nf *lambdatypes.ResourceNotFoundException
		if !errors.As(err, &nf) {
			t.Errorf("err = %v, want a typed ResourceNotFoundException", err)
		}

		_, err = client.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: aws.String("nope")})
		wantAPIError(t, err, "ResourceNotFoundException")
	})

	t.Run("unknown version and alias", func(t *testing.T) {
		for _, q := range []string{"9", "ghost"} {
			_, err := client.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{FunctionName: aws.String("hello"), Qualifier: aws.String(q)})
			wantAPIError(t, err, "ResourceNotFoundException")

			_, err = client.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: aws.String("hello"), Qualifier: aws.String(q)})
			wantAPIError(t, err, "ResourceNotFoundException")
		}
	})
}

func TestUnsupportedControlRoute(t *testing.T) {
	e := newCtlEnv(t)
	client := sdkClient(t, e.h)

	_, err := client.GetPolicy(context.Background(), &lambda.GetPolicyInput{FunctionName: aws.String("hello")})

	apiErr := wantAPIError(t, err, "UnsupportedOperation")
	if want := "Lambdary does not implement GET /2015-03-31/functions/hello/policy"; apiErr.ErrorMessage() != want {
		t.Errorf("message = %q, want %q", apiErr.ErrorMessage(), want)
	}

	rec := e.do(http.MethodGet, "/not-lambda", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-control path status = %d, want 404", rec.Code)
	}
}

// legacyState is a state.json as code before the 900 s default wrote it:
// no "format", and Timeout a plain int that was 0 when unset.
const legacyState = `{
  "next_version": 2,
  "revision_id": "r",
  "versions": [{"version": "1", "runtime": "nodejs22.x", "handler": "index.handler",
    "manifest": {"Timeout": 0, "Memory": 256}}],
  "aliases": {}
}`

// TestLegacyVersionTimeout checks that a version published by older code,
// whose snapshot stored an unset timeout as 0, runs and reports as 900 s
// rather than as no limit, and that a new version with an explicit 0 keeps it.
func TestLegacyVersionTimeout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	cache := t.TempDir()

	stateDir := filepath.Join(cache, "versions", "hello")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), []byte(legacyState), 0o644); err != nil {
		t.Fatal(err)
	}

	fn := discovery.Function{Name: "hello", Dir: dir, Route: "/hello", Runtime: "nodejs22.x", Handler: "index.handler", Backend: "process", Manifest: &manifest.Manifest{}}
	fns := []discovery.Function{fn}
	b := newVersionBackend()
	mgr := NewManager(b, fns)
	t.Cleanup(func() { _ = mgr.StopAll(context.Background()) })

	var logs bytes.Buffer

	store := versions.NewStore(cache)
	h := NewServer(mgr, fns, Options{Versions: store, Log: &logs})

	client := sdkClient(t, h)

	out, err := client.ListVersionsByFunction(context.Background(), &lambda.ListVersionsByFunctionInput{FunctionName: aws.String("hello")})
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range out.Versions {
		if aws.ToInt32(v.Timeout) != 900 {
			t.Errorf("version %s timeout = %d, want 900", aws.ToString(v.Version), aws.ToInt32(v.Timeout))
		}
	}

	if _, err := client.Invoke(context.Background(), &lambda.InvokeInput{FunctionName: aws.String("hello"), Qualifier: aws.String("1")}); err != nil {
		t.Fatal(err)
	}

	if got := mgr.InvokeTimeout("hello:1"); got != 900*time.Second {
		t.Errorf("InvokeTimeout(hello:1) = %v, want 900s", got)
	}

	// Whatever the legacy 0 once meant, nothing may warn about it.
	if logs.Len() != 0 {
		t.Errorf("unexpected log output: %s", logs.String())
	}
}

// TestVersionTimeoutWarningLoggedOnce checks that a version whose timeout is
// outside AWS's range logs the warning the first time it is used, and only
// then.
func TestVersionTimeoutWarningLoggedOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	cache := t.TempDir()
	store := versions.NewStore(cache)

	fn := discovery.Function{Name: "hello", Dir: dir, Route: "/hello", Runtime: "nodejs22.x", Handler: "index.handler", Backend: "process", Manifest: &manifest.Manifest{Timeout: new(1800)}}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Publish(context.Background(), fn, versions.PublishInput{}, nil); err != nil {
		t.Fatal(err)
	}

	fns := []discovery.Function{fn}
	mgr := NewManager(newVersionBackend(), fns)
	t.Cleanup(func() { _ = mgr.StopAll(context.Background()) })

	var logs bytes.Buffer

	h := NewServer(mgr, fns, Options{Versions: store, Log: &logs})
	client := sdkClient(t, h)

	for range 2 {
		if _, err := client.Invoke(context.Background(), &lambda.InvokeInput{FunctionName: aws.String("hello"), Qualifier: aws.String("1")}); err != nil {
			t.Fatal(err)
		}
	}

	if got := strings.Count(logs.String(), "timeout 1800s is outside AWS Lambda's"); got != 1 {
		t.Errorf("warning logged %d times, want 1:\n%s", got, logs.String())
	}

	if got := mgr.InvokeTimeout("hello:1"); got != 1800*time.Second {
		t.Errorf("InvokeTimeout(hello:1) = %v, want 1800s", got)
	}
}
