package container

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
)

// resolvePort runs `<cli> port <id> 8080` and parses the ephemeral host
// port docker published the container's :8080 (the RIE's Invoke API port)
// on.
func resolvePort(ctx context.Context, runner backend.Runner, cli, id string) (string, error) {
	out, err := runner.Run(ctx, cli, "port", id, "8080")
	if err != nil {
		return "", fmt.Errorf("resolving host port: %w", err)
	}

	port, err := parsePort(string(out))
	if err != nil {
		return "", fmt.Errorf("resolving host port: %w", err)
	}

	return port, nil
}

// parsePort extracts a host port from `docker port` output. The command
// prints one "<host>:<port>" line per published address family — normally
// a single "127.0.0.1:PORT" line since Start only publishes on 127.0.0.1,
// but a host with IPv6 enabled can add a second "[::]:PORT"-shaped line.
// parsePort takes the first line it can parse a trailing numeric port from,
// per plans/m1-container-path.md Unit B ("take the first usable").
func parsePort(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		idx := strings.LastIndex(line, ":")
		if idx == -1 || idx == len(line)-1 {
			continue
		}

		port := line[idx+1:]
		if _, err := strconv.Atoi(port); err != nil {
			continue
		}

		return port, nil
	}

	return "", fmt.Errorf("no host port found in %q", output)
}

// waitReady polls 127.0.0.1:port in a backoff loop until it answers an HTTP
// request, timeout elapses, or ctx is cancelled — whichever comes first. It
// is the readiness gate Start blocks on before returning an Instance, per
// backend.Backend.Start's contract that the caller never has to poll for
// readiness itself.
//
// The probe speaks HTTP, not a bare TCP dial: on Docker Desktop (and
// similar VM-backed setups) the host-side docker-proxy accepts TCP
// connections on the published port as soon as the mapping exists — well
// before the RIE inside the container is actually listening on :8080 — and
// only discovers the inner connection is refused once it forwards traffic,
// at which point it drops ours with a bare EOF. A TCP-dial-only probe
// therefore reports "ready" a beat too early and the first invoke can race
// it. Requiring one full HTTP round trip (any response, any status code)
// waits for both hops to be genuinely up.
func waitReady(ctx context.Context, port string, timeout time.Duration) error {
	url := "http://" + net.JoinHostPort("127.0.0.1", port) + "/"
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
// (the RIE has no handler for GET /) counts as "ready"; connection refused,
// reset, or EOF does not.
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
