package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// chattyMain is a function that prints one line to stdout per invocation and
// answers it, so the logs test has real function output to read back.
const chattyMain = `package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	base := "http://" + os.Getenv("AWS_LAMBDA_RUNTIME_API") + "/2018-06-01/runtime/invocation/"

	for {
		next, err := http.Get(base + "next")
		if err != nil {
			os.Exit(1)
		}
		io.Copy(io.Discard, next.Body)
		next.Body.Close()

		fmt.Println("chatty says hello")

		done, err := http.Post(base+next.Header.Get("Lambda-Runtime-Aws-Request-Id")+"/response", "application/json",
			bytes.NewReader([]byte(` + "`" + `{"ok":true}` + "`" + `)))
		if err != nil {
			os.Exit(1)
		}
		done.Body.Close()
	}
}
`

// TestE2ELogs proves a function's stdout reaches GET /_lambdary/logs and
// `lambdary logs` against a real dev server.
func TestE2ELogs(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test skipped: -short")
	}

	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("e2e test skipped: go not on PATH: %v", err)
	}

	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("testdata/go-module")); err != nil {
		t.Fatalf("copying fixture: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(root, "cmd", "chatty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cmd", "chatty", "main.go"), []byte(chattyMain), 0o644); err != nil {
		t.Fatal(err)
	}

	config := `functions:
  chatty:
    go: ./cmd/chatty
`
	if err := os.WriteFile(filepath.Join(root, "lambdary.yml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := startVersionsDev(t, root, "process")

	resp, err := http.Post(srv.url+"/2015-03-31/functions/chatty/invocations", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("invoke status = %d", resp.StatusCode)
	}

	// The function's output is pumped asynchronously, so poll for it.
	deadline := time.Now().Add(30 * time.Second)

	var body string

	for time.Now().Before(deadline) {
		r, err := http.Get(srv.url + "/_lambdary/logs?function=chatty")
		if err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		r.Body.Close()

		body = buf.String()
		if strings.Contains(body, "chatty says hello") {
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	if !strings.Contains(body, `"function":"chatty"`) || !strings.Contains(body, `"text":"chatty says hello"`) {
		t.Fatalf("logs = %q, want the function's line", body)
	}

	u, err := url.Parse(srv.url)
	if err != nil {
		t.Fatal(err)
	}

	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runLogs(context.Background(), &out, port, "chatty", false, 10); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "[chatty] chatty says hello") {
		t.Errorf("lambdary logs output = %q", out.String())
	}
}
