package container

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParsePort(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{"single IPv4 line", "127.0.0.1:54321\n", "54321", false},
		{"no trailing newline", "127.0.0.1:54321", "54321", false},
		{"IPv4 then IPv6 line, takes the first", "0.0.0.0:32768\n[::]:32768\n", "32768", false},
		{"IPv6-only line", "[::]:32768\n", "32768", false},
		{"blank lines are skipped", "\n\n127.0.0.1:9000\n", "9000", false},
		{"empty output", "", "", true},
		{"garbage output", "not a port line\n", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePort(tc.output)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePort(%q) expected error, got %q", tc.output, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("parsePort(%q) unexpected error: %v", tc.output, err)
			}
			if got != tc.want {
				t.Errorf("parsePort(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
	}
}

func TestWaitReady(t *testing.T) {
	t.Run("succeeds once the port answers HTTP requests", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()

		_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
		if err != nil {
			t.Fatalf("net.SplitHostPort() unexpected error: %v", err)
		}

		if err := waitReady(context.Background(), port, 2*time.Second); err != nil {
			t.Errorf("waitReady() unexpected error: %v", err)
		}
	})

	t.Run("a listener that accepts but never answers HTTP is not ready", func(t *testing.T) {
		// Regression test for the docker-proxy race waitReady's doc comment
		// describes: a bare TCP accept must not be mistaken for readiness.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen() unexpected error: %v", err)
		}
		defer ln.Close()

		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()

		_, port, err := net.SplitHostPort(ln.Addr().String())
		if err != nil {
			t.Fatalf("net.SplitHostPort() unexpected error: %v", err)
		}

		if err := waitReady(context.Background(), port, 200*time.Millisecond); err == nil {
			t.Error("waitReady() expected an error against a non-HTTP listener, got nil")
		}
	})

	t.Run("times out against a port nothing listens on", func(t *testing.T) {
		start := time.Now()

		err := waitReady(context.Background(), "1", 200*time.Millisecond)
		if err == nil {
			t.Fatal("waitReady() expected a timeout error, got nil")
		}

		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("waitReady() took %s, want it to respect the tiny deadline", elapsed)
		}
	})

	t.Run("returns promptly when ctx is already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()

		err := waitReady(ctx, "1", 2*time.Second)
		if err == nil {
			t.Fatal("waitReady() expected an error, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waitReady() error = %v, want wrapping context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("waitReady() took %s, want it to return promptly on an already-cancelled ctx", elapsed)
		}
	})
}
