package process

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// reservedPort is the RIE's internal Runtime API default — see
// plans/rie-darwin-spike.md's port-9001 pitfall. allocatePorts excludes it
// from both ports it hands out, even though rieArgs always passes both
// address flags explicitly, because a stray 9001 for either one is a
// footgun not worth ever risking.
const reservedPort = 9001

// maxPortAttempts bounds the retry loops in allocatePort and allocatePorts,
// so a pathological host (or a test asserting on the reservedPort
// exclusion) can't spin forever.
const maxPortAttempts = 20

// allocatePorts returns two distinct ephemeral ports for one function
// instance: invokePort (the RIE's public Invoke API,
// --runtime-interface-emulator-address) and rapiPort (the RIE's internal
// Runtime API, --runtime-api-address). Each comes from allocatePort, which
// never returns reservedPort; allocatePorts additionally retries rapiPort
// until it differs from invokePort, since both are drawn from the same
// ephemeral range and a repeat, while unlikely, is possible.
func allocatePorts() (invokePort, rapiPort int, err error) {
	invokePort, err = allocatePort()
	if err != nil {
		return 0, 0, err
	}

	for i := 0; i < maxPortAttempts; i++ {
		rapiPort, err = allocatePort()
		if err != nil {
			return 0, 0, err
		}

		if rapiPort != invokePort {
			return invokePort, rapiPort, nil
		}
	}

	return 0, 0, fmt.Errorf("process: could not allocate two distinct ports after %d attempts", maxPortAttempts)
}

// allocatePort binds 127.0.0.1:0, letting the OS assign an ephemeral port,
// then immediately closes the listener and returns the port number — the
// same bind-and-close trick the container backend relies on indirectly via
// `docker run -p 127.0.0.1:0:8080`. This has an inherent TOCTOU window
// between closing the listener and the RIE binding the same port; accepted
// as a known limitation for local dev, per plans/m3-process-path.md's
// Deferred section.
func allocatePort() (int, error) {
	for i := 0; i < maxPortAttempts; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, fmt.Errorf("allocating port: %w", err)
		}

		port := l.Addr().(*net.TCPAddr).Port // net.Listen("tcp", ...) always returns a *net.TCPAddr.

		if err := l.Close(); err != nil {
			return 0, fmt.Errorf("allocating port: %w", err)
		}

		if port != reservedPort {
			return port, nil
		}
	}

	return 0, fmt.Errorf("process: could not allocate a port other than %d after %d attempts", reservedPort, maxPortAttempts)
}

// waitReady polls 127.0.0.1:port (the RIE's public Invoke API) in a
// backoff loop until it answers an HTTP request, timeout elapses, or ctx is
// cancelled — whichever comes first. It is the readiness gate Start blocks
// on before returning an Instance, per backend.Backend.Start's contract
// that the caller never has to poll for readiness itself. Mirrors
// container/port.go's waitReady (same "one full HTTP round trip, not a bare
// TCP dial" lesson — a bare listener can accept before its handler is
// wired up), adapted for an int port and no container CLI to shell out to.
func waitReady(ctx context.Context, port int, timeout time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	client := &http.Client{}
	deadline := time.Now().Add(timeout)

	backoff := 50 * time.Millisecond
	const maxBackoff = 500 * time.Millisecond

	var lastErr error

	for {
		attemptCtx, cancel := context.WithTimeout(ctx, minDuration(backoff*4, time.Until(deadline)))
		err := probe(attemptCtx, client, url)
		cancel()

		if err == nil {
			return nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return fmt.Errorf("waiting for %s to be ready: %w", url, ctx.Err())
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("waiting for %s to be ready: timed out after %s: %w", url, timeout, lastErr)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to be ready: %w", url, ctx.Err())
		case <-time.After(minDuration(backoff, time.Until(deadline))):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// probe issues a single GET to url and reports whether the round trip
// completed at the transport level — any HTTP response, including a 4xx
// (the RIE has no handler for GET /), counts as "ready"; connection
// refused, reset, or EOF does not.
func probe(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // draining/closing a probe response body, not a meaningful failure mode.

	return nil
}

// minDuration returns the smaller of a and b, treating a non-positive b as
// "no constraint" so callers computing a remaining-time bound don't have to
// special-case an already-passed deadline into a negative timeout.
func minDuration(a, b time.Duration) time.Duration {
	if b > 0 && b < a {
		return b
	}

	return a
}
