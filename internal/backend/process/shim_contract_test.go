package process

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file drives the embedded shims (shims/bootstrap.mjs,
// shims/bootstrap.py, shims/bootstrap.rb) as real child processes against a
// stub Runtime API server, per plans/m3-process-path.md Unit B's shim test
// requirements (Ruby added by plans/process-ruby-and-container-fallback.md
// Unit A). No RIE is involved — the shims only ever talk to
// $AWS_LAMBDA_RUNTIME_API, which the real RIE normally sets; here the stub
// server plays that role directly. Full RIE integration is Unit C's e2e
// territory.

// capturedRequest is one POST the shim under test made to the stub's
// response/error/init-error endpoints.
type capturedRequest struct {
	kind string // "response", "error", or "init/error"
	body []byte
}

// newRuntimeAPIStub starts an httptest server implementing just enough of
// the Runtime API for one shim run: GET next always returns event under
// requestID, and the response/error/init-error POSTs are captured onto the
// returned channel (buffered, so the shim looping back for a second
// "next" after the test has stopped watching doesn't block it).
func newRuntimeAPIStub(t *testing.T, event []byte, requestID string) (addr string, captures <-chan capturedRequest) {
	t.Helper()

	ch := make(chan capturedRequest, 8)
	capture := func(kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body) //nolint:errcheck // best-effort read for a test double; a failure just yields an empty capture.
			ch <- capturedRequest{kind: kind, body: body}
			w.WriteHeader(http.StatusAccepted)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/2018-06-01/runtime/invocation/next", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Lambda-Runtime-Aws-Request-Id", requestID)
		w.Header().Set("Lambda-Runtime-Deadline-Ms", fmt.Sprintf("%d", time.Now().Add(30*time.Second).UnixMilli()))
		w.Write(event) //nolint:errcheck // writing a canned fixture body to a test ResponseWriter.
	})
	mux.HandleFunc("/2018-06-01/runtime/invocation/"+requestID+"/response", capture("response"))
	mux.HandleFunc("/2018-06-01/runtime/invocation/"+requestID+"/error", capture("error"))
	mux.HandleFunc("/2018-06-01/runtime/init/error", capture("init/error"))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "http://"), ch
}

// newDelayedRuntimeAPIStub is newRuntimeAPIStub, except the next endpoint
// sleeps for delay before writing its response — standing in for the real
// Runtime API's long poll blocking on an idle dev server, per
// TestNodeShim's "survives a delayed next() response" case.
func newDelayedRuntimeAPIStub(t *testing.T, event []byte, requestID string, delay time.Duration) (addr string, captures <-chan capturedRequest) {
	t.Helper()

	ch := make(chan capturedRequest, 8)
	capture := func(kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body) //nolint:errcheck // best-effort read for a test double; a failure just yields an empty capture.
			ch <- capturedRequest{kind: kind, body: body}
			w.WriteHeader(http.StatusAccepted)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/2018-06-01/runtime/invocation/next", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Lambda-Runtime-Aws-Request-Id", requestID)
		w.Header().Set("Lambda-Runtime-Deadline-Ms", fmt.Sprintf("%d", time.Now().Add(30*time.Second).UnixMilli()))
		w.Write(event) //nolint:errcheck // writing a canned fixture body to a test ResponseWriter.
	})
	mux.HandleFunc("/2018-06-01/runtime/invocation/"+requestID+"/response", capture("response"))
	mux.HandleFunc("/2018-06-01/runtime/invocation/"+requestID+"/error", capture("error"))
	mux.HandleFunc("/2018-06-01/runtime/init/error", capture("init/error"))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "http://"), ch
}

// shimProcess is a shim driven as a real child process by startShim.
type shimProcess struct {
	out  *bytes.Buffer
	done chan struct{}
	err  error
}

// startShim launches interpreter (node or python3) with args in dir against
// the stub Runtime API at runtimeAPI ("host:port", no scheme — matching
// what the real RIE sets AWS_LAMBDA_RUNTIME_API to). It is killed (and
// reaped) at test cleanup regardless of whether the test body waits for it
// to exit on its own.
func startShim(t *testing.T, interpreter string, args []string, dir, runtimeAPI string) *shimProcess {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	cmd := exec.CommandContext(ctx, interpreter, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"AWS_LAMBDA_RUNTIME_API="+runtimeAPI,
		"AWS_LAMBDA_FUNCTION_NAME=test-fn",
	)

	sp := &shimProcess{out: &bytes.Buffer{}, done: make(chan struct{})}
	cmd.Stdout = sp.out
	cmd.Stderr = sp.out

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("starting %s %v: %v", interpreter, args, err)
	}

	go func() {
		sp.err = cmd.Wait()
		close(sp.done)
	}()

	t.Cleanup(func() {
		cancel()
		<-sp.done

		if t.Failed() {
			t.Logf("%s %v combined output:\n%s", interpreter, args, sp.out.String())
		}
	})

	return sp
}

