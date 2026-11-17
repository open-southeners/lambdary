package router

import (
	"context"
	"testing"

	"github.com/open-southeners/lambdary/internal/metrics"
)

func TestEmptyPayloadReachesTheHandlerAsEmptyObject(t *testing.T) {
	tests := []struct {
		name, invocationType, body, want string
	}{
		{"sync empty", "", "", "{}"},
		{"async empty", "Event", "", "{}"},
		{"sync keeps whitespace", "", " ", " "},
		{"sync keeps payload", "", `null`, "null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newFakeBackend()
			rt := asyncRouter(b, metrics.New(), Options{AsyncRetryDelays: fastRetries})

			invokeWithType(rt, "greet", tt.invocationType, tt.body)

			waitUntil(t, func() bool { return b.instance("greet") != nil && b.instance("greet").invocationCount() == 1 }, "the invocation")

			if got := b.instance("greet").lastInvocationBody(); string(got) != tt.want {
				t.Errorf("handler received %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInProcessInvokeSubstitutesEmptyPayload(t *testing.T) {
	b := newFakeBackend()
	rt := asyncRouter(b, metrics.New(), Options{AsyncRetryDelays: fastRetries})

	if _, err := rt.Invoke(context.Background(), "greet", nil); err != nil {
		t.Fatal(err)
	}

	if got := b.instance("greet").lastInvocationBody(); string(got) != "{}" {
		t.Errorf("Invoke delivered %q, want {}", got)
	}

	if err := rt.InvokeAsync(context.Background(), "greet", []byte{}); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, func() bool { return b.instance("greet").invocationCount() == 2 }, "the async invocation")

	if got := b.instance("greet").lastInvocationBody(); string(got) != "{}" {
		t.Errorf("InvokeAsync delivered %q, want {}", got)
	}
}
