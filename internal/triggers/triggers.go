// Package triggers runs the event sources a function's manifest declares in
// `events:` while `lambdary dev` serves: SQS queues polled into batches, as an
// event source mapping does, and EventBridge schedules.
package triggers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
	"github.com/open-southeners/lambdary/internal/queue"
	"github.com/open-southeners/lambdary/internal/schedule"
)

// Result is what an invocation returned.
type Result struct {
	Payload       []byte
	FunctionError bool
}

// Invoker runs one synchronous invocation of ref, a function name with an
// optional :qualifier.
type Invoker func(ctx context.Context, ref string, payload []byte) (Result, error)

// pollWait is the long-poll wait of each receive, SQS's maximum.
const pollWait = queue.MaxWait

// retryDelay paces a poller whose queue is missing or failing.
const retryDelay = time.Second

// Runner runs the triggers of one function set.
type Runner struct {
	// Invoke runs the functions.
	Invoke Invoker
	// Queues holds the queues SQS events read.
	Queues *queue.Store
	// Log receives one line per batch and per schedule fire.
	Log io.Writer
	// NoSchedules leaves schedules off, as `dev --no-schedules`.
	NoSchedules bool

	logMu sync.Mutex

	// Seams for tests.
	now      func() time.Time
	after    func(time.Duration) <-chan time.Time
	pollWait time.Duration
}

func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}

	return time.Now()
}

func (r *Runner) wait(d time.Duration) <-chan time.Time {
	if r.after != nil {
		return r.after(d)
	}

	return time.After(d)
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log == nil {
		return
	}

	r.logMu.Lock()
	defer r.logMu.Unlock()

	fmt.Fprintf(r.Log, format+"\n", args...)
}

// Start runs the triggers of every function in fns until the returned stop
// is called. stop waits for them to finish; an invocation in flight is
// cancelled, and messages it held reappear after their visibility timeout.
func (r *Runner) Start(ctx context.Context, fns []discovery.Function) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup

	for _, fn := range fns {
		if fn.Manifest == nil || fn.Version != "" {
			continue
		}

		for i, ev := range fn.Manifest.Events {
			switch {
			case ev.SQS != nil && ev.SQS.IsEnabled():
				wg.Go(func() { r.pollSQS(ctx, fn.Name, ev.SQS) })
			case ev.Schedule != nil && ev.Schedule.IsEnabled() && !r.NoSchedules:
				wg.Go(func() { r.runSchedule(ctx, fn.Name, i, ev.Schedule) })
			}
		}
	}

	return func() {
		cancel()
		wg.Wait()
	}
}

// ref names what an event invokes.
func ref(fn, qualifier string) string {
	if qualifier == "" {
		return fn
	}

	return fn + ":" + qualifier
}

// pollSQS feeds batches from ev's queue to fn until ctx ends.
func (r *Runner) pollSQS(ctx context.Context, fn string, ev *manifest.SQSEvent) {
	missing := false

	for ctx.Err() == nil {
		msgs, err := r.gather(ctx, ev)

		switch {
		case errors.Is(err, queue.ErrNotFound):
			if !missing {
				r.logf("sqs %s → %s: queue does not exist; waiting for it", ev.Queue, fn)
				missing = true
			}
		case err != nil:
			r.logf("sqs %s → %s: receiving: %s", ev.Queue, fn, err)
		default:
			missing = false

			if len(msgs) > 0 {
				r.deliver(ctx, fn, ev, msgs)
			}

			continue
		}

		select {
		case <-ctx.Done():
		case <-r.wait(retryDelay):
		}
	}
}

// gather receives one batch: whatever arrives first, then, with a batch
// window, more until the batch is full or the window closes.
func (r *Runner) gather(ctx context.Context, ev *manifest.SQSEvent) ([]queue.Message, error) {
	size := ev.BatchSize
	if size == 0 {
		size = manifest.DefaultBatchSize
	}

	wait := pollWait
	if r.pollWait > 0 {
		wait = r.pollWait
	}

	msgs, err := r.Queues.Receive(ctx, ev.Queue, queue.ReceiveInput{Max: min(size, queue.MaxReceiveBatch), Wait: &wait})
	if err != nil || len(msgs) == 0 || ev.BatchWindow == 0 {
		return msgs, err
	}

	deadline := r.clock().Add(time.Duration(ev.BatchWindow) * time.Second)

	for len(msgs) < size {
		remaining := deadline.Sub(r.clock())
		if remaining <= 0 {
			break
		}

		remaining = min(remaining, queue.MaxWait)

		more, err := r.Queues.Receive(ctx, ev.Queue, queue.ReceiveInput{Max: min(size-len(msgs), queue.MaxReceiveBatch), Wait: &remaining})
		if err != nil || ctx.Err() != nil {
			break
		}

		msgs = append(msgs, more...)
	}

	return msgs, nil
}