// waitCapture waits up to 5s for a capture on ch, failing the test on
// timeout.
func waitCapture(t *testing.T, ch <-chan capturedRequest) capturedRequest {
	t.Helper()

	select {
	case c := <-ch:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the shim to call the Runtime API")
		return capturedRequest{}
	}
}

// waitExit waits up to 5s for sp to exit on its own (as opposed to being
// killed by test cleanup), failing the test on timeout.
func waitExit(t *testing.T, sp *shimProcess) error {
	t.Helper()

	select {
	case <-sp.done:
		return sp.err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the shim to exit")
		return nil
	}
}

func nodeShimPath(t *testing.T) string {
	t.Helper()

	abs, err := filepath.Abs(filepath.Join("shims", nodeShimName))
	if err != nil {
		t.Fatalf("resolving node shim path: %v", err)
	}

	return abs
}

func pythonShimPath(t *testing.T) string {
	t.Helper()

	abs, err := filepath.Abs(filepath.Join("shims", pythonShimName))
	if err != nil {
		t.Fatalf("resolving python shim path: %v", err)
	}

	return abs
}

func rubyShimPath(t *testing.T) string {
	t.Helper()

	abs, err := filepath.Abs(filepath.Join("shims", rubyShimName))
	if err != nil {
		t.Fatalf("resolving ruby shim path: %v", err)
	}

	return abs
}

func TestNodeShim(t *testing.T) {
	node := requireBin(t, "node")
	shim := nodeShimPath(t)

	t.Run("happy path: next -> handler -> response", func(t *testing.T) {
		addr, captures := newRuntimeAPIStub(t, []byte(`{"ping":"pong"}`), "req-1")
		startShim(t, node, []string{shim, "handler.handler"}, "testdata/node", addr)

		c := waitCapture(t, captures)
		if c.kind != "response" {
			t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "response", c.body)
		}

		var got struct {
			Echoed       map[string]any `json:"echoed"`
			RequestID    string         `json:"requestId"`
			FunctionName string         `json:"functionName"`
		}
		if err := json.Unmarshal(c.body, &got); err != nil {
			t.Fatalf("unmarshalling response body %q: %v", c.body, err)
		}

		if got.Echoed["ping"] != "pong" {
			t.Errorf("response echoed = %v, want the invocation event echoed back", got.Echoed)
		}
		if got.RequestID != "req-1" {
			t.Errorf("response requestId = %q, want %q", got.RequestID, "req-1")
		}
		if got.FunctionName != "test-fn" {
			t.Errorf("response functionName = %q, want %q (from AWS_LAMBDA_FUNCTION_NAME)", got.FunctionName, "test-fn")
		}
	})

	t.Run("handler exception posts to the error endpoint with an errorType", func(t *testing.T) {
		addr, captures := newRuntimeAPIStub(t, []byte(`{}`), "req-2")
		startShim(t, node, []string{shim, "handler.throwing"}, "testdata/node", addr)

		c := waitCapture(t, captures)
		if c.kind != "error" {
			t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "error", c.body)
		}

		var payload struct {
			ErrorMessage string   `json:"errorMessage"`
			ErrorType    string   `json:"errorType"`
			StackTrace   []string `json:"stackTrace"`
		}
		if err := json.Unmarshal(c.body, &payload); err != nil {
			t.Fatalf("unmarshalling error body %q: %v", c.body, err)
		}

		if payload.ErrorType != "Error" {
			t.Errorf("errorType = %q, want %q", payload.ErrorType, "Error")
		}
		if payload.ErrorMessage != "boom" {
			t.Errorf("errorMessage = %q, want %q", payload.ErrorMessage, "boom")
		}
		if len(payload.StackTrace) == 0 {
			t.Error("stackTrace is empty, want at least one frame")
		}
	})

	t.Run("bad handler spec posts to init/error and exits 1", func(t *testing.T) {
		addr, captures := newRuntimeAPIStub(t, []byte(`{}`), "req-3")
		sp := startShim(t, node, []string{shim, "nonexistent.handler"}, "testdata/node", addr)

		c := waitCapture(t, captures)
		if c.kind != "init/error" {
			t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "init/error", c.body)
		}

		assertExitCode(t, waitExit(t, sp), 1)
	})

	// Regression test for a real bug: the shim used to fetch() the
	// long-poll GET .../invocation/next, and fetch's undici Agent applies
	// a 300s headersTimeout by default — the shim would crash with an
	// uncaught HeadersTimeoutError, killing every later invocation, after
	// 5 idle minutes between requests (routine for a local dev server).
	// This can't wait out the real 300s default in a unit test, but it
	// does prove the shim itself imposes no timeout of its own on a
	// delayed next() response — node:http's client, unlike fetch, has
	// none. See bootstrap.mjs's nextInvocation doc comment.
	t.Run("survives a delayed next() response", func(t *testing.T) {
		const delay = 3 * time.Second

		addr, captures := newDelayedRuntimeAPIStub(t, []byte(`{"ping":"pong"}`), "req-delayed", delay)
		startShim(t, node, []string{shim, "handler.handler"}, "testdata/node", addr)

		select {
		case c := <-captures:
			if c.kind != "response" {
				t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "response", c.body)
			}
		case <-time.After(delay + 5*time.Second):
			t.Fatal("timed out waiting for the shim to respond after a delayed next()")
		}
	})
}

