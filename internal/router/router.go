package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/event"
)

// invokePrefix and invokeSuffix bracket the function name in the
// AWS-compatible passthrough path DESIGN.md's "Invoke passthrough" section
// specifies: `POST /2015-03-31/functions/{name}/invocations`.
const (
	invokePrefix = "/2015-03-31/functions/"
	invokeSuffix = "/invocations"
)

// payloadV2 and payloadV1 are the two Function URL / API Gateway event
// payload formats handleRoute maps to an event type: payloadV2 (the
// default — Lambda Function URL / HTTP API "2.0") via event.FromHTTP/
// event.ToHTTP, and payloadV1 (API Gateway REST API "1.0") via
// event.FromHTTPV1/event.ToHTTPV1. A manifest that explicitly asks for
// anything else gets a 501 naming both supported values.
const (
	payloadV2 = "2.0"
	payloadV1 = "1.0"
)

// unsupportedPayloadMessage is the 501 body for a function route whose
// manifest requests a payload format the router doesn't understand.
const unsupportedPayloadMessage = `payload format %s is not supported — supported values are "2.0" and "1.0"`

// Router is the http.Handler router.New returns. It holds no global state:
// every dependency (the Manager, the discovered functions) is injected by
// New.
type Router struct {
	mgr *Manager
	fns []discovery.Function

	// byName groups every discovered function by Name, including groups of
	// size 1. A group with more than one entry is a duplicate-name
	// collision, computed here rather than trusted from discovery's
	// Warnings strings (see New's doc comment for why).
	byName map[string][]discovery.Function
	// names lists every distinct function name, sorted, for 404 "known
	// functions" listings and the GET / index.
	names []string
}

// New builds the http.Handler serving m's functions fns: the AWS-compatible
// invoke passthrough, HTTP↔event mapping for each function's own route,
// route-collision/name-collision fail-safes, and a small discovery index at
// GET /.
//
// Collisions (duplicate names, duplicate routes) are computed directly from
// fns by grouping on Name and on Route, rather than by string-matching
// discovery's Warnings messages: Warnings is free-text meant for humans
// (see internal/discovery's warnCollisions), and parsing it back into
// structured "who's colliding" data would be brittle for no benefit — New
// already has the full []discovery.Function it needs to compute the same
// groups directly and cheaply.
func New(m *Manager, fns []discovery.Function) http.Handler {
	byName := make(map[string][]discovery.Function, len(fns))
	for _, fn := range fns {
		byName[fn.Name] = append(byName[fn.Name], fn)
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	return &Router{mgr: m, fns: fns, byName: byName, names: names}
}

// ServeHTTP implements http.Handler.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if name, ok := invokeName(r.Method, path); ok {
		rt.handleInvoke(w, r, name)
		return
	}

	if matched := matchRoute(rt.fns, path); len(matched) > 0 {
		rt.handleRoute(w, r, matched)
		return
	}

	if r.Method == http.MethodGet && path == "/" {
		rt.handleIndex(w)
		return
	}

	writeJSON(w, http.StatusNotFound, map[string]any{
		"message": fmt.Sprintf("not found: %s %s", r.Method, path),
	})
}

// handleInvoke implements the AWS-compatible invoke passthrough: resolve
// name to a running instance (starting it on first use), hold the
// function's invoke lock for the duration of one request, and forward the
// body verbatim, streaming the upstream status/body/Content-Type back.
func (rt *Router) handleInvoke(w http.ResponseWriter, r *http.Request, name string) {
	if fns := rt.byName[name]; len(fns) > 1 {
		dirs := make([]string, len(fns))
		for i, fn := range fns {
			dirs[i] = fn.Dir
		}

		writeJSON(w, http.StatusConflict, map[string]any{
			"message":   fmt.Sprintf("function name %q is used by multiple functions: %s", name, strings.Join(dirs, ", ")),
			"functions": dirs,
		})
		return
	}

	if _, ok := rt.byName[name]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"message":   fmt.Sprintf("function not found: %s", name),
			"functions": rt.names,
		})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"message": fmt.Sprintf("reading request body: %s", err),
		})
		return
	}

	status, respBody, contentType, err := rt.invoke(r, name, body)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	w.Write(respBody) //nolint:errcheck // best-effort; client disconnect leaves nothing to do about a write error here.
}

