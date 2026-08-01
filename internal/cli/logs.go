package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// noColorEnv is the de facto standard environment variable (https://no-color.org/)
// that disables ANSI color output regardless of terminal detection, per
// plans/m4-dx.md Unit B's log-streaming decision. Any value at all disables
// color, including "" set-but-empty — matching the standard's own wording
// ("when present, regardless of its value").
const noColorEnv = "NO_COLOR"

// logPalette lists the ANSI foreground color codes cycled across functions'
// `[<name>]` log prefixes, in the order functions first start. Chosen for
// readability on both light and dark terminal backgrounds (no black/white,
// no two adjacent entries too close in hue).
var logPalette = []string{
	"36", // cyan
	"33", // yellow
	"35", // magenta
	"32", // green
	"34", // blue
	"31", // red
	"96", // bright cyan
	"93", // bright yellow
}

const ansiReset = "\x1b[0m"

// logStreamer pumps every running instance's combined stdout+stderr to a
// single sink (dev's stderr), each line prefixed `[<name>] ` with a stable,
// per-function ANSI color — the log-multiplexing half of plans/m4-dx.md
// Unit B. One logStreamer is shared for a whole `dev` run, including across
// a structural reload's Manager rebuild, so a function keeps the same color
// even after its instance is restarted.
type logStreamer struct {
	sink         io.Writer
	colorEnabled bool

	mu      sync.Mutex
	writeMu sync.Mutex
	colors  map[string]string
	next    int
}

// newLogStreamer builds a logStreamer writing to sink. Color is enabled
// only when sink looks like a terminal (see isTerminal) AND $NO_COLOR is
// unset — decided once here rather than per line, since neither condition
// changes for the lifetime of a `dev` run.
func newLogStreamer(sink io.Writer) *logStreamer {
	_, noColor := os.LookupEnv(noColorEnv)

	return &logStreamer{
		sink:         sink,
		colorEnabled: isTerminal(sink) && !noColor,
		colors:       make(map[string]string),
	}
}

// stream copies r's lines to the streamer's sink, prefixed `[<name>] `,
// until r reaches EOF (i.e. the instance producing it stopped, per
// backend.Instance.Logs's doc comment) or a read error occurs. It runs
// synchronously; callers pump it in a goroutine per instance (see dev.go's
// Manager.OnStart hook) so one slow/stuck instance's log stream never
// blocks another's.
//
// Lines are read with a growing buffer (bufio.Reader.ReadString) rather
// than bufio.Scanner's fixed-size token buffer, so an unusually long line
// (e.g. a handler dumping a large JSON blob) is still forwarded whole
// instead of erroring out partway through. A final line with no trailing
// newline (the process exited mid-write) is still flushed before stream
// returns, so nothing an instance wrote is lost.
func (s *logStreamer) stream(name string, r io.Reader) {
	color := s.colorFor(name)

	br := bufio.NewReader(r)

	for {
		line, err := br.ReadString('\n')
		if line != "" {
			s.writeLine(name, strings.TrimRight(line, "\r\n"), color)
		}
		if err != nil {
			return
		}
	}
}

// colorFor returns name's stable palette color, assigning the next unused
// one on first sight and cycling logPalette once every function has one.
func (s *logStreamer) colorFor(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if color, ok := s.colors[name]; ok {
		return color
	}

	color := logPalette[s.next%len(logPalette)]
	s.next++
	s.colors[name] = color

	return color
}

// writeLine writes one already-split, already-trimmed line to the sink as
// `[<name>] <line>\n`, colorizing just the `[<name>]` prefix when color is
// enabled. writeMu serializes the actual sink write across every
// concurrently streaming function, so lines from different functions never
// interleave mid-write.
func (s *logStreamer) writeLine(name, line, color string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.colorEnabled {
		fmt.Fprintf(s.sink, "\x1b[%sm[%s]%s %s\n", color, name, ansiReset, line)
		return
	}

	fmt.Fprintf(s.sink, "[%s] %s\n", name, line)
}

// isTerminal reports whether w is connected to a terminal. It only knows
// how to answer for an *os.File (the concrete type os.Stderr is, and the
// only one dev.go ever passes); anything else (a bytes.Buffer in tests, a
// pipe, …) is conservatively treated as "not a terminal", matching NO_COLOR
// semantics of disabling color when in doubt. Deliberately stdlib-only —
// see plans/m4-dx.md Unit B's scope decision against a new terminal-detection
// dependency.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}

	fi, err := f.Stat()
	if err != nil {
		return false
	}

	return fi.Mode()&os.ModeCharDevice != 0
}