// deliver invokes fn with msgs as one SQS event and deletes the messages it
// handled. Messages it failed stay in the queue and come back after their
// visibility timeout, or move to the dead-letter queue.
func (r *Runner) deliver(ctx context.Context, fn string, ev *manifest.SQSEvent, msgs []queue.Message) {
	payload, err := json.Marshal(sqsEvent(ev.Queue, msgs))
	if err != nil {
		r.logf("sqs %s → %s: building the event: %s", ev.Queue, fn, err)
		return
	}

	res, err := r.Invoke(ctx, ref(fn, ev.Qualifier), payload)
	if ctx.Err() != nil {
		return
	}

	retry := fmt.Sprintf("%d message(s) will be retried after the visibility timeout", len(msgs))

	switch {
	case err != nil:
		r.logf("sqs %s → %s: invoke failed: %s; %s", ev.Queue, fn, err, retry)
		return
	case res.FunctionError:
		r.logf("sqs %s → %s: function error: %s; %s", ev.Queue, fn, errorMessage(res.Payload), retry)
		return
	}

	failed := map[string]bool{}

	if ev.ReportsBatchItemFailures() {
		failed, err = batchItemFailures(res.Payload, msgs)
		if err != nil {
			r.logf("sqs %s → %s: %s; %s", ev.Queue, fn, err, retry)
			return
		}
	}

	for _, m := range msgs {
		if failed[m.ID] {
			continue
		}

		if err := r.Queues.DeleteMessage(ev.Queue, m.ReceiptHandle); err != nil {
			r.logf("sqs %s → %s: deleting message %s: %s", ev.Queue, fn, m.ID, err)
		}
	}

	if len(failed) > 0 {
		r.logf("sqs %s → %s: %d message(s), %d failed and will be retried", ev.Queue, fn, len(msgs), len(failed))
		return
	}

	r.logf("sqs %s → %s: %d message(s)", ev.Queue, fn, len(msgs))
}

// batchItemFailures reads the IDs a ReportBatchItemFailures function failed,
// following AWS's rules: no response, or no or an empty batchItemFailures,
// means every message succeeded; an unparseable response, or an
// itemIdentifier that is empty or not in the batch, fails the whole batch.
func batchItemFailures(payload []byte, msgs []queue.Message) (map[string]bool, error) {
	failed := map[string]bool{}

	if len(payload) == 0 {
		return failed, nil
	}

	var resp *struct {
		BatchItemFailures []struct {
			ItemIdentifier *string `json:"itemIdentifier"`
		} `json:"batchItemFailures"`
	}

	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("the response is not a valid batchItemFailures response: %s", err)
	}

	if resp == nil {
		return failed, nil
	}

	inBatch := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		inBatch[m.ID] = true
	}

	for _, f := range resp.BatchItemFailures {
		if f.ItemIdentifier == nil || !inBatch[*f.ItemIdentifier] {
			return nil, errors.New("batchItemFailures names an itemIdentifier that is empty or not in the batch")
		}

		failed[*f.ItemIdentifier] = true
	}

	return failed, nil
}

// errorMessage pulls errorMessage out of a Lambda error envelope.
func errorMessage(payload []byte) string {
	var envelope struct {
		ErrorType    string `json:"errorType"`
		ErrorMessage string `json:"errorMessage"`
	}

	if json.Unmarshal(payload, &envelope) != nil || envelope.ErrorMessage == "" {
		return string(payload)
	}

	if envelope.ErrorType == "" {
		return envelope.ErrorMessage
	}

	return envelope.ErrorType + ": " + envelope.ErrorMessage
}

// runSchedule invokes fn each time ev's expression fires, until ctx ends or
// the expression never fires again. Fires missed while an invocation ran
// are skipped, not queued.
func (r *Runner) runSchedule(ctx context.Context, fn string, index int, ev *manifest.ScheduleEvent) {
	expr, err := schedule.Parse(ev.Expression)
	if err != nil {
		r.logf("schedule %s → %s: %s", ev.Expression, fn, err)
		return
	}

	loc, err := ev.Location()
	if err != nil {
		r.logf("schedule %s → %s: %s", ev.Expression, fn, err)
		return
	}

	rule := fmt.Sprintf("arn:aws:events:%s:%s:rule/%s-schedule-%d", queue.Region, queue.AccountID, fn, index)

	next, ok := expr.Next(r.clock(), loc)

	for ok {
		select {
		case <-ctx.Done():
			return
		case <-r.wait(next.Sub(r.clock())):
		}

		payload := []byte(ev.Input)
		if ev.Input == "" {
			payload, _ = json.Marshal(scheduledEvent(rule, next)) //nolint:errcheck // a struct of strings always marshals.
		}

		res, err := r.Invoke(ctx, ref(fn, ev.Qualifier), payload)

		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			r.logf("schedule %s → %s: invoke failed: %s", ev.Expression, fn, err)
		case res.FunctionError:
			r.logf("schedule %s → %s: function error: %s", ev.Expression, fn, errorMessage(res.Payload))
		default:
			r.logf("schedule %s → %s: invoked", ev.Expression, fn)
		}

		now := r.clock()
		for ok && !next.After(now) {
			next, ok = expr.Next(next, loc)
		}
	}
}
