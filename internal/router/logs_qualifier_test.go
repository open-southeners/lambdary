package router

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/logbus"
)

func TestLogsQualifiers(t *testing.T) {
	e := newCtlEnv(t)
	e.publish(t, "one")
	e.publish(t, "two")
	e.createAlias(t, `{"Name":"live","FunctionVersion":"1","RoutingConfig":{"AdditionalVersionWeights":{"2":0.3}}}`)
	e.createAlias(t, `{"Name":"head","FunctionVersion":"$LATEST"}`)

	bus := logbus.New()
	bus.Publish("hello", "latest")
	bus.Publish("hello:1", "v1")
	bus.Publish("hello:2", "v2")
	bus.Publish("hello:3", "unrelated")
	bus.Publish("hellox", "other function")

	fns := []discovery.Function{e.fn}
	h := NewServer(e.mgr, fns, Options{Versions: e.store, Logs: bus})

	texts := func(query string) string {
		t.Helper()

		got := serve(h, http.MethodGet, "/_lambdary/logs?function="+query, "")
		if got.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", query, got.Code, got.Body.String())
		}

		var out []string
		for _, l := range decodeLines(t, got.Body.String()) {
			out = append(out, l.Text)
		}

		return strings.Join(out, ",")
	}

	tests := map[string]string{
		"hello":         "latest,v1,v2,unrelated",
		"hello:1":       "v1",
		"hello:2":       "v2",
		"hello:$LATEST": "latest",
		"hello:live":    "v1,v2",
		"hello:head":    "latest",
	}

	for query, want := range tests {
		if got := texts(query); got != want {
			t.Errorf("function=%s: got %q, want %q", query, got, want)
		}
	}

	for _, query := range []string{"nope", "nope:1", "hello:9", "hello:ghost"} {
		got := serve(h, http.MethodGet, "/_lambdary/logs?function="+query, "")
		if got.Code != http.StatusNotFound || !strings.Contains(got.Body.String(), "not found") {
			t.Errorf("function=%s: status %d: %s", query, got.Code, got.Body.String())
		}
	}
}
