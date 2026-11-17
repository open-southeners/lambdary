package router

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/logbus"
)

func logsRouter(bus *logbus.Bus) *Router {
	fns := []discovery.Function{fn("a"), fn("b")}

	return NewServer(NewManager(newFakeBackend(), fns), fns, Options{Logs: bus})
}

func decodeLines(t *testing.T, body string) []logbus.Line {
	t.Helper()

	var lines []logbus.Line

	for _, raw := range strings.Split(strings.TrimSpace(body), "\n") {
		if raw == "" {
			continue
		}

		var l logbus.Line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("decoding %q: %v", raw, err)
		}

		lines = append(lines, l)
	}

	return lines
}

func TestLogsTail(t *testing.T) {
	bus := logbus.New()
	bus.Publish("a", "1")
	bus.Publish("b", "2")
	bus.Publish("a", "3")

	h := logsRouter(bus)

	got := serve(h, http.MethodGet, "/_lambdary/logs", "")
	if got.Code != http.StatusOK || got.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("status %d, content type %q", got.Code, got.Header().Get("Content-Type"))
	}

	if lines := decodeLines(t, got.Body.String()); len(lines) != 3 || lines[0].Text != "1" || lines[2].Text != "3" {
		t.Fatalf("all functions: %v", lines)
	}

	got = serve(h, http.MethodGet, "/_lambdary/logs?function=a&tail=1", "")
	if lines := decodeLines(t, got.Body.String()); len(lines) != 1 || lines[0].Text != "3" || lines[0].Function != "a" {
		t.Fatalf("function a tail 1: %v", lines)
	}

	got = serve(h, http.MethodGet, "/_lambdary/logs?tail=0", "")
	if got.Code != http.StatusOK || strings.TrimSpace(got.Body.String()) != "" {
		t.Fatalf("tail 0: %d %q", got.Code, got.Body.String())
	}

	if got = serve(h, http.MethodGet, "/_lambdary/logs?tail=x", ""); got.Code != http.StatusBadRequest {
		t.Fatalf("bad tail status = %d", got.Code)
	}
}

func TestLogsFollowStreamsUntilCancelled(t *testing.T) {
	bus := logbus.New()
	bus.Publish("a", "old")

	srv := httptest.NewServer(logsRouter(bus))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/_lambdary/logs?follow=1&tail=5", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)

	next := func() logbus.Line {
		t.Helper()

		if !sc.Scan() {
			t.Fatalf("stream ended: %v", sc.Err())
		}

		var l logbus.Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}

		return l
	}

	if l := next(); l.Text != "old" {
		t.Fatalf("first line = %v", l)
	}

	bus.Publish("b", "live")

	if l := next(); l.Text != "live" || l.Function != "b" {
		t.Fatalf("live line = %v", l)
	}

	cancel()

	done := make(chan struct{})

	go func() {
		for sc.Scan() {
		}

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after the client cancelled")
	}
}

func TestLogsFollowEndsWhenBusCloses(t *testing.T) {
	bus := logbus.New()
	srv := httptest.NewServer(logsRouter(bus))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_lambdary/logs?follow=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	bus.Close()

	done := make(chan struct{})

	go func() {
		_, _ = resp.Body.Read(make([]byte, 1))

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after the bus closed")
	}
}

func TestLogsUnknownFunction(t *testing.T) {
	h := logsRouter(logbus.New())

	got := serve(h, http.MethodGet, "/_lambdary/logs?function=nope", "")
	if got.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", got.Code)
	}

	var body struct {
		Functions []string `json:"functions"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil || len(body.Functions) != 2 {
		t.Fatalf("body = %q (%v)", got.Body.String(), err)
	}
}

func TestLogsWithoutBus(t *testing.T) {
	got := serve(logsRouter(nil), http.MethodGet, "/_lambdary/logs", "")
	if got.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", got.Code)
	}
}
