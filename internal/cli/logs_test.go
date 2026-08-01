package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// newTestLogStreamer builds a logStreamer writing to buf with color forced
// to colorEnabled, bypassing newLogStreamer's terminal detection (a
// bytes.Buffer is never a terminal, so tests that need color on construct
// the struct directly).
func newTestLogStreamer(buf *bytes.Buffer, colorEnabled bool) *logStreamer {
	return &logStreamer{
		sink:         buf,
		colorEnabled: colorEnabled,
		colors:       make(map[string]string),
	}
}

func TestLogStreamerSplitsAndPrefixesLines(t *testing.T) {
	var buf bytes.Buffer
	s := newTestLogStreamer(&buf, false)

	s.stream("hello", strings.NewReader("first line\nsecond line\nthird line\n"))

	want := "[hello] first line\n[hello] second line\n[hello] third line\n"
	if got := buf.String(); got != want {
		t.Errorf("stream() output = %q, want %q", got, want)
	}
}

func TestLogStreamerFlushesPartialTrailingLine(t *testing.T) {
	var buf bytes.Buffer
	s := newTestLogStreamer(&buf, false)

	// No trailing newline: the "instance" wrote a line and then stopped
	// mid-write (EOF), same as a process exiting without a final \n.
	s.stream("hello", strings.NewReader("complete line\nincomplete line"))

	want := "[hello] complete line\n[hello] incomplete line\n"
	if got := buf.String(); got != want {
		t.Errorf("stream() output = %q, want %q (partial trailing line flushed on EOF)", got, want)
	}
}

func TestLogStreamerHandlesLongLines(t *testing.T) {
	var buf bytes.Buffer
	s := newTestLogStreamer(&buf, false)

	// Larger than bufio.Scanner's default 64KiB token limit, to prove the
	// growing-buffer ReadString approach doesn't choke on it the way a
	// bare bufio.Scanner would (bufio.ErrTooLong).
	long := strings.Repeat("x", 200*1024)

	s.stream("hello", strings.NewReader(long+"\n"))

	want := "[hello] " + long + "\n"
	if got := buf.String(); got != want {
		t.Errorf("stream() output length = %d, want %d (long line preserved whole)", len(got), len(want))
	}
}

func TestLogStreamerColorEnabled(t *testing.T) {
	var buf bytes.Buffer
	s := newTestLogStreamer(&buf, true)

	s.stream("hello", strings.NewReader("hi\n"))

	got := buf.String()
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("stream() output = %q, want an ANSI escape sequence when color is enabled", got)
	}
	if !strings.Contains(got, "hi\n") {
		t.Errorf("stream() output = %q, want to still contain the plain line text", got)
	}
}

func TestLogStreamerColorDisabled(t *testing.T) {
	var buf bytes.Buffer
	s := newTestLogStreamer(&buf, false)

	s.stream("hello", strings.NewReader("hi\n"))

	if got := buf.String(); strings.Contains(got, "\x1b[") {
		t.Errorf("stream() output = %q, want no ANSI escape sequences when color is disabled", got)
	}
}

func TestLogStreamerColorStableAcrossCalls(t *testing.T) {
	var buf bytes.Buffer
	s := newTestLogStreamer(&buf, true)

	first := s.colorFor("hello")
	second := s.colorFor("hello")
	if first != second {
		t.Errorf("colorFor(hello) = %q then %q, want the same color both times", first, second)
	}

	other := s.colorFor("world")
	if other == first {
		t.Errorf("colorFor(world) = %q, want a different color than hello's %q (2 distinct functions)", other, first)
	}
}

func TestNewLogStreamerDisablesColorWhenNoColorSet(t *testing.T) {
	t.Setenv(noColorEnv, "1")

	s := newLogStreamer(os.Stderr)
	if s.colorEnabled {
		t.Errorf("colorEnabled = true with $NO_COLOR set, want false")
	}
}

func TestNewLogStreamerDisablesColorForNonTerminalSink(t *testing.T) {
	var buf bytes.Buffer

	s := newLogStreamer(&buf)
	if s.colorEnabled {
		t.Errorf("colorEnabled = true for a non-*os.File sink, want false")
	}
}

func TestIsTerminalFalseForNonFile(t *testing.T) {
	var buf bytes.Buffer
	if isTerminal(&buf) {
		t.Errorf("isTerminal(bytes.Buffer) = true, want false")
	}
}
