package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
	"github.com/open-southeners/lambdary/internal/versions"
)

// listEnv serves the named functions (given out of order) behind a version
// store, so ListFunctions has several to sort and page.
func listEnv(t *testing.T, names ...string) (http.Handler, *versions.Store, []discovery.Function) {
	t.Helper()

	var fns []discovery.Function

	for _, name := range names {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		setCode(t, dir, "one")

		fns = append(fns, discovery.Function{
			Name:     name,
			Dir:      dir,
			Route:    "/" + name,
			Runtime:  "nodejs22.x",
			Handler:  "index.handler",
			Backend:  "process",
			Manifest: &manifest.Manifest{},
		})
	}

	mgr := NewManager(newVersionBackend(), fns)
	store := versions.NewStore(t.TempDir())

	t.Cleanup(func() { _ = mgr.StopAll(context.Background()) })

	return NewWithVersions(mgr, fns, store), store, fns
}

func functionNames(out []lambdatypes.FunctionConfiguration) []string {
	names := make([]string, len(out))
	for i, f := range out {
		names[i] = aws.ToString(f.FunctionName) + ":" + aws.ToString(f.Version)
	}

	return names
}

func wantNames(t *testing.T, got []lambdatypes.FunctionConfiguration, want ...string) {
	t.Helper()

	names := functionNames(got)
	if len(names) != len(want) {
		t.Fatalf("functions = %v, want %v", names, want)
	}

	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("functions = %v, want %v", names, want)
		}
	}
}

func TestListFunctions(t *testing.T) {
	h, store, fns := listEnv(t, "charlie", "alpha", "bravo")
	client := sdkClient(t, h)
	ctx := context.Background()

	t.Run("default is sorted $LATEST", func(t *testing.T) {
		out, err := client.ListFunctions(ctx, &lambda.ListFunctionsInput{})
		if err != nil {
			t.Fatal(err)
		}

		wantNames(t, out.Functions, "alpha:$LATEST", "bravo:$LATEST", "charlie:$LATEST")

		if out.NextMarker != nil {
			t.Errorf("NextMarker = %s, want none", aws.ToString(out.NextMarker))
		}

		if got := aws.ToString(out.Functions[0].Handler); got != "index.handler" {
			t.Errorf("handler = %s", got)
		}
	})

	t.Run("trailing slash", func(t *testing.T) {
		srv := httptest.NewServer(h)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/2015-03-31/functions/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d", resp.StatusCode)
		}
	})

	t.Run("paging", func(t *testing.T) {
		first, err := client.ListFunctions(ctx, &lambda.ListFunctionsInput{MaxItems: aws.Int32(2)})
		if err != nil {
			t.Fatal(err)
		}

		wantNames(t, first.Functions, "alpha:$LATEST", "bravo:$LATEST")

		if first.NextMarker == nil {
			t.Fatal("no NextMarker on a truncated page")
		}

		second, err := client.ListFunctions(ctx, &lambda.ListFunctionsInput{MaxItems: aws.Int32(2), Marker: first.NextMarker})
		if err != nil {
			t.Fatal(err)
		}

		wantNames(t, second.Functions, "charlie:$LATEST")

		if second.NextMarker != nil {
			t.Errorf("NextMarker = %s on the last page", aws.ToString(second.NextMarker))
		}
	})

	t.Run("invalid MaxItems", func(t *testing.T) {
		_, err := client.ListFunctions(ctx, &lambda.ListFunctionsInput{MaxItems: aws.Int32(0)})
		wantAPIError(t, err, errInvalidParameter)
	})

	t.Run("FunctionVersion ALL", func(t *testing.T) {
		for _, fn := range fns {
			if fn.Name != "bravo" {
				continue
			}

			if _, err := store.Publish(ctx, fn, versions.PublishInput{}, nil); err != nil {
				t.Fatal(err)
			}
		}

		out, err := client.ListFunctions(ctx, &lambda.ListFunctionsInput{FunctionVersion: lambdatypes.FunctionVersionAll})
		if err != nil {
			t.Fatal(err)
		}

		wantNames(t, out.Functions, "alpha:$LATEST", "bravo:$LATEST", "bravo:1", "charlie:$LATEST")

		plain, err := client.ListFunctions(ctx, &lambda.ListFunctionsInput{})
		if err != nil {
			t.Fatal(err)
		}

		wantNames(t, plain.Functions, "alpha:$LATEST", "bravo:$LATEST", "charlie:$LATEST")
	})
}

func TestListFunctionsWithoutVersionStore(t *testing.T) {
	_, _, fns := listEnv(t, "solo")

	client := sdkClient(t, New(NewManager(newVersionBackend(), fns), fns))

	out, err := client.ListFunctions(context.Background(), &lambda.ListFunctionsInput{FunctionVersion: lambdatypes.FunctionVersionAll})
	if err != nil {
		t.Fatal(err)
	}

	wantNames(t, out.Functions, "solo:$LATEST")
}

func TestListFunctionsSkipsNameCollisions(t *testing.T) {
	_, _, fns := listEnv(t, "dup", "ok")
	fns = append(fns, fns[0])

	client := sdkClient(t, New(NewManager(newVersionBackend(), fns), fns))

	out, err := client.ListFunctions(context.Background(), &lambda.ListFunctionsInput{})
	if err != nil {
		t.Fatal(err)
	}

	wantNames(t, out.Functions, "ok:$LATEST")
}

func TestUnsupportedControlCalls(t *testing.T) {
	h, _, _ := listEnv(t, "hello")
	client := sdkClient(t, h)

	_, err := client.CreateFunction(context.Background(), &lambda.CreateFunctionInput{
		FunctionName: aws.String("new"),
		Role:         aws.String("arn:aws:iam::000000000000:role/x"),
		Code:         &lambdatypes.FunctionCode{ZipFile: []byte("zip")},
	})
	wantAPIError(t, err, errUnsupported)

	for _, path := range []string{"/2015-03-31/event-source-mappings/", "/2015-03-31/layers"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusNotImplemented || rec.Header().Get(errorTypeHeader) != errUnsupported {
			t.Errorf("GET %s = %d %q, want 501 %s", path, rec.Code, rec.Header().Get(errorTypeHeader), errUnsupported)
		}
	}
}
