package process

import (
	"io"
	"sync"
)

// outputBuf is a thread-safe, growing byte buffer the RIE's combined
// stdout+stderr writes into. Unlike an io.Pipe (what backend.ExecRunner.
// Start uses for the container backend's short-lived commands), Write
// never blocks even when nothing is reading: a blocked stdout write inside
// the RIE — because Logs() hasn't been called, or its reader is slow — must
// never stall the invoke the RIE is serving. Read replays everything
// written so far before blocking for more, so a reader that starts after
// Write calls have already happened (e.g. a readiness-timeout tail, or
// `dev` attaching to logs after startup) still sees earlier output.
//
// This trades unbounded memory growth over a very long-lived instance for
// that non-blocking guarantee; acceptable for local dev sessions, and
// consistent with plans/m3-process-path.md's other locally-scoped
// tradeoffs (see the Deferred section).
type outputBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	data   []byte
	closed bool
}

// newOutputBuf returns a ready-to-use outputBuf.
func newOutputBuf() *outputBuf {
	b := &outputBuf{}
	b.cond = sync.NewCond(&b.mu)

	return b
}

// Write implements io.Writer, appending p and waking any blocked readers.
// It always succeeds.
func (b *outputBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.data = append(b.data, p...)
	b.mu.Unlock()

	b.cond.Broadcast()

	return len(p), nil
}

// close marks the buffer closed: readers that have caught up to the end of
// data now see io.EOF instead of blocking for more. Called once the
// process this buffer is attached to has exited.
func (b *outputBuf) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()

	b.cond.Broadcast()
}

// tail returns the last n bytes written so far (or everything, if fewer
// than n bytes have been written), for inclusion in a readiness-timeout
// error. Prefixed with a newline and label when non-empty; "" when nothing
// has been captured yet.
func (b *outputBuf) tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.data) == 0 {
		return ""
	}

	start := 0
	if len(b.data) > n {
		start = len(b.data) - n
	}

	return "\nrecent output:\n" + string(b.data[start:])
}

// NewReader returns an io.Reader over the buffer's content, starting from
// whatever has already been written and streaming new writes as they
// arrive until close is called, at which point it reaches EOF — the
// backend.Instance.Logs contract.
func (b *outputBuf) NewReader() io.Reader {
	return &outputBufReader{buf: b}
}

// outputBufReader is the io.Reader NewReader returns; pos tracks how far
// into buf.data this particular reader has consumed.
type outputBufReader struct {
	buf *outputBuf
	pos int
}

func (r *outputBufReader) Read(p []byte) (int, error) {
	r.buf.mu.Lock()
	defer r.buf.mu.Unlock()

	for r.pos >= len(r.buf.data) {
		if r.buf.closed {
			return 0, io.EOF
		}

		r.buf.cond.Wait()
	}

	n := copy(p, r.buf.data[r.pos:])
	r.pos += n

	return n, nil
}
