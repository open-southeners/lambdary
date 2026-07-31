package process

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestAllocatePorts(t *testing.T) {
	invokePort, rapiPort, err := allocatePorts()
	if err != nil {
		t.Fatalf("allocatePorts() unexpected error: %v", err)
	}

	if invokePort == rapiPort {
		t.Errorf("allocatePorts() = (%d, %d), want two distinct ports", invokePort, rapiPort)
	}
	if invokePort == reservedPort || rapiPort == reservedPort {
		t.Errorf("allocatePorts() = (%d, %d), want neither to be the reserved port %d", invokePort, rapiPort, reservedPort)
	}
	if invokePort <= 0 || rapiPort <= 0 {
		t.Errorf("allocatePorts() = (%d, %d), want both positive", invokePort, rapiPort)
	}
}

func TestAllocatePortExcludesReserved(t *testing.T) {
	// allocatePort itself can't be forced to hit 9001 deterministically,
	// so this just guards the invariant across many draws instead of
	// asserting a specific collision.
	for i := 0; i < 200; i++ {
		port, err := allocatePort()
		if err != nil {
			t.Fatalf("allocatePort() unexpected error: %v", err)
		}
		if port == reservedPort {
			t.Fatalf("allocatePort() returned the reserved port %d", reservedPort)
		}
	}
}

func TestWaitReady(t *testing.T) {
	t.Run("succeeds once the port answers HTTP", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(srv.Close)

		_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
		if err != nil {
			t.Fatalf("net.SplitHostPort() unexpected error: %v", err)
		}

		port, err := strconv.Atoi(portStr)
		if err != nil {
			t.Fatalf("parsing port: %v", err)
		}

		if err := waitReady(context.Background(), port, 2*time.Second); err != nil {
			t.Errorf("waitReady() unexpected error: %v", err)
		}
	})

	t.Run("times out against a port nothing listens on", func(t *testing.T) {
		start := time.Now()
		err := waitReady(context.Background(), unusedPort(t), 150*time.Millisecond)
		if err == nil {
			t.Fatal("waitReady() expected a timeout error, got nil")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("waitReady() took %s, want it to respect the tiny timeout", elapsed)
		}
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := waitReady(ctx, unusedPort(t), 2*time.Second); err == nil {
			t.Error("waitReady() expected an error for an already-cancelled context, got nil")
		}
	})
}

// unusedPort allocates and immediately releases a port, for tests that want
// an address nothing listens on.
func unusedPort(t *testing.T) int {
	t.Helper()

	port, err := allocatePort()
	if err != nil {
		t.Fatalf("allocatePort() unexpected error: %v", err)
	}

	return port
}
