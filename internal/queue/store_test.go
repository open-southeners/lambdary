package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock for Store.now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestStore(t *testing.T) (*Store, *fakeClock) {
	t.Helper()

	clock := &fakeClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	s := NewStore()
	s.now = clock.now

	return s, clock
}

var noWait = func() *time.Duration { d := time.Duration(0); return &d }()

func receive(t *testing.T, s *Store, name string, max int) []Message {
	t.Helper()

	msgs, err := s.Receive(context.Background(), name, ReceiveInput{Max: max, Wait: noWait})
	if err != nil {
		t.Fatalf("Receive(%s): %v", name, err)
	}

	return msgs
}

func send(t *testing.T, s *Store, name string, bodies ...string) {
	t.Helper()

	for _, b := range bodies {
		if _, err := s.Send(name, SendInput{Body: b}); err != nil {
			t.Fatalf("Send(%s, %q): %v", name, b, err)
		}
	}
}

func TestVisibilityTimeoutHidesAndReturns(t *testing.T) {
	s, clock := newTestStore(t)
	if err := s.Ensure("jobs", Settings{VisibilityTimeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}

	send(t, s, "jobs", "a")

	got := receive(t, s, "jobs", 10)
	if len(got) != 1 || got[0].Body != "a" || got[0].ReceiveCount != 1 {
		t.Fatalf("first receive = %+v, want a once", got)
	}

	if again := receive(t, s, "jobs", 10); len(again) != 0 {
		t.Fatalf("receive while in flight = %+v, want nothing", again)
	}

	clock.advance(10 * time.Second)

	again := receive(t, s, "jobs", 10)
	if len(again) != 1 || again[0].ReceiveCount != 2 || again[0].ReceiptHandle == got[0].ReceiptHandle {
		t.Fatalf("receive after timeout = %+v, want a again with a new receipt", again)
	}

	// The first receipt is stale: deleting with it changes nothing.
	if err := s.DeleteMessage("jobs", got[0].ReceiptHandle); err != nil {
		t.Fatalf("stale DeleteMessage: %v", err)
	}
	if err := s.DeleteMessage("jobs", again[0].ReceiptHandle); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}

	clock.advance(time.Minute)

	if left := receive(t, s, "jobs", 10); len(left) != 0 {
		t.Fatalf("after delete = %+v, want empty", left)
	}
}

func TestReceiveRespectsMaxAndOrder(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Ensure("jobs", Settings{}); err != nil {
		t.Fatal(err)
	}

	send(t, s, "jobs", "1", "2", "3")

	got := receive(t, s, "jobs", 2)
	if len(got) != 2 || got[0].Body != "1" || got[1].Body != "2" {
		t.Fatalf("receive max 2 = %+v", got)
	}

	if rest := receive(t, s, "jobs", 10); len(rest) != 1 || rest[0].Body != "3" {
		t.Fatalf("second receive = %+v, want 3", rest)
	}
}

func TestRedriveToDeadLetterQueue(t *testing.T) {
	s, clock := newTestStore(t)
	if err := s.Ensure("jobs", Settings{VisibilityTimeout: time.Second, DeadLetterQueue: "jobs-dlq", MaxReceiveCount: 2}); err != nil {
		t.Fatal(err)
	}

	if !s.Exists("jobs-dlq") {
		t.Fatal("Ensure did not create the dead-letter queue")
	}

	send(t, s, "jobs", "poison")

	for i := 1; i <= 2; i++ {
		if got := receive(t, s, "jobs", 1); len(got) != 1 {
			t.Fatalf("receive %d = %+v, want the message", i, got)
		}
		clock.advance(time.Second)
	}

	if got := receive(t, s, "jobs", 1); len(got) != 0 {
		t.Fatalf("third receive = %+v, want the message moved away", got)
	}

	dead := receive(t, s, "jobs-dlq", 10)
	if len(dead) != 1 || dead[0].Body != "poison" {
		t.Fatalf("dead-letter queue = %+v, want poison", dead)
	}
}

func TestDelayedMessages(t *testing.T) {
	s, clock := newTestStore(t)
	if err := s.Ensure("jobs", Settings{}); err != nil {
		t.Fatal(err)
	}

	delay := 5 * time.Second
	if _, err := s.Send("jobs", SendInput{Body: "later", Delay: &delay}); err != nil {
		t.Fatal(err)
	}

	if got := receive(t, s, "jobs", 1); len(got) != 0 {
		t.Fatalf("receive before the delay = %+v", got)
	}

	info, _ := s.Info("jobs")
	if info.Delayed != 1 {
		t.Errorf("Info.Delayed = %d, want 1", info.Delayed)
	}

	clock.advance(delay)

	if got := receive(t, s, "jobs", 1); len(got) != 1 {
		t.Fatalf("receive after the delay = %+v", got)
	}
}