// invoke is the ensure/lock/POST/timeout plumbing shared by both ways a
// request reaches a function's backend: the raw invoke passthrough
// (handleInvoke, body forwarded verbatim) and a function-route hit
// (handleRoute, body is a marshaled event.RequestV2). It resolves name to a
// running instance (starting it on first use via Manager.Ensure), holds the
// function's invoke lock for the duration of the request, and POSTs body to
// the instance within name's InvokeTimeout. A non-nil error is always an
// *invokeError, already tagging whether it stemmed from the deadline, so
// writeUpstreamError can pick 502 vs 504 without needing ctx or timeout in
// hand.
func (rt *Router) invoke(r *http.Request, name string, body []byte) (status int, respBody []byte, contentType string, err error) {
	timeout := rt.mgr.InvokeTimeout(name)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	invokeURL, err := rt.mgr.Ensure(ctx, name)
	if err != nil {
		return 0, nil, "", newInvokeError(ctx, timeout, err)
	}

	var invokeErr error

	// WithLock's fn always returns nil here; the invoke outcome is
	// reported via the closed-over variables instead so a lock error
	// (there is none, today) and an invoke error stay distinguishable.
	_ = rt.mgr.WithLock(name, func() error {
		status, respBody, contentType, invokeErr = doInvoke(ctx, invokeURL, body)
		return nil
	})

	if invokeErr != nil {
		return 0, nil, "", newInvokeError(ctx, timeout, invokeErr)
	}

	return status, respBody, contentType, nil
}

// doInvoke POSTs body to invokeURL and returns the upstream response,
// verbatim, for handleInvoke to relay.
func doInvoke(ctx context.Context, invokeURL string, body []byte) (status int, respBody []byte, contentType string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, invokeURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, "", fmt.Errorf("router: building invoke request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, "", fmt.Errorf("router: invoking: %w", err)
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", fmt.Errorf("router: reading invoke response: %w", err)
	}

	return resp.StatusCode, respBody, resp.Header.Get("Content-Type"), nil
}

// invokeError wraps a backend/network failure from (*Router).invoke,
// tagging whether it stemmed from the invoke's own deadline (Ensure's
// start, or the invoke itself, ran out of time) so writeUpstreamError can
// choose 504 vs 502 without needing ctx or timeout in hand.
type invokeError struct {
	timedOut bool
	timeout  time.Duration
	err      error
}

func newInvokeError(ctx context.Context, timeout time.Duration, err error) *invokeError {
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
	return &invokeError{timedOut: timedOut, timeout: timeout, err: err}
}

func (e *invokeError) Error() string { return e.err.Error() }
func (e *invokeError) Unwrap() error { return e.err }

// writeUpstreamError reports err as 504 when it was a timeout and 502 for
// every other backend/network failure — the same taxonomy for both the
// invoke passthrough and a function-route hit, since both go through
// (*Router).invoke.
func writeUpstreamError(w http.ResponseWriter, err error) {
	var ie *invokeError
	if errors.As(err, &ie) && ie.timedOut {
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"message": fmt.Sprintf("invoke timed out after %s: %s", ie.timeout, ie.err),
		})
		return
	}

	writeJSON(w, http.StatusBadGateway, map[string]any{
		"message": err.Error(),
	})
}