func TestPythonShim(t *testing.T) {
	python3 := requireBin(t, "python3")
	shim := pythonShimPath(t)

	t.Run("happy path: next -> handler -> response", func(t *testing.T) {
		addr, captures := newRuntimeAPIStub(t, []byte(`{"ping":"pong"}`), "req-1")
		startShim(t, python3, []string{shim, "handler.handler"}, "testdata/python", addr)

		c := waitCapture(t, captures)
		if c.kind != "response" {
			t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "response", c.body)
		}

		var got struct {
			Echoed       map[string]any `json:"echoed"`
			RequestID    string         `json:"requestId"`
			FunctionName string         `json:"functionName"`
		}
		if err := json.Unmarshal(c.body, &got); err != nil {
			t.Fatalf("unmarshalling response body %q: %v", c.body, err)
		}

		if got.Echoed["ping"] != "pong" {
			t.Errorf("response echoed = %v, want the invocation event echoed back", got.Echoed)
		}
		if got.RequestID != "req-1" {
			t.Errorf("response requestId = %q, want %q", got.RequestID, "req-1")
		}
		if got.FunctionName != "test-fn" {
			t.Errorf("response functionName = %q, want %q (from AWS_LAMBDA_FUNCTION_NAME)", got.FunctionName, "test-fn")
		}
	})

	t.Run("handler exception posts to the error endpoint with an errorType", func(t *testing.T) {
		addr, captures := newRuntimeAPIStub(t, []byte(`{}`), "req-2")
		startShim(t, python3, []string{shim, "handler.throwing"}, "testdata/python", addr)

		c := waitCapture(t, captures)
		if c.kind != "error" {
			t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "error", c.body)
		}

		var payload struct {
			ErrorMessage string   `json:"errorMessage"`
			ErrorType    string   `json:"errorType"`
			StackTrace   []string `json:"stackTrace"`
		}
		if err := json.Unmarshal(c.body, &payload); err != nil {
			t.Fatalf("unmarshalling error body %q: %v", c.body, err)
		}

		if payload.ErrorType != "ValueError" {
			t.Errorf("errorType = %q, want %q", payload.ErrorType, "ValueError")
		}
		if payload.ErrorMessage != "boom" {
			t.Errorf("errorMessage = %q, want %q", payload.ErrorMessage, "boom")
		}
		if len(payload.StackTrace) == 0 {
			t.Error("stackTrace is empty, want at least one frame")
		}
	})

	t.Run("bad handler spec posts to init/error and exits 1", func(t *testing.T) {
		addr, captures := newRuntimeAPIStub(t, []byte(`{}`), "req-3")
		sp := startShim(t, python3, []string{shim, "nonexistent.handler"}, "testdata/python", addr)

		c := waitCapture(t, captures)
		if c.kind != "init/error" {
			t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "init/error", c.body)
		}

		assertExitCode(t, waitExit(t, sp), 1)
	})
}

