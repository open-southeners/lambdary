package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// boomMain is a function that fails every invocation through the Runtime API's
// error endpoint, so the metrics test has real function errors to count.
const boomMain = `package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
)

func main() {
	base := "http://" + os.Getenv("AWS_LAMBDA_RUNTIME_API") + "/2018-06-01/runtime/invocation/"

	for {
		next, err := http.Get(base + "next")
		if err != nil {
			os.Exit(1)
		}
		io.Copy(io.Discard, next.Body)
		next.Body.Close()

		done, err := http.Post(base+next.Header.Get("Lambda-Runtime-Aws-Request-Id")+"/error", "application/json",
			bytes.NewReader([]byte(` + "`" + `{"errorType":"Boom","errorMessage":"always fails"}` + "`" + `)))
		if err != nil {
			os.Exit(1)
		}
		done.Body.Close()
	}
}
`

// TestE2EMetricsAndHealth proves the metrics done-when against a real dev
// server running real Go functions: invocations through a weighted alias count
// per executed version and under the alias, errors count, and the real
// CloudWatch SDK client reads them through GetMetricData with the queries
// Kiarga's staff panel sends. The JSON endpoint and health report the same.
func TestE2EMetricsAndHealth(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test skipped: -short")
	}

	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("e2e test skipped: go not on PATH: %v", err)
	}

	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("testdata/go-module")); err != nil {
		t.Fatalf("copying fixture: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(root, "cmd", "boom"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cmd", "boom", "main.go"), []byte(boomMain), 0o644); err != nil {
		t.Fatal(err)
	}

	config := `functions:
  hello:
    go: ./cmd/hello
  boom:
    go: ./cmd/boom
build:
  go:
    ldflags: "-X example.com/gomodule/internal/greet.Version=e2e"
`
	if err := os.WriteFile(filepath.Join(root, "lambdary.yml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := startVersionsDev(t, root, "process")

	ctx := context.Background()
	creds := credentials.NewStaticCredentialsProvider("local", "local", "")

	lambdaClient := lambda.New(lambda.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.url), Credentials: creds})
	cw := cloudwatch.New(cloudwatch.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.url), Credentials: creds})

	// Nothing has run yet: every function is stopped.
	wantHealth(t, srv.url, map[string]string{"boom": "stopped", "hello": "stopped"})

	// Versions 1 and 2 differ by their greeting; the alias splits traffic
	// evenly between them.
	if _, err := lambdaClient.PublishVersion(ctx, &lambda.PublishVersionInput{FunctionName: aws.String("hello")}); err != nil {
		t.Fatalf("PublishVersion 1: %v", err)
	}

	editGreeting(t, root, "v1", "v2")
	waitForLatest(t, srv.url, "v2")

	if _, err := lambdaClient.PublishVersion(ctx, &lambda.PublishVersionInput{FunctionName: aws.String("hello")}); err != nil {
		t.Fatalf("PublishVersion 2: %v", err)
	}

	if _, err := lambdaClient.CreateAlias(ctx, &lambda.CreateAliasInput{
		FunctionName:    aws.String("hello"),
		Name:            aws.String("live"),
		FunctionVersion: aws.String("1"),
		RoutingConfig:   &lambdatypes.AliasRoutingConfiguration{AdditionalVersionWeights: map[string]float64{"2": 0.5}},
	}); err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}

	const invocations = 40

	executed := countExecuted(t, srv.url, "live", invocations)
	if executed["1"]+executed["2"] != invocations || executed["1"] == 0 || executed["2"] == 0 {
		t.Fatalf("executed versions = %v, want both versions to have run", executed)
	}

	// waitForLatest above also invoked $LATEST at least once, so only the
	// versions and the alias have exact expectations.
	for i := 0; i < 3; i++ {
		res, err := lambdaClient.Invoke(ctx, &lambda.InvokeInput{FunctionName: aws.String("boom"), Payload: []byte("{}")})
		if err != nil {
			t.Fatalf("Invoke boom: %v", err)
		}
		if aws.ToString(res.FunctionError) == "" {
			t.Fatalf("Invoke boom %d: no FunctionError, want the function to fail", i)
		}
	}

	versions := []string{"1", "2"}

	got := functionMetrics(t, cw, "hello", versions, time.Hour)

	for i, v := range versions {
		if got[i].invocations != float64(executed[v]) {
			t.Errorf("hello:%s Invocations = %v, want %d", v, got[i].invocations, executed[v])
		}
		if got[i].errors != 0 || got[i].throttles != 0 {
			t.Errorf("hello:%s Errors = %v, Throttles = %v, want 0", v, got[i].errors, got[i].throttles)
		}
		if got[i].durationP95 <= 0 {
			t.Errorf("hello:%s Duration p95 = %v, want > 0", v, got[i].durationP95)
		}
	}

	// The alias series counts every invocation made through it.
	live := functionMetrics(t, cw, "hello", []string{"live"}, time.Hour)
	if live[0].invocations != invocations {
		t.Errorf("hello:live Invocations = %v, want %d", live[0].invocations, invocations)
	}

	boom := functionMetrics(t, cw, "boom", []string{"$LATEST"}, time.Hour)
	if boom[0].invocations != 3 || boom[0].errors != 3 {
		t.Errorf("boom:$LATEST Invocations = %v, Errors = %v, want 3 and 3", boom[0].invocations, boom[0].errors)
	}

	// Lambdary's own endpoint agrees.
	snapshot := getJSON(t, srv.url+"/_lambdary/metrics?window=1h")

	var found bool

	for _, f := range snapshot["functions"].([]any) {
		fn := f.(map[string]any)
		if fn["name"] != "boom" {
			continue
		}

		found = true

		total := fn["total"].(map[string]any)
		if total["invocations"] != float64(3) || total["errors"] != float64(3) {
			t.Errorf("/_lambdary/metrics boom total = %v, want 3 invocations and 3 errors", total)
		}
	}

	if !found {
		t.Errorf("/_lambdary/metrics = %v, want boom listed", snapshot)
	}

	// Hot reload may have restarted $LATEST after its last invoke, so run it
	// once more before expecting every function to be up.
	waitForLatest(t, srv.url, "v2")

	wantHealth(t, srv.url, map[string]string{"boom": "running", "hello": "running"})
}

