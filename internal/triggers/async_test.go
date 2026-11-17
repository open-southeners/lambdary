package triggers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

func TestScheduleUsesInvokeAsyncWhenSet(t *testing.T) {
	direct := newRecorder(nil)

	var (
		mu     sync.Mutex
		queued []call
	)

	logs := &syncBuffer{}
	r := &Runner{
		Invoke: direct.invoke,
		Log:    logs,
		InvokeAsync: func(_ context.Context, ref string, payload []byte) error {
			mu.Lock()
			queued = append(queued, call{ref, payload})
			n := len(queued)
			mu.Unlock()

			if n == 1 {
				return errors.New("function not found")
			}

			return nil
		},
	}
	r.after = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	fns := []discovery.Function{fnWith("dispatch", manifest.Event{Schedule: &manifest.ScheduleEvent{Expression: "rate(1 minute)", Input: `{"custom":true}`, Qualifier: "live"}})}

	stop := r.Start(context.Background(), fns)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(queued) >= 2 }, "two queued invocations")
	stop()

	mu.Lock()
	defer mu.Unlock()

	if queued[0].ref != "dispatch:live" || string(queued[0].payload) != `{"custom":true}` {
		t.Errorf("queued %+v, want dispatch:live with the configured input", queued[0])
	}

	direct.mu.Lock()
	defer direct.mu.Unlock()

	if len(direct.calls) != 0 {
		t.Errorf("schedule used the synchronous Invoke %d times", len(direct.calls))
	}

	if !strings.Contains(logs.String(), "invoke failed: function not found") || !strings.Contains(logs.String(), "queued") {
		t.Errorf("log = %q, want the enqueue failure and a queued line", logs.String())
	}
}