func TestRubyShim(t *testing.T) {
	ruby := requireBin(t, "ruby")
	shim := rubyShimPath(t)

	t.Run("happy path: next -> handler -> response", func(t *testing.T) {
		addr, captures := newRuntimeAPIStub(t, []byte(`{"ping":"pong"}`), "req-1")
		startShim(t, ruby, []string{shim, "handler.handler"}, "testdata/ruby", addr)

		c := waitCapture(t, captures)
		if c.kind != "response" {
			t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "response", c.body)
		}

		var got struct {
			Echoed       map[string]any `json:"echoed"`
			RequestID    string         `json:"requestId"`
			FunctionName string         `json:"functionName"`
		}
		if err := json.Unmarshal(c.body, &got); err != nil {
			t.Fatalf("unmarshalling response body %q: %v", c.body, err)
		}

		if got.Echoed["ping"] != "pong" {
			t.Errorf("response echoed = %v, want the invocation event echoed back", got.Echoed)
		}
		if got.RequestID != "req-1" {
			t.Errorf("response requestId = %q, want %q", got.RequestID, "req-1")
		}
		if got.FunctionName != "test-fn" {
			t.Errorf("response functionName = %q, want %q (from AWS_LAMBDA_FUNCTION_NAME)", got.FunctionName, "test-fn")
		}
	})

	t.Run("handler exception posts to the error endpoint with an errorType", func(t *testing.T) {
		addr, captures := newRuntimeAPIStub(t, []byte(`{}`), "req-2")
		startShim(t, ruby, []string{shim, "handler.throwing"}, "testdata/ruby", addr)

		c := waitCapture(t, captures)
		if c.kind != "error" {
			t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "error", c.body)
		}

		var payload struct {
			ErrorMessage string   `json:"errorMessage"`
			ErrorType    string   `json:"errorType"`
			StackTrace   []string `json:"stackTrace"`
		}
		if err := json.Unmarshal(c.body, &payload); err != nil {
			t.Fatalf("unmarshalling error body %q: %v", c.body, err)
		}

		if payload.ErrorType != "ArgumentError" {
			t.Errorf("errorType = %q, want %q", payload.ErrorType, "ArgumentError")
		}
		if payload.ErrorMessage != "boom" {
			t.Errorf("errorMessage = %q, want %q", payload.ErrorMessage, "boom")
		}
		if len(payload.StackTrace) == 0 {
			t.Error("stackTrace is empty, want at least one frame")
		}
	})

	t.Run("bad handler spec posts to init/error and exits 1", func(t *testing.T) {
		addr, captures := newRuntimeAPIStub(t, []byte(`{}`), "req-3")
		sp := startShim(t, ruby, []string{shim, "nonexistent.handler"}, "testdata/ruby", addr)

		c := waitCapture(t, captures)
		if c.kind != "init/error" {
			t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "init/error", c.body)
		}

		assertExitCode(t, waitExit(t, sp), 1)
	})

	// Regression test for the same class of bug TestNodeShim's delayed-next
	// case guards against: bootstrap.rb must disable Net::HTTP's default
	// 60s read_timeout on the long-polling GET .../invocation/next, or an
	// idle dev server between invocations would crash the shim with a
	// Net::ReadTimeout. See bootstrap.rb's next_invocation doc comment.
	t.Run("survives a delayed next() response", func(t *testing.T) {
		const delay = 3 * time.Second

		addr, captures := newDelayedRuntimeAPIStub(t, []byte(`{"ping":"pong"}`), "req-delayed", delay)
		startShim(t, ruby, []string{shim, "handler.handler"}, "testdata/ruby", addr)

		select {
		case c := <-captures:
			if c.kind != "response" {
				t.Fatalf("captured request kind = %q, want %q (body: %s)", c.kind, "response", c.body)
			}
		case <-time.After(delay + 5*time.Second):
			t.Fatal("timed out waiting for the shim to respond after a delayed next()")
		}
	})
}

// assertExitCode fails the test unless err is an *exec.ExitError with the
// given code.
func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("process exit error = %v, want an *exec.ExitError", err)
	}
	if exitErr.ExitCode() != want {
		t.Errorf("process exit code = %d, want %d", exitErr.ExitCode(), want)
	}
}
