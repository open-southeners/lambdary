package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/open-southeners/lambdary/internal/backend"
)

// TestE2EVersionsAndAliases drives versions and aliases through the real AWS
// CLI against a real dev server, on each backend, proving the feature's
// done-when: `aws lambda publish-version`, `update-alias --routing-config` and
// `invoke --qualifier live` behave as on AWS, a canary at 10% sends about 1 in
// 10 invocations to the canary version, and everything survives a restart. It
// uses the Go module fixture, so the frozen artifact is a built binary.
func TestE2EVersionsAndAliases(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test skipped: -short")
	}

	for _, tool := range []string{"aws", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("e2e test skipped: %s not on PATH: %v", tool, err)
		}
	}

	t.Run("process", func(t *testing.T) { runVersionsE2E(t, "process") })

	t.Run("container", func(t *testing.T) {
		if _, err := (backend.ExecRunner{}).Run(context.Background(), "docker", "info"); err != nil {
			t.Skipf("docker daemon unreachable: %v", err)
		}

		runVersionsE2E(t, "container")
	})
}

func runVersionsE2E(t *testing.T, backendName string) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("testdata/go-module")); err != nil {
		t.Fatalf("copying fixture: %v", err)
	}

	srv := startVersionsDev(t, root, backendName)
	cli := awsCLI{t: t, endpoint: srv.url, dir: t.TempDir()}

	// Publishing twice with nothing changed yields one version.
	v := cli.jsonOK("publish-version", "--function-name", "hello", "--description", "sha=abc built=now")
	wantField(t, v, "Version", "1")
	wantField(t, v, "Description", "sha=abc built=now")

	if sha, _ := v["CodeSha256"].(string); sha == "" {
		t.Errorf("publish-version CodeSha256 is empty: %v", v)
	}

	wantField(t, cli.jsonOK("publish-version", "--function-name", "hello"), "Version", "1")

	// Hot reload rebuilds $LATEST from the edited code, which publishes as 2.
	editGreeting(t, root, "v1", "v2")
	waitForLatest(t, srv.url, "v2")
	wantField(t, cli.jsonOK("publish-version", "--function-name", "hello"), "Version", "2")

	// An alias runs its own version, while $LATEST keeps the newest code.
	wantField(t, cli.jsonOK("create-alias", "--function-name", "hello", "--name", "live", "--function-version", "1"), "FunctionVersion", "1")
	cli.wantInvoke("live", "1", "hello says v1")
	cli.wantInvoke("", "$LATEST", "hello says v2")

	// With a 10% canary set, about 1 in 10 invocations goes to it.
	cli.jsonOK("update-alias", "--function-name", "hello", "--name", "live", "--routing-config", `{"AdditionalVersionWeights":{"2":0.1}}`)

	alias := cli.jsonOK("get-alias", "--function-name", "hello", "--name", "live")
	wantField(t, alias, "FunctionVersion", "1")

	weights, _ := alias["RoutingConfig"].(map[string]any)["AdditionalVersionWeights"].(map[string]any)
	if weights["2"] != 0.1 {
		t.Errorf("get-alias weights = %v, want 2=0.1", weights)
	}

	// 400 draws at 10%: mean 40, standard deviation 6, so these bounds are
	// more than 4 deviations out and don't flake.
	if canary := countExecuted(t, srv.url, "live", 400)["2"]; canary < 15 || canary > 70 {
		t.Errorf("canary got %d of 400 invocations, want about 40", canary)
	}

	// An empty routing config clears the canary, which is how Kiarga does it.
	cli.jsonOK("update-alias", "--function-name", "hello", "--name", "live", "--function-version", "1", "--routing-config", `{"AdditionalVersionWeights":{}}`)

	if counts := countExecuted(t, srv.url, "live", 30); counts["1"] != 30 {
		t.Errorf("after clearing the canary, executed = %v, want only version 1", counts)
	}

	// Published versions ignore later edits to the source.
	editGreeting(t, root, "v2", "v3")
	waitForLatest(t, srv.url, "v3")
	cli.wantInvoke("1", "1", "hello says v1")
	cli.wantInvoke("2", "2", "hello says v2")

	versions := cli.jsonOK("list-versions-by-function", "--function-name", "hello")["Versions"].([]any)
	if len(versions) != 3 {
		t.Fatalf("list-versions-by-function returned %d versions, want $LATEST, 1, 2: %v", len(versions), versions)
	}

	for i, want := range []string{"$LATEST", "1", "2"} {
		if got := versions[i].(map[string]any)["Version"]; got != want {
			t.Errorf("Versions[%d].Version = %v, want %s", i, got, want)
		}
	}

	// AWS's error types reach the CLI, as SDK callers rely on them.
	if stderr := cli.fails("get-alias", "--function-name", "hello", "--name", "missing"); !strings.Contains(stderr, "ResourceNotFoundException") {
		t.Errorf("get-alias on a missing alias: stderr = %q, want ResourceNotFoundException", stderr)
	}
	if stderr := cli.fails("create-alias", "--function-name", "hello", "--name", "live", "--function-version", "1"); !strings.Contains(stderr, "ResourceConflictException") {
		t.Errorf("create-alias on an existing alias: stderr = %q, want ResourceConflictException", stderr)
	}

	checkKiargaSDKCalls(t, srv.url)

	// Versions and aliases survive a dev server restart.
	srv.stop(t)

	srv = startVersionsDev(t, root, backendName)
	cli.endpoint = srv.url

	wantField(t, cli.jsonOK("get-alias", "--function-name", "hello", "--name", "live"), "FunctionVersion", "1")
	cli.wantInvoke("live", "1", "hello says v1")
	wantField(t, cli.jsonOK("publish-version", "--function-name", "hello"), "Version", "3")
}

