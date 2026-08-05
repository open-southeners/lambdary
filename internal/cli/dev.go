package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/lockfile"
	"github.com/open-southeners/lambdary/internal/manifest"
	"github.com/open-southeners/lambdary/internal/router"
	"github.com/open-southeners/lambdary/internal/watcher"
)

// shutdownTimeout bounds how long `dev` waits for the HTTP server and the
// running backend instances to stop on SIGINT/SIGTERM before giving up and
// exiting anyway, per plans/m1-container-path.md Unit D ("~10s timeout").
const shutdownTimeout = 10 * time.Second

func newDevCmd() *cobra.Command {
	var (
		port        int
		eager       bool
		backendFlag string
		noReload    bool
	)

	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Discover, watch, and serve functions on a local HTTP server",
		Long: `Discover the functions under --root and serve them all behind one local
HTTP server: the AWS-compatible invoke passthrough
(POST /2015-03-31/functions/<name>/invocations), usable with
"aws lambda invoke --endpoint-url".

Each function's backend starts lazily, on its first invocation, unless
--eager starts every backend up front. --backend picks the default for
functions whose own .lambda.yml local.backend is "auto" (or unset); a
function that sets local.backend to "container" or "process" explicitly
always runs on that one instead, letting functions with different needs
share one dev server. Stop the server with Ctrl-C (SIGINT) or SIGTERM; it
stops every running backend before exiting.

Hot reload is on by default: editing a function's code restarts just that
function (on its next request); adding/removing a function directory or
editing a .lambda.yml/lambdary.yml reconfigures the whole server. A broken
edit is logged and the previous configuration keeps running. --no-reload
turns this off.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := newDevServer(cmd.OutOrStdout(), cmd.ErrOrStderr(), root, port, eager, backendFlag, noReload)
			return d.run(cmd.Context())
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "local HTTP server port (default: lambdary.yml port or 8000)")
	cmd.Flags().BoolVar(&eager, "eager", false, "start every function's backend immediately instead of on first invoke")
	cmd.Flags().StringVar(&backendFlag, "backend", "auto", `default execution backend for functions whose own local.backend is "auto": "auto", "container", or "process"`)
	cmd.Flags().BoolVar(&noReload, "no-reload", false, "disable hot reload (function/config edits require a restart)")

	return cmd
}

// devServer owns one `lambdary dev` run end to end: resolving+discovering
// functions, serving them, and — unless --no-reload — watching for
// filesystem changes and reconfiguring itself live. It exists as its own
// type (rather than a chain of functions, as before M4) so
// internal/cli/e2e_test.go can drive the reload loop directly against a
// real listening HTTP server without exec-ing the built binary.
type devServer struct {
	out, errOut io.Writer

	root        string
	portFlag    int
	eager       bool
	backendFlag string
	noReload    bool

	streamer *logStreamer

	// ready receives the server's actual listen address once bound, so
	// tests (and, in principle, future callers) can dial it without
	// guessing the resolved port. Buffered 1: run() sends at most once.
	ready chan string
}

// newDevServer builds a devServer; see run for what it does.
func newDevServer(out, errOut io.Writer, root string, portFlag int, eager bool, backendFlag string, noReload bool) *devServer {
	return &devServer{
		out:         out,
		errOut:      errOut,
		root:        root,
		portFlag:    portFlag,
		eager:       eager,
		backendFlag: backendFlag,
		noReload:    noReload,
		streamer:    newLogStreamer(errOut),
		ready:       make(chan string, 1),
	}
}

// devStack bundles one resolve+discover+build cycle's output: buildStack
// produces a fresh one at startup and again on every structural reload.
type devStack struct {
	scanRoot string
	cfg      *manifest.Config
	fns      []discovery.Function
	mgr      *router.Manager
	handler  http.Handler
}

// run resolves the backend once, builds the initial devStack, and serves it
// until ctx is cancelled or SIGINT/SIGTERM arrives — see serve for the
// request-serving/reload/shutdown loop.
func (d *devServer) run(ctx context.Context) error {
	if err := validateBackend(d.backendFlag); err != nil {
		return err
	}

	lock, err := lockfile.Load(d.root)
	if err != nil {
		fmt.Fprintf(d.errOut, "warning: ignoring corrupt .lambdary/lock: %s\n", err)
	}

	b, backendName, err := resolveManagerBackend(ctx, d.backendFlag, backend.ExecRunner{}, d.errOut, lock)
	if err != nil {
		return err
	}

	stack, err := d.buildStack(ctx, b)
	if err != nil {
		return err
	}

	if d.eager {
		if err := ensureAll(ctx, stack.mgr, stack.fns); err != nil {
			return err
		}
	}

	port := resolvePort(d.portFlag, stack.cfg)

	renderStartupSummary(d.out, port, stack.fns, backendName)

	return d.serve(ctx, b, stack, port)
}

// buildStack resolves --root, runs discovery, and builds a fresh
// Manager+Router pair over b — the resolve+discover+build cycle shared by
// run's initial startup and serve's structural-reload path. Discovery
// warnings are printed to errOut as they were before M4; a config or
// discovery error is returned as-is, letting the caller decide whether that
// means "fail startup" (run) or "keep the previous configuration" (a
// reload).
func (d *devServer) buildStack(_ context.Context, b router.BackendResolver) (*devStack, error) {
	scanRoot, cfg, err := resolveRoot(d.root)
	if err != nil {
		return nil, err
	}

	fns, err := discovery.Discover(scanRoot, cfg)
	if err != nil {
		return nil, err
	}

	if len(fns) == 0 {
		return nil, fmt.Errorf("no functions found under %s", scanRoot)
	}

	for _, fn := range fns {
		for _, w := range fn.Warnings {
			fmt.Fprintf(d.errOut, "warning: %s\n", w)
		}
	}

	mgr := router.NewManagerWithResolver(b, fns)
	mgr.OnStart = d.onInstanceStart

	return &devStack{
		scanRoot: scanRoot,
		cfg:      cfg,
		fns:      fns,
		mgr:      mgr,
		handler:  router.New(mgr, fns),
	}, nil
}

// onInstanceStart is Manager.OnStart: it pumps a freshly started instance's
// Logs() to d.streamer in its own goroutine, which exits once Logs() (the
// combined stdout+stderr stream) reaches EOF — i.e. once the instance
// stops, per backend.Instance.Logs's doc comment. Manager.OnStart's own
// contract (called outside every Manager lock) is what makes spawning a
// goroutine here safe rather than needing to poll for new instances.
func (d *devServer) onInstanceStart(name string, inst backend.Instance) {
	go d.streamer.stream(name, inst.Logs())
}

// ensureAll starts every function in fns eagerly via mgr.Ensure, used by
// both --eager's startup behaviour and a function-code reload event under
// --eager (restart, then immediately start it again rather than waiting
// for the next request).
func ensureAll(ctx context.Context, mgr *router.Manager, fns []discovery.Function) error {
	for _, fn := range fns {
		if _, err := mgr.Ensure(ctx, fn.Name); err != nil {
			return fmt.Errorf("dev: eager start: %s: %w", fn.Name, err)
		}
	}

	return nil
}

// serve binds port, serves stack's handler behind a stable atomicHandler
// (so a structural reload can swap in a freshly built handler without
// restarting the listener or racing in-flight requests), and runs a single
// event loop handling three concurrent things until one of them ends the
// run: the HTTP server erroring out, SIGINT/SIGTERM, and (unless
// --no-reload) watcher.Watch events. Everything in the loop — including
// which Manager/function set is "current" — is touched by this one
// goroutine only, so none of it needs its own locking; only the
// atomicHandler swap is visible to other goroutines (the HTTP server's own
// request-handling goroutines).
func (d *devServer) serve(ctx context.Context, b router.BackendResolver, stack *devStack, port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Bail out before ever starting the event loop, but still stop any
		// instances stack.mgr already started (e.g. under --eager) rather
		// than leaking them — the same cleanup the loop's own exit path
		// below runs.
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if stopErr := stack.mgr.StopAll(stopCtx); stopErr != nil {
			for _, line := range strings.Split(stopErr.Error(), "\n") {
				fmt.Fprintf(d.errOut, "warning: stopping backend: %s\n", line)
			}
		}

		return fmt.Errorf("dev: listening on %s: %w", addr, err)
	}

	var handler atomicHandler
	handler.Store(stack.handler)

	srv := &http.Server{Handler: &handler}

	d.ready <- ln.Addr().String()

	sigCtx, stopNotify := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopNotify()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()

	curMgr := stack.mgr
	curFns := stack.fns

	var (
		watchEvents <-chan watcher.Event
		watchCancel context.CancelFunc
	)
	if !d.noReload {
		watchEvents, watchCancel = d.startWatch(ctx, stack.scanRoot, curFns)
	}
	defer func() {
		if watchCancel != nil {
			watchCancel()
		}
	}()

	var runErr error

loop:
	for {
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				runErr = fmt.Errorf("dev: serving: %w", err)
			}
			break loop

		case <-sigCtx.Done():
			fmt.Fprintln(d.out, "shutting down...")

			if watchCancel != nil {
				watchCancel()
			}

			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			if err := srv.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintf(d.errOut, "warning: shutting down HTTP server: %s\n", err)
			}
			cancel()

			<-serveErr

			break loop

		case ev, ok := <-watchEvents:
			if !ok {
				// ctx (or the per-watch cancel on a structural reload)
				// ended this watch; nothing more will ever arrive on it.
				watchEvents = nil
				continue
			}

			curMgr, curFns, watchEvents, watchCancel = d.handleReloadEvent(ctx, ev, b, curMgr, curFns, watchEvents, watchCancel, &handler)
		}
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := curMgr.StopAll(stopCtx); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(d.errOut, "warning: stopping backend: %s\n", line)
		}
	}

	return runErr
}

// handleReloadEvent applies one watcher.Event to the running server,
// returning the (possibly unchanged) current Manager/function set/watch
// channel/watch cancel for serve's loop to keep using.
//
// A code change (ev.Function set) restarts just that function via
// Manager.Restart, so the next request cold-starts it fresh — immediately,
// under --eager. A structural change re-runs buildStack; on error, the
// broken edit is logged and the PREVIOUS stack keeps serving (dev never
// exits on a bad edit); on success, the old Manager is stopped before the
// new one starts (so its StopAll can never reach into instances the new
// Manager just started), the handler is swapped atomically, and the
// watcher itself is restarted against the new function set — fsnotify has
// no "add/remove a watched fn dir" operation cheap enough to reuse the old
// watch for this, and Unit A's Watch already does the (re)walk work.
func (d *devServer) handleReloadEvent(
	ctx context.Context,
	ev watcher.Event,
	b router.BackendResolver,
	curMgr *router.Manager,
	curFns []discovery.Function,
	watchEvents <-chan watcher.Event,
	watchCancel context.CancelFunc,
	handler *atomicHandler,
) (*router.Manager, []discovery.Function, <-chan watcher.Event, context.CancelFunc) {
	if !ev.Structural {
		changed := ""
		if len(ev.Paths) > 0 {
			changed = ev.Paths[0]
		}

		fmt.Fprintf(d.errOut, "reloading %s: %s changed\n", ev.Function, changed)

		if err := curMgr.Restart(ctx, ev.Function); err != nil {
			fmt.Fprintf(d.errOut, "warning: restarting %s: %s\n", ev.Function, err)
			return curMgr, curFns, watchEvents, watchCancel
		}

		if d.eager {
			if _, err := curMgr.Ensure(ctx, ev.Function); err != nil {
				fmt.Fprintf(d.errOut, "warning: eager restart %s: %s\n", ev.Function, err)
			}
		}

		return curMgr, curFns, watchEvents, watchCancel
	}

	changed := ""
	if len(ev.Paths) > 0 {
		changed = ev.Paths[0]
	}
	fmt.Fprintf(d.errOut, "reconfiguring: %s changed\n", changed)

	newStack, err := d.buildStack(ctx, b)
	if err != nil {
		fmt.Fprintf(d.errOut, "reload failed: %s — keeping previous configuration\n", err)
		return curMgr, curFns, watchEvents, watchCancel
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	if err := curMgr.StopAll(stopCtx); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(d.errOut, "warning: stopping backend: %s\n", line)
		}
	}
	cancel()

	handler.Store(newStack.handler)

	fmt.Fprintf(d.errOut, "reconfigured: %d functions\n", len(newStack.fns))

	if watchCancel != nil {
		watchCancel()
	}

	var (
		newEvents <-chan watcher.Event
		newCancel context.CancelFunc
	)
	if !d.noReload {
		newEvents, newCancel = d.startWatch(ctx, newStack.scanRoot, newStack.fns)
	}

	return newStack.mgr, newStack.fns, newEvents, newCancel
}

// startWatch starts watcher.Watch over scanRoot+fns under its own
// ctx-derived cancelable context, so a structural reload can cancel just
// this watch (and start a fresh one) without touching serve's own ctx. A
// failure to start (e.g. scanRoot removed out from under dev) disables
// reload for this stack rather than failing the whole request loop — dev
// keeps serving without hot reload until the next successful reconfigure.
//
// Known limitation: scanRoot is discovery's scan root, not necessarily
// --root itself — when a lambdary.yml sets `root:` to point elsewhere (see
// resolveRoot), that lambdary.yml file lives outside the watched tree, so
// edits to it won't trigger a reload. Fixing that would mean watching a
// second, unrelated directory just for one file; per plans/m4-dx.md Unit
// B's scope decision, that's left as a known gap rather than extra
// machinery.
func (d *devServer) startWatch(ctx context.Context, scanRoot string, fns []discovery.Function) (<-chan watcher.Event, context.CancelFunc) {
	watchCtx, cancel := context.WithCancel(ctx)

	ch, err := watcher.Watch(watchCtx, scanRoot, fns)
	if err != nil {
		cancel()
		fmt.Fprintf(d.errOut, "warning: hot reload disabled: starting watcher: %s\n", err)
		return nil, nil
	}

	return ch, cancel
}

// atomicHandler is a stable http.Handler whose ServeHTTP delegates to
// whatever inner http.Handler Store last set. dev's http.Server is
// constructed once with an *atomicHandler as its Handler, so a structural
// reload can swap in a freshly built router.Router (over a freshly built
// Manager) without restarting the listener: any request already inside
// ServeHTTP when Store runs keeps running against the handler it already
// loaded, and every request afterwards sees the new one.
type atomicHandler struct {
	ptr atomic.Pointer[http.Handler]
}

// Store atomically sets h as the handler every subsequent ServeHTTP call
// delegates to.
func (a *atomicHandler) Store(h http.Handler) {
	a.ptr.Store(&h)
}

// ServeHTTP implements http.Handler by delegating to the currently stored
// handler. A request arriving before Store has ever been called (shouldn't
// happen in dev's own use — Store runs before srv.Serve starts accepting)
// gets a 503 rather than a nil-pointer panic.
func (a *atomicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := a.ptr.Load()
	if h == nil {
		http.Error(w, "server not ready", http.StatusServiceUnavailable)
		return
	}

	(*h).ServeHTTP(w, r)
}

// renderStartupSummary prints dev's startup banner to out: the server URL,
// the active backend (see resolveBackend), each discovered function's
// name/route/runtime, and an invoke hint showing the AWS-compatible
// passthrough URL pattern, per plans/m1-container-path.md Unit D and
// plans/m3-process-path.md Unit C.
func renderStartupSummary(out io.Writer, port int, fns []discovery.Function, backendName string) {
	fmt.Fprintf(out, "lambdary dev server listening on http://127.0.0.1:%d\n", port)
	fmt.Fprintf(out, "backend: %s\n\n", backendName)

	for _, fn := range fns {
		fnRuntime := fn.Runtime
		if fnRuntime == "" {
			fnRuntime = "-"
		}

		fmt.Fprintf(out, "  %s  %s  (%s)\n", fn.Name, fn.Route, fnRuntime)
	}

	fmt.Fprintf(out, "\nInvoke a function directly:\n")
	fmt.Fprintf(out, "  curl -X POST http://127.0.0.1:%d/2015-03-31/functions/<name>/invocations -d '<json event>'\n", port)
	fmt.Fprintf(out, "aws lambda invoke --endpoint-url http://127.0.0.1:%d --function-name <name> out.json also works.\n", port)
}
