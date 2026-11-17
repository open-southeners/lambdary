// Package logbus fans function log lines out to live subscribers and keeps a
// bounded history per function, so `lambdary logs` can show recent output and
// follow new lines.
package logbus

import (
	"sort"
	"sync"
	"time"
)

// RingSize is how many lines the bus keeps per function.
const RingSize = 1000

// subscriberBuffer is how many live lines a subscriber may fall behind by
// before the bus starts dropping its lines.
const subscriberBuffer = 256

// Line is one log line of a function instance.
type Line struct {
	Function string    `json:"function"`
	Time     time.Time `json:"time"`
	Text     string    `json:"text"`
}

// entry is a buffered Line plus its publish sequence number, which orders
// lines across functions even when they share a clock tick.
type entry struct {
	line Line
	seq  uint64
}

// ring is a fixed-size circular buffer of entries.
type ring struct {
	buf   []entry
	start int
	n     int
}

func (r *ring) add(e entry) {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = e
		r.n++

		return
	}

	r.buf[r.start] = e
	r.start = (r.start + 1) % len(r.buf)
}

// entries returns the buffered entries, oldest first.
func (r *ring) entries() []entry {
	out := make([]entry, 0, r.n)
	for i := range r.n {
		out = append(out, r.buf[(r.start+i)%len(r.buf)])
	}

	return out
}

type subscriber struct {
	function string
	ch       chan Line
}

// Bus buffers and broadcasts log lines. Build one with New; the zero value is
// not usable. A Bus is safe for concurrent use.
type Bus struct {
	mu     sync.Mutex
	seq    uint64
	closed bool
	rings  map[string]*ring
	subs   map[*subscriber]struct{}
}

// New builds an empty Bus.
func New() *Bus {
	return &Bus{
		rings: make(map[string]*ring),
		subs:  make(map[*subscriber]struct{}),
	}
}

// Publish records one line of fn's output and delivers it to every matching
// subscriber. It never blocks: a subscriber whose buffer is full misses the
// line.
func (b *Bus) Publish(fn, text string) {
	line := Line{Function: fn, Time: time.Now(), Text: text}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++

	r, ok := b.rings[fn]
	if !ok {
		r = &ring{buf: make([]entry, RingSize)}
		b.rings[fn] = r
	}

	r.add(entry{line: line, seq: b.seq})

	for s := range b.subs {
		if s.function != "" && s.function != fn {
			continue
		}

		select {
		case s.ch <- line:
		default:
		}
	}
}

// Subscribe returns a channel that first yields up to tail buffered lines of
// fn, oldest first, then every line published afterwards. An empty fn means
// all functions, with the buffered lines in publish order. The returned
// cancel function unsubscribes and closes the channel; it is safe to call
// more than once.
func (b *Bus) Subscribe(fn string, tail int) (<-chan Line, func()) {
	b.mu.Lock()

	var history []entry

	for name, r := range b.rings {
		if fn == "" || fn == name {
			history = append(history, r.entries()...)
		}
	}

	sort.Slice(history, func(i, j int) bool { return history[i].seq < history[j].seq })

	if tail < 0 {
		tail = 0
	}

	if len(history) > tail {
		history = history[len(history)-tail:]
	}

	s := &subscriber{function: fn, ch: make(chan Line, len(history)+subscriberBuffer)}
	for _, e := range history {
		s.ch <- e.line
	}

	if b.closed {
		close(s.ch)
	} else {
		b.subs[s] = struct{}{}
	}
	b.mu.Unlock()

	var once sync.Once

	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, live := b.subs[s]; live {
				delete(b.subs, s)
				close(s.ch)
			}
			b.mu.Unlock()
		})
	}

	return s.ch, cancel
}

// Close ends every current subscription, closing their channels so followers
// return, and makes later Subscribe calls yield only buffered lines before
// closing. Publish still records lines. It is safe to call more than once.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.closed = true

	for s := range b.subs {
		close(s.ch)
		delete(b.subs, s)
	}
}
