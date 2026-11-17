package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
	"github.com/open-southeners/lambdary/internal/metrics"
)

// metricsServer builds a router over b and fns that records into rec.
func metricsServer(b *fakeBackend, fns []discovery.Function, rec *metrics.Recorder) *Router {
	return NewServer(NewManager(b, fns), fns, Options{Metrics: rec})
}

// queryTotal sums metric's Sum datapoints for function (and resource, "" for
// the function-only series) over the last hour.
func queryTotal(rec *metrics.Recorder, metric, function, resource string) float64 {
	now := time.Now()

	var total float64

	for _, p := range rec.Query(metrics.Query{
		Metric:   metric,
		Function: function,
		Resource: resource,
		Start:    now.Add(-time.Hour),
		End:      now.Add(time.Minute),
		Period:   time.Hour,
		Stat:     "Sum",
	}) {
		total += p.Value
	}

	return total
}

func serve(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func TestRouterRecordsInvokeMetrics(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte(`{"ok":true}`), "application/json", 0)
	}

	fns := []discovery.Function{fn("greet")}
	rec := metrics.New()
	h := metricsServer(b, fns, rec)

	for range 3 {
		if got := serve(h, http.MethodPost, "/2015-03-31/functions/greet/invocations", "{}"); got.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", got.Code)
		}
	}

	if got := queryTotal(rec, metrics.Invocations, "greet", ""); got != 3 {
		t.Errorf("Invocations = %v, want 3", got)
	}
	if got := queryTotal(rec, metrics.Invocations, "greet", "greet:$LATEST"); got != 3 {
		t.Errorf("Invocations under greet:$LATEST = %v, want 3", got)
	}
	if got := queryTotal(rec, metrics.Errors, "greet", ""); got != 0 {
		t.Errorf("Errors = %v, want 0", got)
	}
}

func TestRouterRecordsFunctionErrors(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte(`{"errorType":"ValueError","errorMessage":"boom"}`), "application/json", 0)
	}

	fns := []discovery.Function{fn("boom")}
	rec := metrics.New()
	h := metricsServer(b, fns, rec)

	serve(h, http.MethodPost, "/2015-03-31/functions/boom/invocations", "{}")
	// A function route counts as $LATEST, and its 502 function error as an error.
	serve(h, http.MethodGet, "/boom", "")

	if got := queryTotal(rec, metrics.Invocations, "boom", "boom:$LATEST"); got != 2 {
		t.Errorf("Invocations = %v, want 2", got)
	}
	if got := queryTotal(rec, metrics.Errors, "boom", "boom:$LATEST"); got != 2 {
		t.Errorf("Errors = %v, want 2", got)
	}
}

func TestRouterRecordsUpstreamTimeoutAsError(t *testing.T) {
	slow := fn("slow")
	slow.Manifest = &manifest.Manifest{Timeout: 1}

	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte("ok"), "", 2*time.Second)
	}

	fns := []discovery.Function{slow}
	rec := metrics.New()
	h := metricsServer(b, fns, rec)

	if got := serve(h, http.MethodPost, "/2015-03-31/functions/slow/invocations", "{}"); got.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", got.Code)
	}

	if got := queryTotal(rec, metrics.Invocations, "slow", ""); got != 1 {
		t.Errorf("Invocations = %v, want 1", got)
	}
	if got := queryTotal(rec, metrics.Errors, "slow", ""); got != 1 {
		t.Errorf("Errors = %v, want 1", got)
	}
}

func TestRouterRecordsThrottleWhenDeadlineExpiresWaitingForLock(t *testing.T) {
	busy := fn("busy")
	busy.Manifest = &manifest.Manifest{Timeout: 1}

	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte("ok"), "", 0)
	}

	fns := []discovery.Function{busy}
	rec := metrics.New()
	rt := metricsServer(b, fns, rec)

	// Something else holds the invoke lock for longer than the invoke's
	// whole one-second budget.
	held := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan struct{})

	go func() {
		defer close(lockDone)
		_ = rt.mgr.WithLock("busy", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve(rt, http.MethodPost, "/2015-03-31/functions/busy/invocations", "{}") }()

	time.Sleep(1300 * time.Millisecond)
	close(release)
	<-lockDone

	if got := <-done; got.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", got.Code)
	}

	if got := queryTotal(rec, metrics.Throttles, "busy", ""); got != 1 {
		t.Errorf("Throttles = %v, want 1", got)
	}
	// A throttled invocation never ran: it counts only as a throttle.
	if got := queryTotal(rec, metrics.Invocations, "busy", ""); got != 0 {
		t.Errorf("Invocations = %v, want 0", got)
	}
	if got := queryTotal(rec, metrics.Errors, "busy", ""); got != 0 {
		t.Errorf("Errors = %v, want 0", got)
	}
}

