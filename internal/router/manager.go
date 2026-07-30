// Package router implements Lambdary's local HTTP server, per DESIGN.md's
// "router" component and "Routing & event mapping" section: lazy per-function
// backend lifecycle (Manager), the AWS-compatible invoke passthrough, and
// HTTP↔event mapping on function routes (the `ANY /{route}/*` Function URL
// shape, via internal/event).
package router

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
)

// defaultInvokeTimeout is the invoke deadline used when a function's
// manifest leaves timeout unset. DESIGN.md's `.lambda.yml` spec documents
// timeout as optional with no fixed default, so Manager picks 30s — AWS
// Lambda's own console default — to give the queue+invoke path a bound.
const defaultInvokeTimeout = 30 * time.Second

// ErrUnknownFunction indicates Ensure was called with a name that does not
// match any discovered function. Wrapped with %w so callers can match it
// with errors.Is regardless of the name in the message.
var ErrUnknownFunction = errors.New("unknown function")

// Manager owns the lazy, per-function lifecycle of backend instances:
// starting an instance on first use (Ensure), serializing invocations
// against it (WithLock), and tearing everything down (StopAll). It holds no
// global state — every dependency is injected via NewManager.
//
// Two independent kinds of per-function mutex are involved, deliberately
// kept separate so a long-running invoke on one function never blocks
// Ensure (or an invoke) on another:
//
//   - a start guard, serializing concurrent Ensure calls for the same
//     function so its backend starts exactly once;
//   - an invoke lock, serializing concurrent invocations against the same
//     function's instance, per DESIGN.md's "the RIE processes one
//     invocation at a time per instance" constraint.
type Manager struct {
	backend backend.Backend
	fns     map[string]discovery.Function

	// mapMu guards creation of entries in starts and invokeLocks (not the
	// mutexes themselves, which callers lock/unlock outside mapMu).
	mapMu       sync.Mutex
	starts      map[string]*sync.Mutex
	invokeLocks map[string]*sync.Mutex

	instMu    sync.Mutex
	instances map[string]backend.Instance
}

// NewManager builds a Manager over b for the discovered functions fns,
// keyed by Function.Name. Functions with a duplicate Name (see discovery's
// Warnings) collapse to the last one in fns for Ensure's purposes; callers
// are expected to reject invocations of a colliding name before reaching
// Ensure (see router.go's name-collision handling), so which of the
// colliding functions Ensure would start never actually matters in
// practice.
func NewManager(b backend.Backend, fns []discovery.Function) *Manager {
	byName := make(map[string]discovery.Function, len(fns))
	for _, fn := range fns {
		byName[fn.Name] = fn
	}

	return &Manager{
		backend:     b,
		fns:         byName,
		starts:      make(map[string]*sync.Mutex),
		invokeLocks: make(map[string]*sync.Mutex),
		instances:   make(map[string]backend.Instance),
	}
}

// Ensure returns the invoke URL of name's running backend instance,
// starting one on first use. Concurrent Ensure calls for the same name
// wait for the first to finish rather than starting duplicate instances; a
// failed start is not cached, so the next Ensure call (from this request or
// a later one) retries from scratch instead of failing forever.
func (m *Manager) Ensure(ctx context.Context, name string) (string, error) {
	fn, ok := m.fns[name]
	if !ok {
		return "", fmt.Errorf("router: %w: %s", ErrUnknownFunction, name)
	}

	mu := m.namedMutex(m.starts, name)
	mu.Lock()
	defer mu.Unlock()

	m.instMu.Lock()
	inst, ok := m.instances[name]
	m.instMu.Unlock()
	if ok {
		return inst.InvokeURL(), nil
	}

	inst, err := m.backend.Start(ctx, fn)
	if err != nil {
		return "", fmt.Errorf("router: starting %s: %w", name, err)
	}

	m.instMu.Lock()
	m.instances[name] = inst
	m.instMu.Unlock()

	return inst.InvokeURL(), nil
}

// WithLock runs fn while holding name's invoke lock, serializing
// invocations against a single function's instance per DESIGN.md's "one
// invocation at a time" constraint. It is independent of the start guard
// Ensure uses, so a slow invoke on name never blocks Ensure (or WithLock)
// for a different function.
func (m *Manager) WithLock(name string, fn func() error) error {
	mu := m.namedMutex(m.invokeLocks, name)
	mu.Lock()
	defer mu.Unlock()

	return fn()
}

// InvokeTimeout returns the invoke deadline for name: its manifest's
// timeout when set, else defaultInvokeTimeout. Unknown names also return
// the default — Ensure is the one responsible for rejecting those.
func (m *Manager) InvokeTimeout(name string) time.Duration {
	fn, ok := m.fns[name]
	if !ok || fn.Manifest == nil || fn.Manifest.Timeout == 0 {
		return defaultInvokeTimeout
	}

	return time.Duration(fn.Manifest.Timeout) * time.Second
}

// StopAll stops every instance started so far, collecting per-instance
// errors with errors.Join rather than stopping at the first failure so one
// stuck container doesn't strand the rest running.
func (m *Manager) StopAll(ctx context.Context) error {
	m.instMu.Lock()
	instances := make([]backend.Instance, 0, len(m.instances))
	for name, inst := range m.instances {
		instances = append(instances, inst)
		delete(m.instances, name)
	}
	m.instMu.Unlock()

	var errs []error
	for _, inst := range instances {
		if err := inst.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// namedMutex returns the per-name mutex in set, creating it on first
// access. set is either m.starts or m.invokeLocks; mapMu only guards the
// map lookup/insert, not the returned mutex's own Lock/Unlock.
func (m *Manager) namedMutex(set map[string]*sync.Mutex, name string) *sync.Mutex {
	m.mapMu.Lock()
	defer m.mapMu.Unlock()

	mu, ok := set[name]
	if !ok {
		mu = &sync.Mutex{}
		set[name] = mu
	}

	return mu
}