// handleRoute serves a request matched to one or more functions' Route by
// matchRoute. More than one match means those functions collide on that
// route (409, naming competitors). Exactly one match is a genuine
// function-route hit: dispatch to whichever payload format fn's manifest
// requests (payloadV2, the default, or payloadV1), or 501 for anything
// else, naming both supported values.
func (rt *Router) handleRoute(w http.ResponseWriter, r *http.Request, matched []discovery.Function) {
	if len(matched) > 1 {
		writeCollision(w, "route", matched[0].Route, matched)
		return
	}

	fn := matched[0]

	switch payload := payloadVersion(fn); payload {
	case "", payloadV2:
		rt.handleRouteV2(w, r, fn)
	case payloadV1:
		rt.handleRouteV1(w, r, fn)
	default:
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"message": fmt.Sprintf(unsupportedPayloadMessage, payload),
		})
	}
}

// handleRouteV2 serves fn's route under the Function URL / HTTP API "2.0"
// payload format (payloadV2): build the v2 event for r, invoke fn the same
// way the passthrough does (Ensure/lock/timeout, via (*Router).invoke),
// then translate the raw invoke result back into an HTTP response with
// event.ToHTTP.
func (rt *Router) handleRouteV2(w http.ResponseWriter, r *http.Request, fn discovery.Function) {
	ev, err := event.FromHTTP(r, fn.Route)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"message": fmt.Sprintf("reading request body: %s", err),
		})
		return
	}

	body, err := json.Marshal(ev)
	if err != nil {
		// ev is built entirely from strings/bools this package controls;
		// Marshal failing here would mean a bug in event.RequestV2 itself,
		// not bad input, so this is a 500 rather than an upstream problem.
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"message": fmt.Sprintf("encoding request event: %s", err),
		})
		return
	}

	status, respBody, _, err := rt.invoke(r, fn.Name, body)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	if isFunctionError(status, respBody) {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"message": "function error",
			"error":   errorPayload(respBody),
		})
		return
	}

	if err := event.ToHTTP(w, respBody); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"message": fmt.Sprintf("malformed function response: %s", err),
		})
	}
}

// handleRouteV1 serves fn's route under the API Gateway REST API "1.0"
// payload format (payloadV1): the same Ensure/lock/timeout/function-error
// handling as handleRouteV2, but built from event.FromHTTPV1/
// event.ToHTTPV1 instead. event.ToHTTPV1 is stricter than event.ToHTTP —
// there's no "bare JSON -> 200" fallback for an unshaped return value — so
// a malformed response is reported as 502 naming payload 1.0 specifically,
// mirroring API Gateway's own behavior for a malformed Lambda proxy
// response.
func (rt *Router) handleRouteV1(w http.ResponseWriter, r *http.Request, fn discovery.Function) {
	ev, err := event.FromHTTPV1(r, fn.Route)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"message": fmt.Sprintf("reading request body: %s", err),
		})
		return
	}

	body, err := json.Marshal(ev)
	if err != nil {
		// ev is built entirely from strings/bools this package controls;
		// Marshal failing here would mean a bug in event.RequestV1 itself,
		// not bad input, so this is a 500 rather than an upstream problem.
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"message": fmt.Sprintf("encoding request event: %s", err),
		})
		return
	}

	status, respBody, _, err := rt.invoke(r, fn.Name, body)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	if isFunctionError(status, respBody) {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"message": "function error",
			"error":   errorPayload(respBody),
		})
		return
	}

	if err := event.ToHTTPV1(w, respBody); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"message": fmt.Sprintf("malformed function response for payload 1.0: %s", err),
		})
	}
}

// payloadVersion returns fn's configured Function URL payload format
// (Manifest.URL.Payload), or "" when fn has no manifest at all — treated
// the same as an unset field, since both mean "use the default".
func payloadVersion(fn discovery.Function) string {
	if fn.Manifest == nil {
		return ""
	}

	return fn.Manifest.URL.Payload
}

// isFunctionError reports whether an invoke's raw HTTP status/body
// indicates the function handler itself failed, rather than the request
// producing a normal (shaped or unshaped) return value. The Runtime
// Interface Emulator signals a handler failure two different ways
// depending on the invoke path: a non-2xx HTTP status, or HTTP 200 with a
// JSON error envelope body and no statusCode field (see isErrorEnvelope) —
// the latter is what distinguishes a genuine handler error from an
// unshaped return value that merely happens to be a JSON object.
func isFunctionError(status int, body []byte) bool {
	if status < 200 || status > 299 {
		return true
	}

	return isErrorEnvelope(body)
}

