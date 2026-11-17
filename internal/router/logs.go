package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-southeners/lambdary/internal/versions"
)

// logsPath serves the function log stream.
const logsPath = "/_lambdary/logs"

// defaultLogTail is how many buffered lines GET /_lambdary/logs replays when
// the request names no tail.
const defaultLogTail = 100

// logMatcher builds the predicate selecting the log lines of ref, a function
// name optionally qualified as name:version or name:alias. A bare name
// matches every instance of the function, $LATEST and published versions
// alike; a version matches that version's instance; an alias matches the
// versions it routes to now (its version and any canary version). The
// instance names it accepts are fixed when it is built. Its error wraps
// versions.ErrNotFound for an unknown function, version or alias.
func (rt *Router) logMatcher(ref string) (func(instance string) bool, error) {
	name, qualifier, _ := strings.Cut(ref, ":")

	if _, ok := rt.byName[name]; !ok {
		return nil, fmt.Errorf("%w: function not found: %s", versions.ErrNotFound, name)
	}

	if qualifier == "" {
		return func(instance string) bool {
			return instance == name || strings.HasPrefix(instance, name+":")
		}, nil
	}

	targets, err := rt.logQualifierVersions(name, qualifier)
	if err != nil {
		return nil, err
	}

	instances := make(map[string]bool, len(targets)+1)

	for _, v := range targets {
		if v == versions.Latest {
			// $LATEST's instance is named by the plain function name.
			instances[name] = true
		}

		instances[name+":"+v] = true
	}

	return func(instance string) bool { return instances[instance] }, nil
}

// logQualifierVersions returns the versions of name that qualifier (a version
// number, $LATEST or an alias) stands for, or an error wrapping
// versions.ErrNotFound when it names none.
func (rt *Router) logQualifierVersions(name, qualifier string) ([]string, error) {
	if qualifier == versions.Latest {
		return []string{versions.Latest}, nil
	}

	if rt.versions == nil {
		return nil, fmt.Errorf("%w: version or alias not found: %s:%s", versions.ErrNotFound, name, qualifier)
	}

	alias, err := rt.versions.GetAlias(name, qualifier)
	if err == nil {
		out := []string{alias.Version}
		if alias.CanaryVersion != "" {
			out = append(out, alias.CanaryVersion)
		}

		return out, nil
	}

	if !errors.Is(err, versions.ErrNotFound) {
		return nil, err
	}

	// Not an alias: a published version number, or unknown.
	version, err := rt.versions.Resolve(name, qualifier)
	if err != nil {
		return nil, err
	}

	return []string{version}, nil
}

// handleLogs serves GET /_lambdary/logs?function=<name>&tail=<n>&follow=<0|1>
// as NDJSON, one logbus.Line per line: the last tail buffered lines (100 by
// default) of one function or, without function, of all of them. function is
// a name, optionally qualified as name:version or name:alias; see logMatcher
// for which instances each form selects. Without follow it returns after
// those lines; with follow it keeps streaming new lines until the client
// disconnects or the bus closes. It answers 501 when the router has no log
// bus and 404 for an unknown function, version or alias.
func (rt *Router) handleLogs(w http.ResponseWriter, r *http.Request) {
	if rt.logs == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"message": "this server does not collect logs",
		})
		return
	}

	query := r.URL.Query()

	var match func(instance string) bool

	if function := query.Get("function"); function != "" {
		var err error

		match, err = rt.logMatcher(function)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, versions.ErrNotFound) {
				status = http.StatusNotFound
			}

			body := map[string]any{"message": err.Error()}

			// Known function names only help when the name itself is wrong;
			// for a bad version or alias of a real function they mislead.
			if name, _ := parseFunctionRef(function); len(rt.byName[name]) == 0 {
				body["functions"] = rt.names
			}

			writeJSON(w, status, body)

			return
		}
	}

	tail := defaultLogTail
	if raw := query.Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"message": fmt.Sprintf("invalid tail %q: want a non-negative integer", raw),
			})
			return
		}

		tail = n
	}

	follow := false
	if raw := query.Get("follow"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"message": fmt.Sprintf("invalid follow %q: want 0 or 1", raw),
			})
			return
		}

		follow = v
	}

	lines, cancel := rt.logs.Subscribe(match, tail)
	defer cancel()

	// What is buffered right now is the replayed history; a request that does
	// not follow ends once it has been written.
	remaining := len(lines)

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Flush the headers now so a follower sees the stream open even before
	// any line arrives.
	flush()

	enc := json.NewEncoder(w)

	for follow || remaining > 0 {
		select {
		case line, ok := <-lines:
			if !ok {
				return
			}

			remaining--

			if err := enc.Encode(line); err != nil {
				return
			}

			flush()
		case <-r.Context().Done():
			return
		}
	}
}
