package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/lockfile"
)

// serverProbeTimeout bounds how long invoke waits for a dev server's GET /
// to answer before falling back to standalone mode.
const serverProbeTimeout = 500 * time.Millisecond

// standaloneStartTimeout bounds how long standalone mode waits for the
// function's backend to start, including a possible first-run image pull
// — generous, since AWS's Lambda base images can be large and slow to
// pull on a first run (see internal/backend/container's own readiness
// wait, which this timeout wraps).
const standaloneStartTimeout = 5 * time.Minute

// standaloneStopTimeout bounds how long standalone mode waits for the
// instance it started to stop again before giving up.
const standaloneStopTimeout = 30 * time.Second

// defaultInvokeHTTPTimeout is the invoke HTTP call's deadline when a
// function's manifest leaves timeout unset, mirroring
// internal/router.Manager's own default.
const defaultInvokeHTTPTimeout = 30 * time.Second

func newInvokeCmd() *cobra.Command {
	var (
		eventSource string
		port        int
		backendFlag string
	)

	cmd := &cobra.Command{
		Use:   "invoke <function>",
		Short: "Invoke one function once and print its response",
		Long: `Invoke one function once and print its response.

Server mode: if a "lambdary dev" server is already listening on --port,
invoke POSTs the event to its AWS-compatible passthrough
(POST /2015-03-31/functions/<function>/invocations) and streams the
response it returns.

Standalone mode: if no dev server answers on --port, invoke discovers the
function itself, starts its backend just for this one call (per --backend),
invokes it, prints the response, and stops the backend again — useful for a
quick one-off check without a dev server running.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInvoke(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), root, args[0], eventSource, port, backendFlag)
		},
	}

	cmd.Flags().StringVarP(&eventSource, "event", "e", "", `path to a JSON event file ("-" reads stdin; default: "{}")`)
	cmd.Flags().IntVar(&port, "port", 0, "dev server port to probe (default: lambdary.yml port or 8000)")
	cmd.Flags().StringVar(&backendFlag, "backend", "auto", `execution backend for standalone mode: "auto", "container", or "process" (ignored in server mode)`)

	return cmd
}

// runInvoke resolves the event body and target port, then picks server or
// standalone mode by probing --port for a running dev server, per
// plans/m1-container-path.md Unit D. backendFlag only matters in standalone
// mode: server mode reuses whatever backend the running dev server already
// picked.
func runInvoke(ctx context.Context, out, errOut io.Writer, stdin io.Reader, root, name, eventSource string, portFlag int, backendFlag string) error {
	if err := validateBackend(backendFlag); err != nil {
		return err
	}

	event, err := resolveEvent(eventSource, stdin)
	if err != nil {
		return err
	}

	scanRoot, cfg, err := resolveRoot(root)
	if err != nil {
		return err
	}

	port := resolvePort(portFlag, cfg)

	if detectServer(ctx, port) {
		return invokeServer(ctx, out, errOut, port, name, event)
	}

	fns, err := discovery.Discover(scanRoot, cfg)
	if err != nil {
		return err
	}

	fn, err := findFunction(fns, name)
	if err != nil {
		return err
	}

	lock, err := lockfile.Load(root)
	if err != nil {
		fmt.Fprintf(errOut, "warning: ignoring corrupt .lambdary/lock: %s\n", err)
	}

	return invokeStandalone(ctx, out, errOut, fn, event, backendFlag, lock)
}

// resolveEvent reads the invoke payload from source: a JSON file path,
// "-" for stdin, or "" (unset) for the default empty event "{}".
func resolveEvent(source string, stdin io.Reader) ([]byte, error) {
	switch source {
	case "":
		return []byte("{}"), nil
	case "-":
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("invoke: reading event from stdin: %w", err)
		}

		return data, nil
	default:
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("invoke: reading event file %s: %w", source, err)
		}

		return data, nil
	}
}

// detectServer probes http://127.0.0.1:<port>/ and reports whether a
// lambdary dev server answered there, identified by router.Router's
// GET / index response ({"server":"lambdary", ...}).
func detectServer(ctx context.Context, port int) bool {
	probeCtx, cancel := context.WithTimeout(ctx, serverProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	if err != nil {
		return false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var payload struct {
		Server string `json:"server"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false
	}

	return payload.Server == "lambdary"
}

