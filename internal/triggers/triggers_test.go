package triggers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
	"github.com/open-southeners/lambdary/internal/queue"
)

// recorder is a fake Invoker that records every call and answers with
// respond.
type recorder struct {
	mu      sync.Mutex
	calls   []call
	respond func(ref string, payload []byte) (Result, error)
	notify  chan struct{}
}

type call struct {
	ref     string
	payload []byte
}

func newRecorder(respond func(string, []byte) (Result, error)) *recorder {
	return &recorder{respond: respond, notify: make(chan struct{}, 100)}
}

func (r *recorder) invoke(_ context.Context, ref string, payload []byte) (Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, call{ref, payload})
	r.mu.Unlock()

	// Never block: a schedule on an instant fake clock invokes in a tight
	// loop, and a full channel already wakes waitCalls.
	defer func() {
		select {
		case r.notify <- struct{}{}:
		default:
		}
	}()

	if r.respond == nil {
		return Result{Payload: []byte("null")}, nil
	}

	return r.respond(ref, payload)
}

func (r *recorder) waitCalls(t *testing.T, n int) []call {
	t.Helper()

	deadline := time.After(5 * time.Second)

	for {
		r.mu.Lock()
		got := append([]call(nil), r.calls...)
		r.mu.Unlock()

		if len(got) >= n {
			return got
		}

		select {
		case <-r.notify:
		case <-deadline:
			t.Fatalf("got %d invocations, want %d", len(got), n)
		}
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func fnWith(name string, events ...manifest.Event) discovery.Function {
	return discovery.Function{Name: name, Manifest: &manifest.Manifest{Events: events}}
}

func records(t *testing.T, payload []byte) []sqsRecord {
	t.Helper()

	var ev sqsEventPayload
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("decoding SQS event %s: %v", payload, err)
	}

	return ev.Records
}

func TestSQSDeliversBatchesAndDeletes(t *testing.T) {
	store := queue.NewStore()
	if err := store.Ensure("jobs", queue.Settings{}); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{"a", "b", "c"} {
		if _, err := store.Send("jobs", queue.SendInput{Body: body}); err != nil {
			t.Fatal(err)
		}
	}

	rec := newRecorder(nil)
	log := &syncBuffer{}
	r := &Runner{Invoke: rec.invoke, Queues: store, Log: log, pollWait: 50 * time.Millisecond}

	stop := r.Start(context.Background(), []discovery.Function{
		fnWith("worker", manifest.Event{SQS: &manifest.SQSEvent{Queue: "jobs", Qualifier: "live"}}),
	})
	defer stop()

	got := rec.waitCalls(t, 1)

	if got[0].ref != "worker:live" {
		t.Errorf("invoked %q, want worker:live", got[0].ref)
	}

	recs := records(t, got[0].payload)
	if len(recs) != 3 {
		t.Fatalf("batch has %d records, want 3", len(recs))
	}

	r0 := recs[0]
	if r0.Body != "a" || r0.EventSource != "aws:sqs" || r0.EventSourceARN != queue.ARN("jobs") || r0.Attributes["ApproximateReceiveCount"] != "1" {
		t.Errorf("first record = %+v", r0)
	}

	waitFor(t, func() bool {
		info, _ := store.Info("jobs")
		return info.Visible+info.InFlight == 0
	}, "the delivered messages to be deleted")

	if !strings.Contains(log.String(), "sqs jobs → worker: 3 message(s)") {
		t.Errorf("log = %q", log.String())
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSQSPartialFailuresRetryThenDeadLetter(t *testing.T) {
	store := queue.NewStore()

	ev := &manifest.SQSEvent{
		Queue:                 "jobs",
		FunctionResponseTypes: []string{manifest.ReportBatchItemFailures},
	}

	// A zero visibility timeout makes failed messages return at once.
	if err := store.Ensure("jobs", queue.Settings{VisibilityTimeout: time.Nanosecond, DeadLetterQueue: "jobs-dlq", MaxReceiveCount: 2}); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{"ok", "poison"} {
		if _, err := store.Send("jobs", queue.SendInput{Body: body}); err != nil {
			t.Fatal(err)
		}
	}

	rec := newRecorder(func(_ string, payload []byte) (Result, error) {
		var failures []map[string]string

		var ev sqsEventPayload
		json.Unmarshal(payload, &ev) //nolint:errcheck // the test sends valid events.

		for _, rec := range ev.Records {
			if rec.Body == "poison" {
				failures = append(failures, map[string]string{"itemIdentifier": rec.MessageID})
			}
		}

		out, _ := json.Marshal(map[string]any{"batchItemFailures": failures}) //nolint:errcheck
		return Result{Payload: out}, nil
	})

	r := &Runner{Invoke: rec.invoke, Queues: store, pollWait: 20 * time.Millisecond}
	stop := r.Start(context.Background(), []discovery.Function{fnWith("worker", manifest.Event{SQS: ev})})
	defer stop()

	waitFor(t, func() bool {
		info, _ := store.Info("jobs-dlq")
		return info.Visible == 1
	}, "the poison message to reach the dead-letter queue")

	stop()

	// poison was delivered exactly max_receive_count times.
	poison := 0
	for _, c := range rec.waitCalls(t, 1) {
		for _, r := range records(t, c.payload) {
			if r.Body == "poison" {
				poison++
			}
		}
	}

	if poison != 2 {
		t.Errorf("poison was delivered %d times, want 2", poison)
	}

	dead, err := store.Receive(context.Background(), "jobs-dlq", queue.ReceiveInput{Max: 10})
	if err != nil || len(dead) != 1 || dead[0].Body != "poison" {
		t.Errorf("dead-letter queue holds %+v, %v; want poison only", dead, err)
	}

	if info, _ := store.Info("jobs"); info.Visible+info.InFlight+info.Delayed != 0 {
		t.Errorf("jobs still holds messages: %+v", info)
	}
}

func TestSQSFunctionErrorKeepsTheBatch(t *testing.T) {
	store := queue.NewStore()
	if err := store.Ensure("jobs", queue.Settings{VisibilityTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Send("jobs", queue.SendInput{Body: "x"}); err != nil {
		t.Fatal(err)
	}

	rec := newRecorder(func(string, []byte) (Result, error) {
		return Result{Payload: []byte(`{"errorType":"Boom","errorMessage":"exploded"}`), FunctionError: true}, nil
	})

	log := &syncBuffer{}
	r := &Runner{Invoke: rec.invoke, Queues: store, Log: log, pollWait: 20 * time.Millisecond}

	stop := r.Start(context.Background(), []discovery.Function{fnWith("worker", manifest.Event{SQS: &manifest.SQSEvent{Queue: "jobs"}})})
	rec.waitCalls(t, 1)
	stop()

	if info, _ := store.Info("jobs"); info.InFlight != 1 {
		t.Errorf("after a function error, jobs = %+v, want the message in flight", info)
	}

	if !strings.Contains(log.String(), "function error: Boom: exploded") {
		t.Errorf("log = %q", log.String())
	}
}

func TestBatchItemFailures(t *testing.T) {
	msgs := []queue.Message{{ID: "a"}, {ID: "b"}}

	tests := []struct {
		name    string
		payload string
		want    []string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"null", "null", nil, false},
		{"no field", `{}`, nil, false},
		{"empty list", `{"batchItemFailures":[]}`, nil, false},
		{"one failure", `{"batchItemFailures":[{"itemIdentifier":"b"}]}`, []string{"b"}, false},
		{"unknown id", `{"batchItemFailures":[{"itemIdentifier":"z"}]}`, nil, true},
		{"null id", `{"batchItemFailures":[{"itemIdentifier":null}]}`, nil, true},
		{"bad key", `{"batchItemFailures":[{"id":"a"}]}`, nil, true},
		{"not an object", `"ok"`, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := batchItemFailures([]byte(tt.payload), msgs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}

			if len(got) != len(tt.want) {
				t.Fatalf("failed = %v, want %v", got, tt.want)
			}

			for _, id := range tt.want {
				if !got[id] {
					t.Errorf("failed = %v, want %s in it", got, id)
				}
			}
		})
	}
}

func TestSQSWaitsForMissingQueue(t *testing.T) {
	store := queue.NewStore()
	rec := newRecorder(nil)
	log := &syncBuffer{}

	r := &Runner{Invoke: rec.invoke, Queues: store, Log: log, pollWait: 20 * time.Millisecond}
	r.after = func(time.Duration) <-chan time.Time { return time.After(10 * time.Millisecond) }

	stop := r.Start(context.Background(), []discovery.Function{fnWith("worker", manifest.Event{SQS: &manifest.SQSEvent{Queue: "later"}})})
	defer stop()

	waitFor(t, func() bool { return strings.Contains(log.String(), "queue does not exist") }, "the missing-queue notice")

	if err := store.Create("later", queue.Settings{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Send("later", queue.SendInput{Body: "hi"}); err != nil {
		t.Fatal(err)
	}

	rec.waitCalls(t, 1)

	if n := strings.Count(log.String(), "queue does not exist"); n != 1 {
		t.Errorf("missing-queue notice logged %d times, want once", n)
	}
}

func TestScheduleFiresScheduledEvents(t *testing.T) {
	base := time.Date(2026, 11, 5, 10, 0, 30, 0, time.UTC)

	var (
		mu  sync.Mutex
		now = base
	)

	rec := newRecorder(nil)
	r := &Runner{Invoke: rec.invoke, Log: &syncBuffer{}}
	r.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()

		ch := make(chan time.Time, 1)
		ch <- now
		return ch
	}

	stop := r.Start(context.Background(), []discovery.Function{
		fnWith("dispatch",
			manifest.Event{Schedule: &manifest.ScheduleEvent{Expression: "cron(* * * * ? *)"}},
			manifest.Event{Schedule: &manifest.ScheduleEvent{Expression: "rate(1 minute)", Input: `{"custom":true}`, Enabled: new(bool)}},
		),
	})

	got := rec.waitCalls(t, 3)
	stop()

	for i, c := range got[:3] {
		if c.ref != "dispatch" {
			t.Errorf("call %d invoked %q", i, c.ref)
		}

		var ev scheduledEventPayload
		if err := json.Unmarshal(c.payload, &ev); err != nil {
			t.Fatalf("call %d payload %s: %v", i, c.payload, err)
		}

		want := base.Truncate(time.Minute).Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339)
		if ev.Time != want || ev.DetailType != "Scheduled Event" || ev.Source != "aws.events" {
			t.Errorf("call %d event = %+v, want a Scheduled Event at %s", i, ev, want)
		}

		if len(ev.Resources) != 1 || !strings.HasSuffix(ev.Resources[0], ":rule/dispatch-schedule-0") {
			t.Errorf("call %d resources = %v", i, ev.Resources)
		}
	}
}

func TestScheduleInputAndNoSchedules(t *testing.T) {
	rec := newRecorder(nil)
	r := &Runner{Invoke: rec.invoke}
	r.after = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	fns := []discovery.Function{fnWith("dispatch", manifest.Event{Schedule: &manifest.ScheduleEvent{Expression: "rate(1 minute)", Input: `{"custom":true}`}})}

	stop := r.Start(context.Background(), fns)
	got := rec.waitCalls(t, 1)
	stop()

	if string(got[0].payload) != `{"custom":true}` {
		t.Errorf("payload = %s, want the configured input", got[0].payload)
	}

	off := newRecorder(nil)
	r = &Runner{Invoke: off.invoke, NoSchedules: true}
	r.after = func(time.Duration) <-chan time.Time { return time.After(0) }

	stop = r.Start(context.Background(), fns)
	time.Sleep(50 * time.Millisecond)
	stop()

	if len(off.calls) != 0 {
		t.Errorf("NoSchedules still invoked %d times", len(off.calls))
	}
}

func TestStopCancelsInFlightInvoke(t *testing.T) {
	store := queue.NewStore()
	if err := store.Ensure("jobs", queue.Settings{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Send("jobs", queue.SendInput{Body: "slow"}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	r := &Runner{
		Queues:   store,
		pollWait: 20 * time.Millisecond,
		Invoke: func(ctx context.Context, _ string, _ []byte) (Result, error) {
			close(started)
			<-ctx.Done()
			return Result{}, errors.New("cancelled")
		},
	}

	stop := r.Start(context.Background(), []discovery.Function{fnWith("worker", manifest.Event{SQS: &manifest.SQSEvent{Queue: "jobs"}})})
	<-started

	done := make(chan struct{})
	go func() { stop(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not cancel the in-flight invocation")
	}
}
