package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// logsPath serves the function log stream.
const logsPath = "/_lambdary/logs"

// defaultLogTail is how many buffered lines GET /_lambdary/logs replays when
// the request names no tail.
const defaultLogTail = 100

// handleLogs serves GET /_lambdary/logs?function=<name>&tail=<n>&follow=<0|1>
// as NDJSON, one logbus.Line per line: the last tail buffered lines (100 by
// default) of one function or, without function, of all of them. Without
// follow it returns after those; with follow it keeps streaming new lines
// until the client disconnects or the bus closes. It answers 501 when the
// router has no log bus and 404 for an unknown function.
func (rt *Router) handleLogs(w http.ResponseWriter, r *http.Request) {
	if rt.logs == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"message": "this server does not collect logs",
		})
		return
	}

	query := r.URL.Query()

	function := query.Get("function")
	if function != "" {
		if _, ok := rt.byName[function]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"message":   fmt.Sprintf("function not found: %s", function),
				"functions": rt.names,
			})
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

	lines, cancel := rt.logs.Subscribe(function, tail)
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
