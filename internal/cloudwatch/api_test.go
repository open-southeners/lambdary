package cloudwatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/encoding/cbor"

	"github.com/open-southeners/lambdary/internal/metrics"
)

// newClient returns the real CloudWatch SDK client pointed at an API over
// rec, so the test speaks the exact bytes an application would send.
func newClient(t *testing.T, rec *metrics.Recorder) *cloudwatch.Client {
	t.Helper()

	api := NewAPI(rec)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Handles(r) {
			http.Error(w, "not a CloudWatch call", http.StatusNotFound)
			return
		}

		api.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return cloudwatch.New(cloudwatch.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("local", "local", ""),
		RetryMaxAttempts: 1,
	})
}

// functionQuery builds a query the way Kiarga does: one metric of one
// function and Resource dimension value.
func functionQuery(id, metric, stat, function, resource string, period int32) cwtypes.MetricDataQuery {
	dims := []cwtypes.Dimension{
		{Name: aws.String("FunctionName"), Value: aws.String(function)},
	}
	if resource != "" {
		dims = append(dims, cwtypes.Dimension{Name: aws.String("Resource"), Value: aws.String(resource)})
	}

	return cwtypes.MetricDataQuery{
		Id: aws.String(id),
		MetricStat: &cwtypes.MetricStat{
			Metric: &cwtypes.Metric{Namespace: aws.String("AWS/Lambda"), MetricName: aws.String(metric), Dimensions: dims},
			Period: aws.Int32(period),
			Stat:   aws.String(stat),
		},
	}
}

func TestGetMetricData(t *testing.T) {
	rec := metrics.New()
	now := time.Now()

	for i := range 4 {
		rec.Record(metrics.Invocation{
			Function:  "ping",
			Resources: []string{"ping:1", "ping:live"},
			Start:     now.Add(-10 * time.Minute),
			Duration:  time.Duration(10*(i+1)) * time.Millisecond,
			Error:     i == 0,
		})
	}
	rec.Record(metrics.Invocation{Function: "ping", Resources: []string{"ping:2"}, Start: now.Add(-5 * time.Minute), Duration: time.Millisecond})
	rec.Record(metrics.Invocation{Function: "ping", Resources: []string{"ping:1"}, Start: now.Add(-5 * time.Minute), Throttled: true})

	client := newClient(t, rec)

	out, err := client.GetMetricData(context.Background(), &cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(now.Add(-time.Hour)),
		EndTime:   aws.Time(now),
		MetricDataQueries: []cwtypes.MetricDataQuery{
			functionQuery("inv0", "Invocations", "Sum", "ping", "ping:1", 3600),
			functionQuery("err0", "Errors", "Sum", "ping", "ping:1", 3600),
			functionQuery("thr0", "Throttles", "Sum", "ping", "ping:1", 3600),
			functionQuery("dur0", "Duration", "p95", "ping", "ping:1", 3600),
			functionQuery("inv1", "Invocations", "Sum", "ping", "ping:live", 3600),
			functionQuery("inv2", "Invocations", "Sum", "ping", "ping:2", 3600),
			functionQuery("all", "Invocations", "Sum", "ping", "", 3600),
			functionQuery("none", "Invocations", "Sum", "ping", "ping:9", 3600),
			functionQuery("other", "Invocations", "Sum", "pong", "", 3600),
		},
	})
	if err != nil {
		t.Fatalf("GetMetricData: %v", err)
	}

	got := map[string][]float64{}
	for _, r := range out.MetricDataResults {
		if r.StatusCode != cwtypes.StatusCodeComplete {
			t.Errorf("%s: StatusCode = %q, want Complete", aws.ToString(r.Id), r.StatusCode)
		}
		if len(r.Timestamps) != len(r.Values) {
			t.Errorf("%s: %d timestamps for %d values", aws.ToString(r.Id), len(r.Timestamps), len(r.Values))
		}

		got[aws.ToString(r.Id)] = r.Values
	}

	// The last hour as one period of 3600s, with the start a few seconds past a
	// minute boundary, still yields a single datapoint per query.
	for id, want := range map[string]float64{
		"inv0": 4, "err0": 1, "thr0": 1, "dur0": 40, "inv1": 4, "inv2": 1, "all": 5,
	} {
		if len(got[id]) != 1 || got[id][0] != want {
			t.Errorf("%s = %v, want [%v]", id, got[id], want)
		}
	}

	for _, id := range []string{"none", "other"} {
		if r, ok := got[id]; !ok || len(r) != 0 {
			t.Errorf("%s = %v (present: %v), want a result without values", id, r, ok)
		}
	}
}

func TestGetMetricDataOrdersByScanBy(t *testing.T) {
	rec := metrics.New()
	now := time.Now()

	rec.Record(metrics.Invocation{Function: "ping", Start: now.Add(-30 * time.Minute), Duration: time.Millisecond})
	rec.Record(metrics.Invocation{Function: "ping", Start: now.Add(-20 * time.Minute), Duration: time.Millisecond})
	rec.Record(metrics.Invocation{Function: "ping", Start: now.Add(-20 * time.Minute), Duration: time.Millisecond})

	client := newClient(t, rec)

	for scan, want := range map[cwtypes.ScanBy][]float64{
		cwtypes.ScanByTimestampDescending: {2, 1},
		cwtypes.ScanByTimestampAscending:  {1, 2},
	} {
		out, err := client.GetMetricData(context.Background(), &cloudwatch.GetMetricDataInput{
			StartTime:         aws.Time(now.Add(-time.Hour)),
			EndTime:           aws.Time(now),
			ScanBy:            scan,
			MetricDataQueries: []cwtypes.MetricDataQuery{functionQuery("q", "Invocations", "Sum", "ping", "", 600)},
		})
		if err != nil {
			t.Fatalf("%s: %v", scan, err)
		}

		if v := out.MetricDataResults[0].Values; fmt.Sprint(v) != fmt.Sprint(want) {
			t.Errorf("%s: values = %v, want %v", scan, v, want)
		}
	}
}

