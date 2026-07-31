package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/router"
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
	)

	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Discover, watch, and serve functions on a local HTTP server",
		Long: `Discover the functions under --root and serve them all behind one local
HTTP server: the AWS-compatible invoke passthrough
(POST /2015-03-31/functions/<name>/invocations), usable with
"aws lambda invoke --endpoint-url".

Each function's backend starts lazily, on its first invocation, unless
--eager starts every backend up front. Stop the server with Ctrl-C
(SIGINT) or SIGTERM; it stops every running backend before exiting.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDev(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), root, port, eager, backendFlag)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "local HTTP server port (default: lambdary.yml port or 8000)")
	cmd.Flags().BoolVar(&eager, "eager", false, "start every function's backend immediately instead of on first invoke")
	cmd.Flags().StringVar(&backendFlag, "backend", "auto", `execution backend: "auto", "container", or "process"`)

	return cmd
}

// runDev resolves root+config, discovers functions, resolves the requested
// backend (see resolveBackend), builds the router.Manager/Router pair, and
// serves it on 127.0.0.1:<port> until ctx is cancelled or a SIGINT/SIGTERM
// arrives.
func runDev(ctx context.Context, out, errOut io.Writer, root string, portFlag int, eager bool, backendFlag string) error {
	if err := validateBackend(backendFlag); err != nil {
		return err
	}

	scanRoot, cfg, err := resolveRoot(root)
	if err != nil {
		return err
	}

	fns, err := discovery.Discover(scanRoot, cfg)
	if err != nil {
		return err
	}

	if len(fns) == 0 {
		return fmt.Errorf("no functions found under %s", scanRoot)
	}

	for _, fn := range fns {
		for _, w := range fn.Warnings {
			fmt.Fprintf(errOut, "warning: %s\n", w)
		}
	}

	b, backendName, err := resolveBackend(ctx, backendFlag, backend.ExecRunner{}, errOut)
	if err != nil {
		return err
	}

	mgr := router.NewManager(b, fns)
	handler := router.New(mgr, fns)

	if eager {
		for _, fn := range fns {
			if _, err := mgr.Ensure(ctx, fn.Name); err != nil {
				return fmt.Errorf("dev: eager start: %s: %w", fn.Name, err)
			}
		}
	}

	port := resolvePort(portFlag, cfg)

	renderStartupSummary(out, port, fns, backendName)

	return serve(ctx, out, errOut, handler, mgr, port)
}

// serve runs an http.Server for handler on 127.0.0.1:<port> until ctx is
// cancelled or SIGINT/SIGTERM arrives, then gracefully shuts the server
// down and stops every backend instance mgr started, per
// plans/m1-container-path.md Unit D's shutdown sequence.
func serve(ctx context.Context, out, errOut io.Writer, handler http.Handler, mgr *router.Manager, port int) error {
	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: handler,
	}

	sigCtx, stopNotify := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopNotify()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	var runErr error

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("dev: serving: %w", err)
		}
	case <-sigCtx.Done():
		fmt.Fprintln(out, "shutting down...")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(errOut, "warning: shutting down HTTP server: %s\n", err)
		}

		<-serveErr
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := mgr.StopAll(stopCtx); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(errOut, "warning: stopping backend: %s\n", line)
		}
	}

	return runErr
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
