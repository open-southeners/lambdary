package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/open-southeners/lambdary/internal/logbus"
)

// defaultLogsTail is how many past lines `lambdary logs` shows by default.
const defaultLogsTail = 100

func newLogsCmd() *cobra.Command {
	var (
		follow bool
		tail   int
		port   int
	)

	cmd := &cobra.Command{
		Use:   "logs [function]",
		Short: "Print the output of a running dev server's functions",
		Long: `Print the output of a running dev server's functions.

Asks the "lambdary dev" server listening on --port for its buffered function
output (the last 1,000 lines per function) and prints the last --tail lines as
"[function] line", for one function or, without an argument, all of them.
A bare name includes every published version of the function; name:version
or name:alias selects just that version, or the versions the alias routes to.
With --follow it keeps printing new lines until interrupted.

Example: lambdary logs worker -f --tail 20`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tail < 0 {
				return fmt.Errorf("--tail must not be negative, got %d", tail)
			}

			_, cfg, err := resolveRoot(root)
			if err != nil {
				return err
			}

			name := ""
			if len(args) == 1 {
				name = args[0]
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			return runLogs(ctx, cmd.OutOrStdout(), resolvePort(port, cfg), name, follow, tail)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines until interrupted")
	cmd.Flags().IntVar(&tail, "tail", defaultLogsTail, "how many past lines to show")
	cmd.Flags().IntVar(&port, "port", 0, "dev server port (default: lambdary.yml port or 8000)")

	return cmd
}

// runLogs streams the dev server on port's /_lambdary/logs to out as
// `[function] text` lines, colored like dev's own output when out is a
// terminal. Following ends cleanly when ctx is cancelled.
func runLogs(ctx context.Context, out io.Writer, port int, function string, follow bool, tail int) error {
	if !detectServer(ctx, port) {
		if err := ctx.Err(); err != nil {
			return nil
		}

		return fmt.Errorf("no lambdary dev server on port %d", port)
	}

	query := url.Values{}
	if function != "" {
		query.Set("function", function)
	}

	query.Set("tail", strconv.Itoa(tail))

	if follow {
		query.Set("follow", "1")
	}

	endpoint := fmt.Sprintf("http://127.0.0.1:%d/_lambdary/logs?%s", port, query.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("reading logs from the dev server on port %d: %w", port, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return logsStatusError(resp)
	}

	streamer := newLogStreamer(out)

	br := bufio.NewReader(resp.Body)

	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			var line logbus.Line
			if jsonErr := json.Unmarshal(raw, &line); jsonErr != nil {
				return fmt.Errorf("decoding log line: %w", jsonErr)
			}

			fmt.Fprintln(out, formatLogLine(line.Function, line.Text, streamer.colorFor(line.Function), streamer.colorEnabled))
		}

		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("reading logs: %w", err)
		}
	}
}

// logsStatusError turns a non-200 logs response into an error carrying the
// server's message.
func logsStatusError(resp *http.Response) error {
	var payload struct {
		Message   string   `json:"message"`
		Functions []string `json:"functions"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&payload) //nolint:errcheck // an unreadable body just leaves the status text.

	msg := payload.Message
	if msg == "" {
		msg = resp.Status
	}

	if len(payload.Functions) > 0 {
		return fmt.Errorf("%s (known functions: %v)", msg, payload.Functions)
	}

	return errors.New(msg)
}
