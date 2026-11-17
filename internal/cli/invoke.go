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
	"github.com/open-southeners/lambdary/internal/fixtures"
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

// invokeHTTPMargin is how much longer standalone mode's HTTP call waits
// than the function's own timeout, so the runtime's timeout response
// arrives before the client gives up.
const invokeHTTPMargin = 5 * time.Second

func newInvokeCmd() *cobra.Command {
	var (
		eventSource string
		sets        []string
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
quick one-off check without a dev server running.

Event: --event takes a JSON file path, "-" for stdin, or a sample AWS event
type — sqs, s3, schedule or apigw — which builds a realistic payload (a file
named like the type takes precedence). Override its fields with repeatable
--set key=value; --set needs a sample event type. Keys:
  sqs:      body, queue, count, attributes.<k>
  s3:       bucket, key, size, event (default ObjectCreated:Put)
  schedule: rule, time, detail (JSON)
  apigw:    method, path, body, query, header.<k>

Example: lambdary invoke worker -e sqs --set body='{"id":1}' --set count=3`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInvoke(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), root, args[0], eventSource, sets, port, backendFlag)
		},
	}

	cmd.Flags().StringVarP(&eventSource, "event", "e", "", `path to a JSON event file, "-" for stdin, or a sample event type (`+strings.Join(fixtures.Kinds(), ", ")+`) (default: "{}")`)
	cmd.Flags().StringArrayVar(&sets, "set", nil, "override a field of a sample event type, as key=value (repeatable; see the command help for keys)")
	cmd.Flags().IntVar(&port, "port", 0, "dev server port to probe (default: lambdary.yml port or 8000)")
	cmd.Flags().StringVar(&backendFlag, "backend", "auto", `execution backend for standalone mode when the function's own local.backend is "auto": "auto", "container", or "process" (ignored in server mode)`)

	return cmd
}

// runInvoke resolves the event body and target port, then picks server or
// standalone mode by probing --port for a running dev server, per
// plans/m1-container-path.md Unit D. backendFlag only matters in standalone
// mode: server mode reuses whatever backend the running dev server already
// picked.
func runInvoke(ctx context.Context, out, errOut io.Writer, stdin io.Reader, root, name, eventSource string, sets []string, portFlag int, backendFlag string) error {
	if err := validateBackend(backendFlag); err != nil {
		return err
	}

	event, err := resolveEvent(eventSource, sets, stdin)
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

	for _, w := range fn.Warnings {
		fmt.Fprintf(errOut, "warning: %s\n", w)
	}

	lock, err := lockfile.Load(root)
	if err != nil {
		fmt.Fprintf(errOut, "warning: ignoring corrupt .lambdary/lock: %s\n", err)
	}
	cacheDir := cacheDirFor(root)

	// Only fn's own layers matter for a single standalone invoke — no need
	// to fetch every other discovered function's ARNs (and no need for
	// credentials they might require) just to run this one.
	ensureLayersFetched(ctx, []discovery.Function{fn}, cacheDir, lock, errOut)

	return invokeStandalone(ctx, out, errOut, fn, event, backendFlag, lock, cacheDir)
}

// resolveEvent reads the invoke payload from source: a JSON file path,
// "-" for stdin, a fixture kind (see internal/fixtures: sqs, s3, schedule,
// apigw), or "" (unset) for the default empty event "{}". A kind builds its
// sample event with sets ("key=value" overrides) applied, but only when no
// file named like the kind exists, so a file called "sqs" still wins. sets
// are only valid with a fixture kind.
func resolveEvent(source string, sets []string, stdin io.Reader) ([]byte, error) {
	if fixtures.IsKind(source) {
		if _, err := os.Stat(source); err != nil {
			set, err := parseSets(sets)
			if err != nil {
				return nil, err
			}

			return fixtures.Build(source, set)
		}
	}

	if len(sets) > 0 {
		return nil, fmt.Errorf("invoke: --set requires --event to be a fixture type (%s)", strings.Join(fixtures.Kinds(), ", "))
	}

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

// parseSets turns repeated "key=value" --set flags into a map; a later
// duplicate key wins.
func parseSets(sets []string) (map[string]string, error) {
	out := make(map[string]string, len(sets))

	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invoke: invalid --set %q; want key=value", s)
		}

		out[k] = v
	}

	return out, nil
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

// effectiveInvokeBackendMode returns the backend mode invokeStandalone
// should resolve: fn's own explicit local.backend hint ("container" or
// "process") always wins over backendFlag; "auto" (or unset) defers to
// backendFlag — the same per-function precedence `dev` applies (see
// resolveManagerBackend).
func effectiveInvokeBackendMode(fn discovery.Function, backendFlag string) string {
	if fn.Backend == "container" || fn.Backend == "process" {
		return fn.Backend
	}

	return backendFlag
}

// invokeStandalone resolves the backend named by backendFlag (see
// resolveBackend) — unless fn's own local.backend hint explicitly names
// "container" or "process", which always wins over backendFlag, the same
// per-function precedence `dev` applies (see resolveManagerBackend) — and
// starts fn's backend just for this one call, POSTs event to it, prints
// the response to out, and stops the instance again — via defer, so Stop
// runs on every path once Start succeeds, including invoke errors. lock
// (nil-able) is threaded into resolveBackend for image-digest
// pinning/recording, per plans/m5-extras.md's Unit C. cacheDir is the
// project's `.lambdary` directory (see cacheDirFor), threaded alongside
// lock into every backend this resolves.
//
// When the resolved backend is process and fn's runtime has no
// process-backend shim (needsContainerFallback), invokeStandalone falls
// back to the container backend before Start, printing the same one-line
// notice to errOut that dev's per-function resolver prints — see
// fallbackToContainerBackend and
// plans/process-ruby-and-container-fallback.md's Unit B. Standalone invoke
// has no cross-call cache to dedupe against (it runs once and exits), so
// the notice is unconditional here, unlike perFunctionBackend's
// once-per-function guard.
func invokeStandalone(ctx context.Context, out, errOut io.Writer, fn discovery.Function, event []byte, backendFlag string, lock *lockfile.Lock, cacheDir string) error {
	b, kind, _, err := resolveBackend(ctx, effectiveInvokeBackendMode(fn, backendFlag), backend.ExecRunner{}, errOut, lock, cacheDir)
	if err != nil {
		return err
	}

	if needsContainerFallback(kind, fn) {
		b, err = fallbackToContainerBackend(ctx, fn, func(ctx context.Context) (backend.Backend, error) {
			containerBackend, _, _, err := resolveContainerBackend(ctx, backend.ExecRunner{}, lock, cacheDir)
			return containerBackend, err
		}, func() {
			fmt.Fprint(errOut, containerFallbackNotice(fn))
		})
		if err != nil {
			return err
		}
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

	invokeCtx := ctx

	if d := functionTimeout(fn); d > 0 {
		var cancel context.CancelFunc

		invokeCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

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
// effective timeout (manifest.DefaultTimeout when unset) plus
// invokeHTTPMargin, mirroring internal/router.Manager.InvokeTimeout for the
// standalone path, which has no Manager of its own. 0 means no deadline,
// for a function whose timeout is 0 (no limit).
func functionTimeout(fn discovery.Function) time.Duration {
	secs := fn.Manifest.TimeoutSeconds()
	if secs == 0 {
		return 0
	}

	return time.Duration(secs)*time.Second + invokeHTTPMargin
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