// checkKiargaSDKCalls makes the calls Kiarga's staff panel makes through the Go
// SDK, which serializes and parses independently of the CLI: the versions
// paginator, GetAlias, UpdateAlias with an empty weights map to clear a canary,
// a qualified Invoke, and matching the typed not-found error.
func checkKiargaSDKCalls(t *testing.T, endpoint string) {
	t.Helper()

	ctx := context.Background()

	client := lambda.New(lambda.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})

	var seen []string

	pages := lambda.NewListVersionsByFunctionPaginator(client, &lambda.ListVersionsByFunctionInput{
		FunctionName: aws.String("hello"),
		MaxItems:     aws.Int32(1),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Fatalf("SDK ListVersionsByFunction: %v", err)
		}

		for _, v := range page.Versions {
			if _, err := time.Parse("2006-01-02T15:04:05.000-0700", aws.ToString(v.LastModified)); err != nil {
				t.Errorf("SDK LastModified: %v", err)
			}

			seen = append(seen, aws.ToString(v.Version))
		}
	}

	if got := strings.Join(seen, ","); got != "$LATEST,1,2" {
		t.Errorf("SDK versions = %s, want $LATEST,1,2", got)
	}

	if _, err := client.UpdateAlias(ctx, &lambda.UpdateAliasInput{
		FunctionName:    aws.String("hello"),
		Name:            aws.String("live"),
		FunctionVersion: aws.String("1"),
		RoutingConfig:   &lambdatypes.AliasRoutingConfiguration{AdditionalVersionWeights: map[string]float64{"2": 0.25}},
	}); err != nil {
		t.Fatalf("SDK UpdateAlias: %v", err)
	}

	alias, err := client.GetAlias(ctx, &lambda.GetAliasInput{FunctionName: aws.String("hello"), Name: aws.String("live")})
	if err != nil {
		t.Fatalf("SDK GetAlias: %v", err)
	}
	if alias.RoutingConfig == nil || alias.RoutingConfig.AdditionalVersionWeights["2"] != 0.25 {
		t.Errorf("SDK GetAlias routing = %+v, want 2=0.25", alias.RoutingConfig)
	}

	if _, err := client.UpdateAlias(ctx, &lambda.UpdateAliasInput{
		FunctionName:    aws.String("hello"),
		Name:            aws.String("live"),
		FunctionVersion: aws.String("1"),
		RoutingConfig:   &lambdatypes.AliasRoutingConfiguration{AdditionalVersionWeights: map[string]float64{}},
	}); err != nil {
		t.Fatalf("SDK UpdateAlias clearing the canary: %v", err)
	}

	res, err := client.Invoke(ctx, &lambda.InvokeInput{
		FunctionName: aws.String("hello"),
		Qualifier:    aws.String("live"),
		Payload:      []byte("{}"),
	})
	if err != nil {
		t.Fatalf("SDK Invoke: %v", err)
	}
	if aws.ToString(res.ExecutedVersion) != "1" || res.FunctionError != nil {
		t.Errorf("SDK Invoke: ExecutedVersion = %q, FunctionError = %v", aws.ToString(res.ExecutedVersion), res.FunctionError)
	}

	var notFound *lambdatypes.ResourceNotFoundException

	_, err = client.GetAlias(ctx, &lambda.GetAliasInput{FunctionName: aws.String("hello"), Name: aws.String("missing")})
	if !errors.As(err, &notFound) {
		t.Errorf("SDK GetAlias on a missing alias: err = %v, want ResourceNotFoundException", err)
	}

	_, err = client.Invoke(ctx, &lambda.InvokeInput{FunctionName: aws.String("no-such-function"), Payload: []byte("{}")})
	if !errors.As(err, &notFound) {
		t.Errorf("SDK Invoke on a missing function: err = %v, want ResourceNotFoundException", err)
	}
}

// versionsDev is one running dev server.
type versionsDev struct {
	url    string
	cancel context.CancelFunc
	done   chan error
}