// versionMetrics is what Kiarga reads for one version.
type versionMetrics struct {
	invocations, errors, throttles, durationP95 float64
}

// functionMetrics asks GetMetricData for each of versions exactly as Kiarga's
// FunctionMetrics does: ids inv/err/thr/dur plus the version's index,
// FunctionName and Resource=name:version dimensions, and a period equal to
// the window rounded down to a minute. Invocation, error and throttle values
// are summed and the duration is the first value.
func functionMetrics(t *testing.T, cw *cloudwatch.Client, function string, versions []string, window time.Duration) []versionMetrics {
	t.Helper()

	end := time.Now()
	start := end.Add(-window)

	period := int32(window / time.Second)
	if period < 60 {
		period = 60
	}

	period -= period % 60

	var queries []cwtypes.MetricDataQuery

	for i, v := range versions {
		dims := []cwtypes.Dimension{
			{Name: aws.String("FunctionName"), Value: aws.String(function)},
			{Name: aws.String("Resource"), Value: aws.String(function + ":" + v)},
		}

		for _, metric := range []struct{ id, name, stat string }{
			{"inv", "Invocations", "Sum"}, {"err", "Errors", "Sum"},
			{"thr", "Throttles", "Sum"}, {"dur", "Duration", "p95"},
		} {
			queries = append(queries, cwtypes.MetricDataQuery{
				Id: aws.String(fmt.Sprintf("%s%d", metric.id, i)),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{Namespace: aws.String("AWS/Lambda"), MetricName: aws.String(metric.name), Dimensions: dims},
					Period: aws.Int32(period),
					Stat:   aws.String(metric.stat),
				},
			})
		}
	}

	out := make([]versionMetrics, len(versions))

	pages := cloudwatch.NewGetMetricDataPaginator(cw, &cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(start), EndTime: aws.Time(end), MetricDataQueries: queries,
	})

	for pages.HasMorePages() {
		page, err := pages.NextPage(context.Background())
		if err != nil {
			t.Fatalf("SDK GetMetricData: %v", err)
		}

		for _, r := range page.MetricDataResults {
			id := aws.ToString(r.Id)

			var idx int
			if _, err := fmt.Sscanf(id[3:], "%d", &idx); err != nil || idx >= len(out) {
				continue
			}

			var total float64
			for _, v := range r.Values {
				total += v
			}

			switch id[:3] {
			case "inv":
				out[idx].invocations = total
			case "err":
				out[idx].errors = total
			case "thr":
				out[idx].throttles = total
			case "dur":
				if len(r.Values) > 0 {
					out[idx].durationP95 = r.Values[0]
				}
			}
		}
	}

	return out
}

// getJSON GETs url and decodes its JSON object body.
func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: reading body: %v", url, err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, body)
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("GET %s: parsing %q: %v", url, body, err)
	}

	return parsed
}

// wantHealth checks GET /_lambdary/health reports ok and each function's state.
func wantHealth(t *testing.T, base string, want map[string]string) {
	t.Helper()

	health := getJSON(t, base+"/_lambdary/health")
	if health["status"] != "ok" {
		t.Errorf("health status = %v, want ok", health["status"])
	}

	got := map[string]string{}

	for _, f := range health["functions"].([]any) {
		fn := f.(map[string]any)
		got[fn["name"].(string)] = fn["state"].(string)
	}

	for name, state := range want {
		if got[name] != state {
			t.Errorf("health: %s is %q, want %q (all: %v)", name, got[name], state, got)
		}
	}
}
