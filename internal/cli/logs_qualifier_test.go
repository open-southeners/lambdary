package cli

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestRunLogsQualifiedFunction(t *testing.T) {
	var gotQuery string

	port := logsServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery

		_, _ = w.Write([]byte(`{"function":"a:1","time":"2026-01-01T00:00:00Z","text":"hello"}` + "\n"))
	})

	var out bytes.Buffer
	if err := runLogs(context.Background(), &out, port, "a:live", false, 5); err != nil {
		t.Fatal(err)
	}

	if want := "[a:1] hello\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}

	if gotQuery != "function=a%3Alive&tail=5" {
		t.Errorf("query = %q", gotQuery)
	}
}