func TestLongPollWakesOnSend(t *testing.T) {
	s := NewStore()
	if err := s.Ensure("jobs", Settings{}); err != nil {
		t.Fatal(err)
	}

	wait := 5 * time.Second
	done := make(chan []Message)

	go func() {
		msgs, _ := s.Receive(context.Background(), "jobs", ReceiveInput{Max: 1, Wait: &wait})
		done <- msgs
	}()

	time.Sleep(50 * time.Millisecond)
	send(t, s, "jobs", "hi")

	select {
	case msgs := <-done:
		if len(msgs) != 1 || msgs[0].Body != "hi" {
			t.Fatalf("long poll got %+v", msgs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long poll did not wake on send")
	}
}

func TestLongPollEndsWithContext(t *testing.T) {
	s := NewStore()
	if err := s.Ensure("jobs", Settings{}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	wait := 20 * time.Second
	start := time.Now()

	msgs, err := s.Receive(ctx, "jobs", ReceiveInput{Max: 1, Wait: &wait})
	if err != nil || len(msgs) != 0 {
		t.Fatalf("Receive = %v, %v; want nothing", msgs, err)
	}

	if time.Since(start) > 2*time.Second {
		t.Fatal("Receive ignored the cancelled context")
	}
}

func TestChangeVisibility(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Ensure("jobs", Settings{}); err != nil {
		t.Fatal(err)
	}

	send(t, s, "jobs", "a")
	got := receive(t, s, "jobs", 1)

	if err := s.ChangeVisibility("jobs", got[0].ReceiptHandle, 0); err != nil {
		t.Fatalf("ChangeVisibility: %v", err)
	}

	if again := receive(t, s, "jobs", 1); len(again) != 1 {
		t.Fatalf("receive after making visible = %+v", again)
	}

	if err := s.ChangeVisibility("jobs", got[0].ReceiptHandle, 0); !errors.Is(err, ErrNotInflight) {
		t.Errorf("ChangeVisibility with a stale receipt = %v, want ErrNotInflight", err)
	}
}

func TestStoreErrors(t *testing.T) {
	s, _ := newTestStore(t)

	if _, err := s.Send("missing", SendInput{Body: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Send to a missing queue = %v, want ErrNotFound", err)
	}

	for _, st := range []Settings{
		{DeadLetterQueue: "dlq"},
		{MaxReceiveCount: 3},
		{VisibilityTimeout: 13 * time.Hour},
	} {
		if err := s.Ensure("jobs", st); !errors.Is(err, ErrInvalidParameter) {
			t.Errorf("Ensure(%+v) = %v, want ErrInvalidParameter", st, err)
		}
	}

	if err := s.Ensure("jobs.fifo", Settings{}); !errors.Is(err, ErrInvalidParameter) {
		t.Errorf("Ensure of a FIFO name = %v, want ErrInvalidParameter", err)
	}

	if err := s.Ensure("jobs", Settings{}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteMessage("jobs", "not-a-receipt"); !errors.Is(err, ErrInvalidReceipt) {
		t.Errorf("DeleteMessage with a bad receipt = %v, want ErrInvalidReceipt", err)
	}

	if _, err := s.Send("jobs", SendInput{}); !errors.Is(err, ErrInvalidParameter) {
		t.Errorf("Send of an empty body = %v, want ErrInvalidParameter", err)
	}
}

func TestAttributesMD5(t *testing.T) {
	v := "value"
	got := attributesMD5(map[string]Attribute{"attr": {DataType: "String", StringValue: &v}})

	if len(got) != 32 {
		t.Fatalf("attributesMD5 = %q, want a hex MD5", got)
	}

	if attributesMD5(nil) != "" {
		t.Error("attributesMD5(nil) should be empty")
	}

	// Order of insertion must not matter.
	w := "w"
	a := attributesMD5(map[string]Attribute{"a": {DataType: "String", StringValue: &v}, "b": {DataType: "Number", StringValue: &w}})
	b := attributesMD5(map[string]Attribute{"b": {DataType: "Number", StringValue: &w}, "a": {DataType: "String", StringValue: &v}})
	if a != b {
		t.Error("attributesMD5 depends on map order")
	}
}

func TestStopWaitingEndsLongPolls(t *testing.T) {
	s := NewStore()
	if err := s.Ensure("jobs", Settings{}); err != nil {
		t.Fatal(err)
	}

	wait := 20 * time.Second
	done := make(chan struct{})

	go func() {
		s.Receive(context.Background(), "jobs", ReceiveInput{Max: 1, Wait: &wait}) //nolint:errcheck
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	s.StopWaiting()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopWaiting did not end the long poll")
	}

	start := time.Now()
	if _, err := s.Receive(context.Background(), "jobs", ReceiveInput{Max: 1, Wait: &wait}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Error("a receive after StopWaiting still waited")
	}
}
