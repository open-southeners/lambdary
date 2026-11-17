package logbus

import (
	"fmt"
	"testing"
	"time"
)

func drain(ch <-chan Line, n int) []Line {
	var out []Line

	for range n {
		select {
		case l := <-ch:
			out = append(out, l)
		case <-time.After(time.Second):
			return out
		}
	}

	return out
}

func TestRingEviction(t *testing.T) {
	b := New()

	for i := range RingSize + 5 {
		b.Publish("a", fmt.Sprint(i))
	}

	ch, cancel := b.Subscribe("a", RingSize*2)
	defer cancel()

	got := drain(ch, RingSize)
	if len(got) != RingSize || got[0].Text != "5" || got[RingSize-1].Text != fmt.Sprint(RingSize+4) {
		t.Fatalf("got %d lines, first %q last %q", len(got), got[0].Text, got[len(got)-1].Text)
	}

	select {
	case l := <-ch:
		t.Fatalf("unexpected extra line %v", l)
	default:
	}
}

func TestTailOrderingAcrossFunctions(t *testing.T) {
	b := New()
	b.Publish("a", "1")
	b.Publish("b", "2")
	b.Publish("a", "3")
	b.Publish("b", "4")

	ch, cancel := b.Subscribe("", 3)
	defer cancel()

	got := drain(ch, 3)
	if len(got) != 3 || got[0].Text != "2" || got[1].Text != "3" || got[2].Text != "4" {
		t.Fatalf("got %v", got)
	}

	chA, cancelA := b.Subscribe("a", 10)
	defer cancelA()

	if got := drain(chA, 2); len(got) != 2 || got[0].Text != "1" || got[1].Text != "3" {
		t.Fatalf("function filter got %v", got)
	}

	chZero, cancelZero := b.Subscribe("", 0)
	defer cancelZero()

	select {
	case l := <-chZero:
		t.Fatalf("tail 0 yielded %v", l)
	default:
	}
}

func TestLiveDelivery(t *testing.T) {
	b := New()
	b.Publish("a", "old")

	ch, cancel := b.Subscribe("a", 1)
	defer cancel()

	b.Publish("b", "other")
	b.Publish("a", "new")

	got := drain(ch, 2)
	if len(got) != 2 || got[0].Text != "old" || got[1].Text != "new" || got[1].Function != "a" || got[1].Time.IsZero() {
		t.Fatalf("got %v", got)
	}
}

func TestSlowSubscriberDropsInsteadOfBlocking(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("", 0)
	defer cancel()

	done := make(chan struct{})

	go func() {
		for i := range subscriberBuffer * 3 {
			b.Publish("a", fmt.Sprint(i))
		}

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}

	if n := len(ch); n != subscriberBuffer {
		t.Fatalf("buffered %d lines, want %d", n, subscriberBuffer)
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("", 0)

	cancel()
	cancel()

	if _, ok := <-ch; ok {
		t.Fatal("channel still open after cancel")
	}

	b.Publish("a", "after") // must not panic on the closed subscriber
}