// isErrorEnvelope reports whether body is a JSON object carrying both
// errorType and errorMessage but no statusCode — AWS Lambda's own handler
// error envelope shape, as opposed to a shaped Function URL response
// (which has statusCode) or an ordinary unshaped return value.
func isErrorEnvelope(body []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return false
	}

	if _, hasStatusCode := top["statusCode"]; hasStatusCode {
		return false
	}

	_, hasType := top["errorType"]
	_, hasMessage := top["errorMessage"]

	return hasType && hasMessage
}

// errorPayload prepares an upstream function-error body for embedding in
// the router's own 502 JSON envelope: raw JSON when body already is valid
// JSON (the common case — the error envelope itself), a plain string
// otherwise, so the 502 response stays valid JSON either way.
func errorPayload(body []byte) any {
	if json.Valid(body) {
		return json.RawMessage(body)
	}

	return string(body)
}

// handleIndex serves GET /: a minimal discovery index plus a "server"
// marker field so other Lambdary tooling (the `invoke` CLI command) can
// detect a running dev server versus an unrelated HTTP service on the same
// port.
func (rt *Router) handleIndex(w http.ResponseWriter) {
	functions := make([]map[string]string, 0, len(rt.fns))
	for _, fn := range rt.fns {
		functions = append(functions, map[string]string{
			"name":    fn.Name,
			"route":   fn.Route,
			"runtime": fn.Runtime,
			"backend": fn.Backend,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"server":    "lambdary",
		"functions": functions,
	})
}

// invokeName reports the function name targeted by a
// `POST /2015-03-31/functions/{name}/invocations` request, and whether
// method+path actually match that shape at all.
func invokeName(method, path string) (string, bool) {
	if method != http.MethodPost {
		return "", false
	}

	if !strings.HasPrefix(path, invokePrefix) || !strings.HasSuffix(path, invokeSuffix) {
		return "", false
	}

	name := strings.TrimSuffix(strings.TrimPrefix(path, invokePrefix), invokeSuffix)
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}

	return name, true
}

// matchRoute returns the functions whose Route matches path most
// specifically — an exact match, or path under Route+"/" as a prefix.
// Ties (more than one function sharing the longest matching Route) are all
// returned together, since that's exactly a route collision; callers use
// len(result) to tell a clean match (1) from a collision (>1) from no
// match (0).
func matchRoute(fns []discovery.Function, path string) []discovery.Function {
	var best []discovery.Function
	bestLen := -1

	for _, fn := range fns {
		if fn.Route == "" {
			continue
		}

		if path != fn.Route && !strings.HasPrefix(path, fn.Route+"/") {
			continue
		}

		switch {
		case len(fn.Route) > bestLen:
			bestLen = len(fn.Route)
			best = []discovery.Function{fn}
		case len(fn.Route) == bestLen:
			best = append(best, fn)
		}
	}

	return best
}

// writeCollision writes the 409 JSON DESIGN.md's route-collision decision
// calls for: naming every function competing for kind (e.g. "route" or
// "function name") value, so a user hitting the collision at runtime sees
// exactly where to fix it.
func writeCollision(w http.ResponseWriter, kind, value string, competitors []discovery.Function) {
	names := make([]string, len(competitors))
	for i, fn := range competitors {
		names[i] = fn.Name
	}

	writeJSON(w, http.StatusConflict, map[string]any{
		"message":   fmt.Sprintf("%s %q is shared by functions: %s", kind, value, strings.Join(names, ", ")),
		"functions": names,
	})
}

// writeJSON writes v as a JSON response body with status and the standard
// Content-Type header.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck // best-effort; nothing to do about a write error after headers are sent.
}
