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
	resolve BackendResolver
	fns     map[string]discovery.Function

	// mapMu guards creation of entries in starts and invokeLocks (not the
	// mutexes themselves, which callers lock/unlock outside mapMu).
	mapMu       sync.Mutex
	starts      map[string]*sync.Mutex
	invokeLocks map[string]*sync.Mutex

	instMu    sync.Mutex
	instances map[string]backend.Instance

	// OnStart, when set, is called every time Ensure successfully starts a
	// fresh instance (never on a cache hit) — dev's log-streaming hook uses
	// it to start pumping the instance's Logs() without polling for new
	// instances. It is called outside every Manager lock, after the
	// instance is already visible to other Ensure calls, so it must not
	// assume exclusivity and should return quickly (spawn a goroutine for
	// any long-running work). Callers set it once, before Manager starts
	// serving requests, so no lock guards the field itself — see
	// plans/m4-dx.md Unit B.
	OnStart func(name string, inst backend.Instance)
}

// BackendResolver picks the backend.Backend that should run fn — see
// internal/cli's implementation (dev.go/backend_resolver.go), which honors
// a function's own local.backend hint (discovery.Function.Backend) instead
// of every function sharing whatever one backend `dev`/`invoke` happened to
// resolve, the gap CURRENT_ISSUES.md tracked as "`local.backend` is not
// honored per function".
type BackendResolver func(ctx context.Context, fn discovery.Function) (backend.Backend, error)

// staticResolver adapts a single backend.Backend into a BackendResolver
// that always returns it, regardless of fn — NewManager's behavior for
// callers that only ever run every function on one backend.
func staticResolver(b backend.Backend) BackendResolver {
	return func(context.Context, discovery.Function) (backend.Backend, error) {
		return b, nil
	}
}

// NewManager builds a Manager over a single backend b, shared by every
// function in fns regardless of its own local.backend hint — see
// NewManagerWithResolver for callers (namely `lambdary dev`) that need to
// honor per-function overrides instead.
func NewManager(b backend.Backend, fns []discovery.Function) *Manager {
	return NewManagerWithResolver(staticResolver(b), fns)
}

// NewManagerWithResolver builds a Manager that asks resolve for each
// function's backend.Backend individually (see BackendResolver), keyed by
// Function.Name. Functions with a duplicate Name (see discovery's
// Warnings) collapse to the last one in fns for Ensure's purposes; callers
// are expected to reject invocations of a colliding name before reaching
// Ensure (see router.go's name-collision handling), so which of the
// colliding functions Ensure would start never actually matters in
// practice.
func NewManagerWithResolver(resolve BackendResolver, fns []discovery.Function) *Manager {
	byName := make(map[string]discovery.Function, len(fns))
	for _, fn := range fns {
		byName[fn.Name] = fn
	}

	return &Manager{
		resolve:     resolve,
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

	m.instMu.Lock()
	inst, ok := m.instances[name]
	m.instMu.Unlock()
	if ok {
		mu.Unlock()
		return inst.InvokeURL(), nil
	}

	b, err := m.resolve(ctx, fn)
	if err != nil {
		mu.Unlock()
		return "", fmt.Errorf("router: resolving backend for %s: %w", name, err)
	}

	inst, err = b.Start(ctx, fn)
	if err != nil {
		mu.Unlock()
		return "", fmt.Errorf("router: starting %s: %w", name, err)
	}

	m.instMu.Lock()
	m.instances[name] = inst
	m.instMu.Unlock()

	mu.Unlock()

	if m.OnStart != nil {
		m.OnStart(name, inst)
	}

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

// Restart stops name's running instance (if any) and forgets it, so the
// next Ensure cold-starts a fresh one — the "code changed" half of hot
// reload (see plans/m4-dx.md Unit B): the caller (dev's watcher event loop)
// calls Restart on a function code change rather than tearing the whole
// server down. name must be a known function; unknown names report
// ErrUnknownFunction, same as Ensure.
//
// Restart takes both of name's mutexes, in the same order Ensure/WithLock
// would use them if nested: the start guard first (so no concurrent Ensure
// can race in and observe a half-torn-down instance, or start a new one
// that Restart then wrongly stops), then the invoke lock (so an in-flight
// invocation finishes against the old instance before Restart stops it).
// Both stay held for Restart's whole duration, including the Stop call
// itself, so the next Ensure for name is guaranteed to see it gone and
// start fresh rather than possibly racing ahead of the teardown.
func (m *Manager) Restart(ctx context.Context, name string) error {
	if _, ok := m.fns[name]; !ok {
		return fmt.Errorf("router: %w: %s", ErrUnknownFunction, name)
	}

	startMu := m.namedMutex(m.starts, name)
	startMu.Lock()
	defer startMu.Unlock()

	invokeMu := m.namedMutex(m.invokeLocks, name)
	invokeMu.Lock()
	defer invokeMu.Unlock()

	m.instMu.Lock()
	inst, ok := m.instances[name]
	if ok {
		delete(m.instances, name)
	}
	m.instMu.Unlock()

	if !ok {
		return nil
	}

	if err := inst.Stop(ctx); err != nil {
		return fmt.Errorf("router: restarting %s: %w", name, err)
	}

	return nil
}

// Discard stops and forgets name's cached instance, but only if it is still
// the one whose InvokeURL is invokeURL — the identity check keeps a caller
// that just watched an instance die from evicting a healthy replacement
// some other request already started in the meantime. See
// plans/dead-runtime-and-provided-container.md Unit A: the router calls
// Discard after an invoke reveals its instance is dead (a transport failure
// or a Runtime.ExitError envelope), so the next Ensure cold-starts a fresh
// one instead of the function being bricked until a code-change Restart.
//
// Unlike Restart, Discard takes only the start guard
// (m.namedMutex(m.starts, name)), never the invoke lock: the instance is
// presumed dead already, so blocking behind a doomed in-flight invoke (up to
// the function's own timeout) would only serialize recovery for nothing.
// Callers invoke Discard after releasing their own WithLock, once the dead
// invoke has already returned. name must be a known function; unknown names
// report ErrUnknownFunction, same as Ensure/Restart.
func (m *Manager) Discard(ctx context.Context, name, invokeURL string) error {
	if _, ok := m.fns[name]; !ok {
		return fmt.Errorf("router: %w: %s", ErrUnknownFunction, name)
	}

	startMu := m.namedMutex(m.starts, name)
	startMu.Lock()
	defer startMu.Unlock()

	m.instMu.Lock()
	inst, ok := m.instances[name]
	if ok && inst.InvokeURL() == invokeURL {
		delete(m.instances, name)
	} else {
		ok = false
	}
	m.instMu.Unlock()

	if !ok {
		return nil
	}

	if err := inst.Stop(ctx); err != nil {
		return fmt.Errorf("router: discarding %s: %w", name, err)
	}

	return nil
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
