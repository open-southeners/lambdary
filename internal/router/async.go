package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Invocation types of the X-Amz-Invocation-Type header.
const (
	invocationRequestResponse = "RequestResponse"
	invocationEvent           = "Event"
	invocationDryRun          = "DryRun"
)

// invocationTypeHeader names the invoke API's InvocationType parameter.
const invocationTypeHeader = "X-Amz-Invocation-Type"

// DefaultAsyncRetryDelays is the wait before each retry of a failed
// asynchronous invocation: one retry after a second and a second after two
// more. AWS retries twice as well, but waits about one minute and then about
// two minutes; Lambdary shortens the waits so a failing handler is seen to
// fail for good within a few seconds of local work. Set
// Options.AsyncRetryDelays to change either the waits or, by their count,
// the number of retries.
var DefaultAsyncRetryDelays = []time.Duration{time.Second, 2 * time.Second}

// errAsyncClosed reports an asynchronous invocation refused because the
// router is shutting down.
var errAsyncClosed = errors.New("router is shutting down")

// asyncTarget is a validated asynchronous invocation: what to run, with
// which payload, and how to count it.
type asyncTarget struct {
	ref      string
	target   string
	executed string
	payload  []byte
	meta     invocationMeta
}

// InvokeAsync validates ref (a name or ARN, optionally :qualified) as Invoke
// does and queues one asynchronous invocation of it with payload, returning
// once it is queued. The invocation then runs in the background under the
// router's own context, not ctx, and is retried as AWS retries an Event
// invoke when it fails (see runAsync). It serves in-process callers whose
// AWS counterpart invokes asynchronously, such as schedules.
func (rt *Router) InvokeAsync(_ context.Context, ref string, payload []byte) error {
	name, qualifier := parseFunctionRef(ref)

	target, executed, err := rt.resolveRef(name, qualifier, "")
	if err != nil {
		return err
	}

	return rt.enqueue(asyncTarget{
		ref:      ref,
		target:   target,
		executed: executed,
		payload:  emptyPayloadAsObject(payload),
		meta:     rt.invocationMeta(name, executed, qualifier, ""),
	})
}

// enqueue starts a in the background, unless the router was shut down.
func (rt *Router) enqueue(a asyncTarget) error {
	rt.asyncMu.Lock()
	defer rt.asyncMu.Unlock()

	if rt.baseCtx.Err() != nil {
		return errAsyncClosed
	}

	rt.async.Add(1)

	go func() {
		defer rt.async.Done()
		rt.runAsync(a)
	}()

	return nil
}

// runAsync runs a, retrying after each of the router's retry delays while
// attempts fail: a function error, or an upstream or timeout error. Every
// attempt records metrics as a normal invocation. The last failure is
// logged. Shutting the router down abandons the run, including a pending
// retry wait.
func (rt *Router) runAsync(a asyncTarget) {
	for attempt := 0; ; attempt++ {
		status, body, _, err := rt.invoke(rt.baseCtx, a.target, a.payload, a.meta)

		if err == nil && !isFunctionError(status, body) {
			return
		}

		if rt.baseCtx.Err() != nil {
			return
		}

		if attempt >= len(rt.retryDelays) {
			rt.logf("async invoke %s failed after %d attempts: %s", a.ref, attempt+1, failureReason(status, body, err))
			return
		}

		timer := time.NewTimer(rt.retryDelays[attempt])

		select {
		case <-timer.C:
		case <-rt.baseCtx.Done():
			timer.Stop()
			return
		}
	}
}

// failureReason describes a failed attempt for the log.
func failureReason(status int, body []byte, err error) string {
	if err != nil {
		return err.Error()
	}

	const limit = 200

	text := string(body)
	if len(text) > limit {
		text = text[:limit] + "..."
	}

	return fmt.Sprintf("function error (status %d): %s", status, text)
}

// Shutdown stops accepting asynchronous invocations, cancels the running
// ones and their pending retries, and waits for them to return or for ctx to
// end, whichever comes first. It is safe to call more than once.
func (rt *Router) Shutdown(ctx context.Context) error {
	rt.asyncMu.Lock()
	rt.cancel()
	rt.asyncMu.Unlock()

	done := make(chan struct{})
	go func() {
		rt.async.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// logf writes one line to the router's log, if it has one.
func (rt *Router) logf(format string, args ...any) {
	if rt.log == nil {
		return
	}

	rt.logMu.Lock()
	defer rt.logMu.Unlock()

	fmt.Fprintf(rt.log, format+"\n", args...)
}

// handleAsyncInvoke serves an Event invoke whose function and qualifier
// handleInvoke already validated: it reads the body, queues the invocation
// and answers 202 with no body, as AWS does.
func (rt *Router) handleAsyncInvoke(w http.ResponseWriter, r *http.Request, ref string, a asyncTarget) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"message": fmt.Sprintf("reading request body: %s", err),
		})
		return
	}

	a.ref = ref
	a.payload = emptyPayloadAsObject(body)

	if err := rt.enqueue(a); err != nil {
		writeAWSError(w, http.StatusServiceUnavailable, errService, err.Error())
		return
	}

	w.Header().Set("X-Amz-Executed-Version", a.executed)
	w.WriteHeader(http.StatusAccepted)
}
