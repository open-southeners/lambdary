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
)

// invokePrefix and invokeSuffix bracket the function name in the
// AWS-compatible passthrough path DESIGN.md's "Invoke passthrough" section
// specifies: `POST /2015-03-31/functions/{name}/invocations`.
const (
	invokePrefix = "/2015-03-31/functions/"
	invokeSuffix = "/invocations"
)

// routeEventMessage is returned for a request that hits a function's route
// but isn't the invoke passthrough — HTTP↔event mapping is M2, not
// implemented here (see plans/m1-container-path.md).
const routeEventMessage = "HTTP event mapping lands in M2 — invoke via POST /2015-03-31/functions/%s/invocations"

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
// invoke passthrough, route-collision/name-collision fail-safes, the M2
// placeholder for function routes, and a small discovery index at GET /.
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
		rt.handleRoute(w, matched)
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

	timeout := rt.mgr.InvokeTimeout(name)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	invokeURL, err := rt.mgr.Ensure(ctx, name)
	if err != nil {
		writeUpstreamError(w, ctx, timeout, err)
		return
	}

	var (
		status      int
		respBody    []byte
		contentType string
		invokeErr   error
	)

	// WithLock's fn always returns nil here; the invoke outcome is
	// reported via the closed-over variables instead so a lock error
	// (there is none, today) and an invoke error stay distinguishable.
	_ = rt.mgr.WithLock(name, func() error {
		status, respBody, contentType, invokeErr = doInvoke(ctx, invokeURL, body)
		return nil
	})

	if invokeErr != nil {
		writeUpstreamError(w, ctx, timeout, invokeErr)
		return
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	w.Write(respBody) //nolint:errcheck // best-effort; client disconnect leaves nothing to do about a write error here.
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

// writeUpstreamError reports err as 504 when it stems from ctx's deadline
// (Ensure's start, or the invoke itself, ran out of time) and 502 for every
// other backend/network failure.
func writeUpstreamError(w http.ResponseWriter, ctx context.Context, timeout time.Duration, err error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"message": fmt.Sprintf("invoke timed out after %s: %s", timeout, err),
		})
		return
	}

	writeJSON(w, http.StatusBadGateway, map[string]any{
		"message": err.Error(),
	})
}

// handleRoute serves a request matched to one or more functions' Route by
// matchRoute. More than one match means those functions collide on that
// route (409, naming competitors); exactly one means the route is valid
// but HTTP↔event mapping isn't implemented yet (501).
func (rt *Router) handleRoute(w http.ResponseWriter, matched []discovery.Function) {
	if len(matched) > 1 {
		writeCollision(w, "route", matched[0].Route, matched)
		return
	}

	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"message": fmt.Sprintf(routeEventMessage, matched[0].Name),
	})
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
