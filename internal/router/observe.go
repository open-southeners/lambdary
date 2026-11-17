package router

import (
	"fmt"
	"net/http"
	"time"

	"github.com/open-southeners/lambdary/internal/metrics"
	"github.com/open-southeners/lambdary/internal/versions"
)

// Lambdary's own observability endpoints, alongside the AWS-compatible ones.
const (
	metricsPath = "/_lambdary/metrics"
	healthPath  = "/_lambdary/health"
)

// defaultMetricsWindow is how far back GET /_lambdary/metrics looks when the
// request names no window.
const defaultMetricsWindow = time.Hour

// invocationMeta says which series an invocation counts under: the Lambdary
// function name and the Resource dimension values (name:version, plus
// name:alias when the invoke went through an alias).
type invocationMeta struct {
	function  string
	resources []string
}

// invocationMeta builds the metrics labels of an invoke of name that ran
// executed. refQualifier and queryQualifier are the qualifier as given in the
// function reference and in ?Qualifier=; either may be empty. A qualifier
// that is neither $LATEST nor the version that ran must be an alias, which
// is counted too, as AWS counts an alias invoke under both its Resource
// dimension values. Function routes and unqualified invokes pass $LATEST.
func (rt *Router) invocationMeta(name, executed, refQualifier, queryQualifier string) invocationMeta {
	meta := invocationMeta{function: name, resources: []string{name + ":" + executed}}

	qualifier := refQualifier
	if qualifier == "" {
		qualifier = queryQualifier
	}

	if qualifier != "" && qualifier != versions.Latest && qualifier != executed {
		meta.resources = append(meta.resources, name+":"+qualifier)
	}

	return meta
}

// record stores one finished invocation, a no-op without a recorder.
// throttled invocations never ran, so the recorder counts them only as
// throttles.
func (rt *Router) record(meta invocationMeta, started time.Time, failed, throttled bool) {
	if rt.metrics == nil {
		return
	}

	rt.metrics.Record(metrics.Invocation{
		Function:  meta.function,
		Resources: meta.resources,
		Start:     started,
		Duration:  time.Since(started),
		Error:     failed,
		Throttled: throttled,
	})
}

// handleMetrics serves GET /_lambdary/metrics?window=1h: every function's
// invocations, errors, throttles and duration percentiles over the trailing
// window (a Go duration, one hour by default), in total and per Resource
// dimension value. It answers 501 when the router has no recorder.
func (rt *Router) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if rt.metrics == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"message": "this server does not record metrics",
		})
		return
	}

	window := defaultMetricsWindow
	if raw := r.URL.Query().Get("window"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"message": fmt.Sprintf("invalid window %q: want a positive Go duration such as 15m or 1h", raw),
			})
			return
		}

		window = d
	}

	functions := rt.metrics.Snapshot(window)
	if functions == nil {
		functions = []metrics.FunctionSnapshot{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"window":    window.String(),
		"functions": functions,
	})
}

// handleHealth serves GET /_lambdary/health: 200 with every discovered
// function and whether it currently has a running instance ("running") or
// will start one on its next invoke ("stopped"). It needs no recorder.
func (rt *Router) handleHealth(w http.ResponseWriter) {
	running := rt.mgr.Running()

	type functionHealth struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}

	functions := make([]functionHealth, 0, len(rt.names))
	for _, name := range rt.names {
		state := "stopped"
		if running[name] {
			state = "running"
		}

		functions = append(functions, functionHealth{Name: name, State: state})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"functions": functions,
	})
}
