package process

import (
	"bufio"
	"io"
	"testing"
	"time"
)

func TestOutputBuf(t *testing.T) {
	t.Run("Read replays content written before the reader started", func(t *testing.T) {
		buf := newOutputBuf()
		buf.Write([]byte("hello\n")) //nolint:errcheck // outputBuf.Write never errors.
		buf.close()

		got, err := io.ReadAll(buf.NewReader())
		if err != nil {
			t.Fatalf("ReadAll() unexpected error: %v", err)
		}
		if string(got) != "hello\n" {
			t.Errorf("ReadAll() = %q, want %q", got, "hello\n")
		}
	})

	t.Run("Read blocks for new writes and unblocks on close", func(t *testing.T) {
		buf := newOutputBuf()
		r := bufio.NewReader(buf.NewReader())

		done := make(chan struct{})
		var line string
		var readErr error

		go func() {
			line, readErr = r.ReadString('\n')
			close(done)
		}()

		select {
		case <-done:
			t.Fatal("Read returned before anything was written")
		case <-time.After(50 * time.Millisecond):
		}

		buf.Write([]byte("later\n")) //nolint:errcheck // outputBuf.Write never errors.

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Read did not unblock after a write")
		}

		if readErr != nil {
			t.Fatalf("ReadString() unexpected error: %v", readErr)
		}
		if line != "later\n" {
			t.Errorf("ReadString() = %q, want %q", line, "later\n")
		}

		// A second read past the last write should now block until close,
		// then see EOF rather than hang forever.
		eof := make(chan error, 1)
		go func() {
			_, err := r.ReadByte()
			eof <- err
		}()

		select {
		case err := <-eof:
			t.Fatalf("ReadByte() returned %v before close", err)
		case <-time.After(50 * time.Millisecond):
		}

		buf.close()

		select {
		case err := <-eof:
			if err != io.EOF {
				t.Errorf("ReadByte() after close = %v, want io.EOF", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("ReadByte() did not unblock after close")
		}
	})

	t.Run("Write never blocks even with no reader", func(t *testing.T) {
		buf := newOutputBuf()

		done := make(chan struct{})
		go func() {
			for i := 0; i < 1000; i++ {
				buf.Write([]byte("some log line that nobody is reading\n")) //nolint:errcheck // outputBuf.Write never errors.
			}
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Write blocked with no reader attached")
		}
	})

	t.Run("tail returns the last n bytes", func(t *testing.T) {
		buf := newOutputBuf()
		buf.Write([]byte("0123456789")) //nolint:errcheck // outputBuf.Write never errors.

		if got := buf.tail(4); got != "\nrecent output:\n6789" {
			t.Errorf("tail(4) = %q, want %q", got, "\nrecent output:\n6789")
		}
		if got := buf.tail(100); got != "\nrecent output:\n0123456789" {
			t.Errorf("tail(100) = %q, want the whole buffer", got)
		}
	})

	t.Run("tail is empty for an untouched buffer", func(t *testing.T) {
		buf := newOutputBuf()

		if got := buf.tail(10); got != "" {
			t.Errorf("tail(10) = %q, want empty string", got)
		}
	})
}