func TestGetMetricDataReturnDataFalseOmitsResult(t *testing.T) {
	client := newClient(t, metrics.New())

	q := functionQuery("hidden", "Invocations", "Sum", "ping", "", 60)
	q.ReturnData = aws.Bool(false)

	out, err := client.GetMetricData(context.Background(), &cloudwatch.GetMetricDataInput{
		StartTime:         aws.Time(time.Now().Add(-time.Hour)),
		EndTime:           aws.Time(time.Now()),
		MetricDataQueries: []cwtypes.MetricDataQuery{q, functionQuery("shown", "Invocations", "Sum", "ping", "", 60)},
	})
	if err != nil {
		t.Fatalf("GetMetricData: %v", err)
	}

	if len(out.MetricDataResults) != 1 || aws.ToString(out.MetricDataResults[0].Id) != "shown" {
		t.Errorf("results = %+v, want only the query that returns data", out.MetricDataResults)
	}
}

func TestGetMetricDataRejectsUnsupportedQueries(t *testing.T) {
	client := newClient(t, metrics.New())

	namespace := functionQuery("q", "Invocations", "Sum", "ping", "", 60)
	namespace.MetricStat.Metric.Namespace = aws.String("AWS/EC2")

	dimension := functionQuery("q", "Invocations", "Sum", "ping", "", 60)
	dimension.MetricStat.Metric.Dimensions = append(dimension.MetricStat.Metric.Dimensions, cwtypes.Dimension{Name: aws.String("Version"), Value: aws.String("1")})

	noFunction := functionQuery("q", "Invocations", "Sum", "ping", "", 60)
	noFunction.MetricStat.Metric.Dimensions = nil

	expression := cwtypes.MetricDataQuery{Id: aws.String("q"), Expression: aws.String("SUM(METRICS())")}

	for name, q := range map[string]cwtypes.MetricDataQuery{
		"namespace":   namespace,
		"dimension":   dimension,
		"no function": noFunction,
		"expression":  expression,
		"bad stat":    functionQuery("q", "Invocations", "Median", "ping", "", 60),
		"zero period": functionQuery("q", "Invocations", "Sum", "ping", "", 0),
	} {
		_, err := client.GetMetricData(context.Background(), &cloudwatch.GetMetricDataInput{
			StartTime:         aws.Time(time.Now().Add(-time.Hour)),
			EndTime:           aws.Time(time.Now()),
			MetricDataQueries: []cwtypes.MetricDataQuery{q},
		})

		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidParameterValueException" {
			t.Errorf("%s: err = %v, want InvalidParameterValueException", name, err)
		}
	}
}

func TestOtherOperationsAreRejected(t *testing.T) {
	api := NewAPI(metrics.New())

	req := httptest.NewRequest(http.MethodPost, servicePath+"ListMetrics", bytes.NewReader(cbor.Encode(cbor.Map{})))
	req.Header.Set(protocolHeader, protocolValue)

	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	v, err := cbor.Decode(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("decoding error body: %v", err)
	}

	m, _ := v.(cbor.Map)
	if m["__type"] != cbor.String("InvalidParameterValueException") {
		t.Errorf("__type = %v, want InvalidParameterValueException", m["__type"])
	}
	if rec.Header().Get(protocolHeader) != protocolValue || rec.Header().Get("Content-Type") != "application/cbor" {
		t.Errorf("headers = %v, want the RPCv2 CBOR protocol and content type", rec.Header())
	}
}

func TestNilRecorderAnswers501(t *testing.T) {
	_, err := newClient(t, nil).GetMetricData(context.Background(), &cloudwatch.GetMetricDataInput{
		StartTime:         aws.Time(time.Now().Add(-time.Hour)),
		EndTime:           aws.Time(time.Now()),
		MetricDataQueries: []cwtypes.MetricDataQuery{functionQuery("q", "Invocations", "Sum", "ping", "", 60)},
	})
	if err == nil {
		t.Fatal("GetMetricData without a recorder succeeded, want an error")
	}
}

func TestHandles(t *testing.T) {
	for name, tc := range map[string]struct {
		method, path, protocol string
		want                   bool
	}{
		"cloudwatch call": {http.MethodPost, servicePath + "GetMetricData", protocolValue, true},
		"wrong protocol":  {http.MethodPost, servicePath + "GetMetricData", "", false},
		"wrong path":      {http.MethodPost, "/2015-03-31/functions/x/invocations", protocolValue, false},
		"wrong method":    {http.MethodGet, servicePath + "GetMetricData", protocolValue, false},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.protocol != "" {
			req.Header.Set(protocolHeader, tc.protocol)
		}

		if got := Handles(req); got != tc.want {
			t.Errorf("%s: Handles = %v, want %v", name, got, tc.want)
		}
	}
}