func startVersionsDev(t *testing.T, root, backendName string) *versionsDev {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	d := newDevServer(io.Discard, io.Discard, root, freeTCPPort(t), false, backendName, false)

	srv := &versionsDev{cancel: cancel, done: make(chan error, 1)}

	go func() { srv.done <- d.run(ctx) }()

	srv.url = "http://" + waitDevServerReady(t, d, 5*time.Minute)

	t.Cleanup(func() { srv.stop(t) })

	return srv
}

func (s *versionsDev) stop(t *testing.T) {
	t.Helper()

	if s.done == nil {
		return
	}

	s.cancel()

	select {
	case err := <-s.done:
		if err != nil {
			t.Errorf("devServer.run() error = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Errorf("dev server did not stop")
	}

	s.done = nil
}

// awsCLI runs `aws lambda` against the dev server with throwaway credentials.
type awsCLI struct {
	t        *testing.T
	endpoint string
	dir      string
}

func (a awsCLI) run(args ...string) (stdout, stderr string, err error) {
	a.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	full := append([]string{"lambda", "--endpoint-url", a.endpoint, "--output", "json"}, args...)
	cmd := exec.CommandContext(ctx, "aws", full...)
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID=local",
		"AWS_SECRET_ACCESS_KEY=local",
		"AWS_DEFAULT_REGION=us-east-1",
		"AWS_PAGER=",
		"AWS_MAX_ATTEMPTS=1",
		"AWS_EC2_METADATA_DISABLED=true",
	)

	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf

	err = cmd.Run()

	return out.String(), errBuf.String(), err
}

func (a awsCLI) jsonOK(args ...string) map[string]any {
	a.t.Helper()

	stdout, stderr, err := a.run(args...)
	if err != nil {
		a.t.Fatalf("aws lambda %v: %v\nstderr: %s", args, err, stderr)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		a.t.Fatalf("aws lambda %v: parsing %q: %v", args, stdout, err)
	}

	return parsed
}

func (a awsCLI) fails(args ...string) string {
	a.t.Helper()

	_, stderr, err := a.run(args...)
	if err == nil {
		a.t.Fatalf("aws lambda %v succeeded, want failure", args)
	}

	return stderr
}

// wantInvoke invokes hello through `aws lambda invoke` and checks which
// version ran and what it answered.
func (a awsCLI) wantInvoke(qualifier, wantExecuted, wantBody string) {
	a.t.Helper()

	out := filepath.Join(a.dir, "out.json")
	args := []string{"invoke", "--function-name", "hello", "--cli-binary-format", "raw-in-base64-out", "--payload", "{}"}

	if qualifier != "" {
		args = append(args, "--qualifier", qualifier)
	}

	result := a.jsonOK(append(args, out)...)
	wantField(a.t, result, "ExecutedVersion", wantExecuted)

	if _, failed := result["FunctionError"]; failed {
		a.t.Errorf("invoke %q reported FunctionError: %v", qualifier, result)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		a.t.Fatalf("reading invoke output: %v", err)
	}

	if !strings.Contains(string(body), wantBody) {
		a.t.Errorf("invoke %q body = %s, want it to contain %q", qualifier, body, wantBody)
	}
}

func wantField(t *testing.T, got map[string]any, field, want string) {
	t.Helper()

	if got[field] != want {
		t.Errorf("%s = %v, want %q (in %v)", field, got[field], want, got)
	}
}

// editGreeting changes the text the shared greet package answers with.
func editGreeting(t *testing.T, root, from, to string) {
	t.Helper()

	path := filepath.Join(root, "internal", "greet", "greet.go")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	edited := strings.Replace(string(data), "says "+from, "says "+to, 1)
	if edited == string(data) {
		t.Fatalf("%s has no %q to replace", path, from)
	}

	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func invokeURL(base, qualifier string) string {
	url := base + "/2015-03-31/functions/hello/invocations"
	if qualifier != "" {
		url += "?Qualifier=" + qualifier
	}

	return url
}

// waitForLatest polls $LATEST until hot reload has it answering with text.
func waitForLatest(t *testing.T, base, text string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Minute)

	for time.Now().Before(deadline) {
		resp, err := http.Post(invokeURL(base, ""), "application/json", strings.NewReader("{}"))
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if strings.Contains(string(body), text) {
				return
			}
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("$LATEST never answered with %q", text)
}

// countExecuted invokes qualifier n times, returning how many invocations each
// version ran. It also checks that each answer came from the version the
// response names, so the header can't drift from what actually ran.
func countExecuted(t *testing.T, base, qualifier string, n int) map[string]int {
	t.Helper()

	counts := map[string]int{}

	for i := 0; i < n; i++ {
		resp, err := http.Post(invokeURL(base, qualifier), "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("invoke %d: %v", i, err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("invoke %d: reading body: %v", i, err)
		}

		executed := resp.Header.Get("X-Amz-Executed-Version")
		counts[executed]++

		if !strings.Contains(string(body), "says v"+executed) {
			t.Fatalf("invoke %d: executed version %q answered %s", i, executed, body)
		}
	}

	return counts
}