// invokeServer POSTs event to the dev server listening on port's invoke
// passthrough for name, streaming a 2xx response body to out. A non-2xx
// response is written to errOut instead, and invokeServer returns an
// error so the caller exits non-zero.
func invokeServer(ctx context.Context, out, errOut io.Writer, port int, name string, event []byte) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/2015-03-31/functions/%s/invocations", port, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(event))
	if err != nil {
		return fmt.Errorf("invoke: building request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("invoke: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort; nothing to do about a read error while already reporting one.
		fmt.Fprintln(errOut, string(body))

		return fmt.Errorf("invoke: %s: server responded %d", name, resp.StatusCode)
	}

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("invoke: streaming response: %w", err)
	}

	return nil
}

// invokeStandalone resolves the backend named by backendFlag (see
// resolveBackend), starts fn's backend just for this one call, POSTs event
// to it, prints the response to out, and stops the instance again — via
// defer, so Stop runs on every path once Start succeeds, including invoke
// errors. lock (nil-able) is threaded into resolveBackend for image-digest
// pinning/recording, per plans/m5-extras.md's Unit C.
func invokeStandalone(ctx context.Context, out, errOut io.Writer, fn discovery.Function, event []byte, backendFlag string, lock *lockfile.Lock) error {
	b, _, err := resolveBackend(ctx, backendFlag, backend.ExecRunner{}, errOut, lock)
	if err != nil {
		return err
	}

	startCtx, cancel := context.WithTimeout(ctx, standaloneStartTimeout)
	defer cancel()

	inst, err := b.Start(startCtx, fn)
	if err != nil {
		return fmt.Errorf("invoke: starting %s: %w", fn.Name, err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), standaloneStopTimeout)
		defer cancel()

		_ = inst.Stop(stopCtx)
	}()

	invokeCtx, cancel := context.WithTimeout(ctx, functionTimeout(fn))
	defer cancel()

	req, err := http.NewRequestWithContext(invokeCtx, http.MethodPost, inst.InvokeURL(), bytes.NewReader(event))
	if err != nil {
		return fmt.Errorf("invoke: building request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("invoke: %s: %w", fn.Name, err)
	}
	defer resp.Body.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("invoke: streaming response: %w", err)
	}

	return nil
}

// functionTimeout returns the invoke HTTP call's deadline for fn: its
// manifest's timeout when set, else defaultInvokeHTTPTimeout — mirroring
// internal/router.Manager.InvokeTimeout's default for the standalone path,
// which has no Manager of its own.
func functionTimeout(fn discovery.Function) time.Duration {
	if fn.Manifest != nil && fn.Manifest.Timeout > 0 {
		return time.Duration(fn.Manifest.Timeout) * time.Second
	}

	return defaultInvokeHTTPTimeout
}

// findFunction returns the single discovered function named name. An
// absent name errors listing every known function name; a name shared by
// more than one function (a discovery.Function name collision) errors
// naming the competing directories, matching internal/router's own
// name-collision handling.
func findFunction(fns []discovery.Function, name string) (discovery.Function, error) {
	var matches []discovery.Function

	for _, fn := range fns {
		if fn.Name == name {
			matches = append(matches, fn)
		}
	}

	switch len(matches) {
	case 0:
		names := make([]string, len(fns))
		for i, fn := range fns {
			names[i] = fn.Name
		}

		return discovery.Function{}, fmt.Errorf("invoke: function %q not found; known functions: %s", name, strings.Join(names, ", "))
	case 1:
		return matches[0], nil
	default:
		dirs := make([]string, len(matches))
		for i, fn := range matches {
			dirs[i] = fn.Dir
		}

		return discovery.Function{}, fmt.Errorf("invoke: function name %q is used by multiple functions: %s", name, strings.Join(dirs, ", "))
	}
}