func TestRouterRecordsAliasInvokeUnderVersionAndAlias(t *testing.T) {
	e := newCtlEnv(t)
	rec := metrics.New()
	e.h = NewServer(e.mgr, []discovery.Function{e.fn}, Options{Versions: e.store, Metrics: rec})

	e.publish(t, "one")
	e.publish(t, "two")
	e.createAlias(t, `{"Name":"live","FunctionVersion":"2"}`)

	for _, path := range []string{
		ctlPrefix + "hello/invocations?Qualifier=live",
		ctlPrefix + "hello:live/invocations",
		ctlPrefix + "hello:1/invocations",
		ctlPrefix + "hello/invocations",
	} {
		if got := e.do(http.MethodPost, path, "{}"); got.Code != http.StatusOK {
			t.Fatalf("POST %s = %d: %s", path, got.Code, got.Body.String())
		}
	}

	for resource, want := range map[string]float64{
		"hello:2":       2,
		"hello:live":    2,
		"hello:1":       1,
		"hello:$LATEST": 1,
		"":              4,
	} {
		if got := queryTotal(rec, metrics.Invocations, "hello", resource); got != want {
			t.Errorf("Invocations under %q = %v, want %v", resource, got, want)
		}
	}
}

func TestRouterInProcessInvokeRecordsMetrics(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte(`{}`), "application/json", 0)
	}

	fns := []discovery.Function{fn("trigger")}
	rec := metrics.New()
	rt := metricsServer(b, fns, rec)

	if _, err := rt.Invoke(context.Background(), "trigger", []byte("{}")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if got := queryTotal(rec, metrics.Invocations, "trigger", "trigger:$LATEST"); got != 1 {
		t.Errorf("Invocations = %v, want 1", got)
	}
}

func TestRouterMetricsEndpoint(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte(`{}`), "application/json", 0)
	}

	fns := []discovery.Function{fn("greet")}
	h := metricsServer(b, fns, metrics.New())

	serve(h, http.MethodPost, "/2015-03-31/functions/greet/invocations", "{}")

	rec := serve(h, http.MethodGet, "/_lambdary/metrics?window=15m", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Window    string `json:"window"`
		Functions []struct {
			Name  string `json:"name"`
			Total struct {
				Invocations int64 `json:"invocations"`
			} `json:"total"`
			Resources []struct {
				Resource    string `json:"resource"`
				Invocations int64  `json:"invocations"`
				Errors      int64  `json:"errors"`
				Throttles   int64  `json:"throttles"`
			} `json:"resources"`
		} `json:"functions"`
	}
	decodeJSON(t, rec, &payload)

	if payload.Window != "15m0s" {
		t.Errorf("window = %q, want 15m0s", payload.Window)
	}
	if len(payload.Functions) != 1 || payload.Functions[0].Name != "greet" || payload.Functions[0].Total.Invocations != 1 {
		t.Fatalf("functions = %+v, want greet with one invocation", payload.Functions)
	}
	if res := payload.Functions[0].Resources; len(res) != 1 || res[0].Resource != "greet:$LATEST" || res[0].Invocations != 1 {
		t.Errorf("resources = %+v, want greet:$LATEST with one invocation", res)
	}

	var def struct {
		Window string `json:"window"`
	}
	decodeJSON(t, serve(h, http.MethodGet, "/_lambdary/metrics", ""), &def)
	if def.Window != "1h0m0s" {
		t.Errorf("default window = %q, want 1h0m0s", def.Window)
	}

	for _, bad := range []string{"soon", "-5m", "0s"} {
		if got := serve(h, http.MethodGet, "/_lambdary/metrics?window="+bad, ""); got.Code != http.StatusBadRequest {
			t.Errorf("window=%s status = %d, want 400", bad, got.Code)
		}
	}
}

func TestRouterWithoutRecorderAnswers501(t *testing.T) {
	fns := []discovery.Function{fn("greet")}
	h := New(NewManager(newFakeBackend(), fns), fns)

	if got := serve(h, http.MethodGet, "/_lambdary/metrics", ""); got.Code != http.StatusNotImplemented {
		t.Errorf("metrics status = %d, want 501", got.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/service/GraniteServiceVersion20100801/operation/GetMetricData", strings.NewReader(""))
	req.Header.Set("Smithy-Protocol", "rpc-v2-cbor")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("GetMetricData status = %d, want 501", rec.Code)
	}
}

func TestRouterHealth(t *testing.T) {
	b := newFakeBackend()
	b.newInstance = func(string) *fakeInstance {
		return newFakeInstance(http.StatusOK, []byte(`{}`), "application/json", 0)
	}

	fns := []discovery.Function{fn("b"), fn("a")}

	// Health needs no recorder, so build with New.
	h := New(NewManager(b, fns), fns)

	type health struct {
		Status    string `json:"status"`
		Functions []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"functions"`
	}

	check := func(wantA, wantB string) {
		t.Helper()

		rec := serve(h, http.MethodGet, "/_lambdary/health", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		var got health
		decodeJSON(t, rec, &got)

		if got.Status != "ok" || len(got.Functions) != 2 ||
			got.Functions[0].Name != "a" || got.Functions[0].State != wantA ||
			got.Functions[1].Name != "b" || got.Functions[1].State != wantB {
			t.Errorf("health = %+v, want ok with a=%s b=%s", got, wantA, wantB)
		}
	}

	check("stopped", "stopped")

	serve(h, http.MethodPost, "/2015-03-31/functions/b/invocations", "{}")

	check("stopped", "running")
}
