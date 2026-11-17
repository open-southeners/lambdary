package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/logbus"
)

// logsServer is a stand-in dev server: it identifies itself on GET / and
// serves /_lambdary/logs from bus through the real router handler's contract.
func logsServer(t *testing.T, handler http.HandlerFunc) int {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"server":"lambdary"}`))
	})
	mux.HandleFunc("/_lambdary/logs", handler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	return port
}

func TestRunLogsPrintsTail(t *testing.T) {
	var gotQuery string

	port := logsServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"function":"a","time":"2026-01-01T00:00:00Z","text":"hello"}` + "\n" +
			`{"function":"b","time":"2026-01-01T00:00:01Z","text":"world"}` + "\n"))
	})

	var out bytes.Buffer
	if err := runLogs(context.Background(), &out, port, "a", false, 5); err != nil {
		t.Fatal(err)
	}

	if want := "[a] hello\n[b] world\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}

	if gotQuery != "function=a&tail=5" {
		t.Errorf("query = %q", gotQuery)
	}
}

func TestRunLogsFollowStopsOnCancel(t *testing.T) {
	started := make(chan struct{})

	port := logsServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("follow") != "1" {
			t.Errorf("follow not requested: %q", r.URL.RawQuery)
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"function":"a","time":"2026-01-01T00:00:00Z","text":"first"}` + "\n"))
		w.(http.Flusher).Flush()
		close(started)

		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu  sync.Mutex
		out bytes.Buffer
	)

	done := make(chan error, 1)

	go func() {
		done <- runLogs(ctx, &lockedWriter{mu: &mu, w: &out}, port, "", true, 10)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the follow request")
	}

	// Give the client a moment to print the line, then interrupt it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		printed := strings.Contains(out.String(), "[a] first")
		mu.Unlock()

		if printed {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLogs after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runLogs did not return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()

	if !strings.Contains(out.String(), "[a] first") {
		t.Errorf("output = %q", out.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.w.Write(p)
}

func TestRunLogsNoServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	err = runLogs(context.Background(), &bytes.Buffer{}, port, "", false, 1)
	if err == nil || !strings.Contains(err.Error(), "no lambdary dev server on port") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunLogsServerError(t *testing.T) {
	port := logsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"function not found: x","functions":["a","b"]}`))
	})

	err := runLogs(context.Background(), &bytes.Buffer{}, port, "x", false, 1)
	if err == nil || !strings.Contains(err.Error(), "function not found: x") || !strings.Contains(err.Error(), "a b") {
		t.Fatalf("err = %v", err)
	}
}

func TestLogStreamerPublishesToBus(t *testing.T) {
	var buf bytes.Buffer

	s := newTestLogStreamer(&buf, false)
	s.bus = logbus.New()

	s.stream("hello", strings.NewReader("one\ntwo\n"))

	ch, cancel := s.bus.Subscribe(func(n string) bool { return n == "hello" }, 10)
	defer cancel()

	if l := <-ch; l.Text != "one" {
		t.Fatalf("first = %v", l)
	}

	if l := <-ch; l.Text != "two" {
		t.Fatalf("second = %v", l)
	}

	if buf.String() != "[hello] one\n[hello] two\n" {
		t.Errorf("sink = %q", buf.String())
	}
}
