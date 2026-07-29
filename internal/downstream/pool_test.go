package downstream_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// poolFixture builds a pool over in-process fake downstreams and reports
// every spec it was asked to connect (in order).
type poolFixture struct {
	mu      sync.Mutex
	dialed  []downstream.Spec
	failFor map[downstream.DeriveKey]error

	clock time.Time
	pool  *downstream.Pool
}

func newPoolFixture(t *testing.T, max int) *poolFixture {
	t.Helper()
	f := &poolFixture{
		failFor: map[downstream.DeriveKey]error{},
		clock:   time.Unix(1_700_000_000, 0),
	}
	f.pool = downstream.NewPool(downstream.PoolOptions{
		MaxPerServer: max,
		IdleTTL:      30 * time.Minute,
		// Negative: no background ticker — reclaim is driven explicitly so
		// the test never races a sweeper.
		SweepInterval: -1,
		Now:           func() time.Time { return f.now() },
		Connect: func(ctx context.Context, spec downstream.Spec) (*downstream.Server, error) {
			f.mu.Lock()
			f.dialed = append(f.dialed, spec)
			err := f.failFor[spec.DeriveKey]
			f.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return downstream.Connect(ctx, spec, downstream.Deps{
				Dial: func(context.Context, downstream.Spec) (transport.Transport, error) {
					return fakemcp.Connect(fakemcp.Minimal("echo"))
				},
			})
		},
	})
	t.Cleanup(f.pool.Close)
	return f
}

func (f *poolFixture) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *poolFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

func (f *poolFixture) dials() []downstream.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]downstream.Spec, len(f.dialed))
	copy(out, f.dialed)
	return out
}

// baseServer is a stand-in for the gateway's base connection.
func baseServer(t *testing.T, id string) *downstream.Server {
	t.Helper()
	s, err := downstream.Connect(context.Background(), downstream.Spec{ID: id}, downstream.Deps{
		Dial: func(context.Context, downstream.Spec) (transport.Transport, error) {
			return fakemcp.Connect(fakemcp.Minimal("echo"))
		},
	})
	if err != nil {
		t.Fatalf("base connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// TestPoolTwoRootsTwoInstances is the headline behaviour: the same server,
// two roots, two live processes — each dialed with its own cwd — while the
// route (server id) stays one.
func TestPoolTwoRootsTwoInstances(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t, 4)
	base := baseServer(t, "fs")
	spec := downstream.Spec{ID: "fs", Command: "srv", Cwd: "${ROOT}", Derive: downstream.DeriveRoot}

	k1 := downstream.RootDeriveKey("/w/one")
	k2 := downstream.RootDeriveKey("/w/two")
	l1, err := f.pool.Acquire(context.Background(), base, spec.Derived(k1, downstream.DeriveContext{Root: "/w/one"}), k1)
	if err != nil {
		t.Fatalf("acquire /w/one: %v", err)
	}
	l2, err := f.pool.Acquire(context.Background(), base, spec.Derived(k2, downstream.DeriveContext{Root: "/w/two"}), k2)
	if err != nil {
		t.Fatalf("acquire /w/two: %v", err)
	}
	if l1.Server == l2.Server {
		t.Fatal("two roots shared one instance")
	}
	if l1.Server == base || l2.Server == base {
		t.Fatal("a derived lease returned the base instance")
	}
	if !l1.Derived || l1.Fallback {
		t.Fatalf("lease flags = derived %v fallback %v", l1.Derived, l1.Fallback)
	}
	if l1.Server.ID() != "fs" || l2.Server.ID() != "fs" {
		t.Fatal("a derived instance must keep the base server id (RouteOf provenance)")
	}
	dials := f.dials()
	if len(dials) != 2 || dials[0].Cwd != "/w/one" || dials[1].Cwd != "/w/two" {
		t.Fatalf("dialed specs = %+v", dials)
	}
	if dials[0].ScopeName != string(k1) {
		t.Fatalf("vault scope name = %q, want %q", dials[0].ScopeName, k1)
	}

	// The same key again reuses the live instance: no second dial.
	l1b, err := f.pool.Acquire(context.Background(), base, spec.Derived(k1, downstream.DeriveContext{Root: "/w/one"}), k1)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if l1b.Server != l1.Server {
		t.Fatal("re-acquiring a live derivation dialed a second instance")
	}
	if len(f.dials()) != 2 {
		t.Fatalf("dial count = %d, want 2", len(f.dials()))
	}
	l1.Release()
	l1b.Release()
	l2.Release()

	// The empty key is the base instance and costs no bookkeeping at all.
	lb, err := f.pool.Acquire(context.Background(), base, spec, "")
	if err != nil {
		t.Fatalf("base acquire: %v", err)
	}
	if lb.Server != base || lb.Derived {
		t.Fatal("the empty key must return the base instance unchanged")
	}
	lb.Release()
}

// TestPoolCapReusesBase: over the per-server cap the call is served by the
// base instance, loudly (Fallback + Overflows), never by refusing service.
func TestPoolCapReusesBase(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t, 1)
	base := baseServer(t, "fs")
	spec := downstream.Spec{ID: "fs", Command: "srv", Derive: downstream.DeriveRoot}

	k1 := downstream.RootDeriveKey("/w/one")
	k2 := downstream.RootDeriveKey("/w/two")
	l1, err := f.pool.Acquire(context.Background(), base, spec.Derived(k1, downstream.DeriveContext{}), k1)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Release()

	l2, err := f.pool.Acquire(context.Background(), base, spec.Derived(k2, downstream.DeriveContext{}), k2)
	if err != nil {
		t.Fatalf("over-cap acquire must serve, not fail: %v", err)
	}
	defer l2.Release()
	if l2.Server != base || l2.Derived || !l2.Fallback {
		t.Fatalf("over-cap lease = %+v, want the base instance with Fallback set", l2)
	}
	if f.pool.Overflows() != 1 {
		t.Fatalf("Overflows = %d, want 1", f.pool.Overflows())
	}
	if len(f.dials()) != 1 {
		t.Fatalf("dial count = %d, want 1 (the over-cap key must not spawn)", len(f.dials()))
	}

	// With no base instance to fall back to, the cap is an error rather
	// than a silent success against nothing.
	if _, err := f.pool.Acquire(context.Background(), nil, spec.Derived(k2, downstream.DeriveContext{}), k2); !errors.Is(err, downstream.ErrNoBaseInstance) {
		t.Fatalf("error = %v, want ErrNoBaseInstance", err)
	}
}

// TestPoolIdleReclaim: a released instance survives until IdleTTL, is
// reclaimed after it, and a REFERENCED instance is never reclaimed.
func TestPoolIdleReclaim(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t, 4)
	base := baseServer(t, "fs")
	spec := downstream.Spec{ID: "fs", Command: "srv", Derive: downstream.DeriveSession}
	key := downstream.SessionDeriveKey("cursor:3")

	held, err := f.pool.Acquire(context.Background(), base, spec.Derived(key, downstream.DeriveContext{}), key)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	f.advance(time.Hour)
	if n := f.pool.Sweep(); n != 0 {
		t.Fatalf("swept %d referenced instances; a live lease must pin its instance", n)
	}

	held.Release()
	f.advance(10 * time.Minute) // released, but not yet idle for the TTL
	if n := f.pool.Sweep(); n != 0 {
		t.Fatalf("swept %d instances before the idle TTL elapsed", n)
	}
	if len(f.pool.Instances()) != 1 {
		t.Fatalf("instances = %+v, want the released one still live", f.pool.Instances())
	}

	f.advance(30 * time.Minute)
	if n := f.pool.Sweep(); n != 1 {
		t.Fatalf("swept %d instances after the TTL, want 1", n)
	}
	if len(f.pool.Instances()) != 0 {
		t.Fatalf("instances after reclaim = %+v", f.pool.Instances())
	}

	// Reclaim is not a tombstone: the next call re-dials.
	l, err := f.pool.Acquire(context.Background(), base, spec.Derived(key, downstream.DeriveContext{}), key)
	if err != nil {
		t.Fatalf("re-acquire after reclaim: %v", err)
	}
	l.Release()
	if len(f.dials()) != 2 {
		t.Fatalf("dial count = %d, want 2 (initial + re-dial)", len(f.dials()))
	}
}

// TestPoolCascadeAndFailures covers the two remaining lifecycle edges:
// CloseKey drops one derivation across servers, and a derivation that
// cannot connect is an error — never a silent fallback to the base
// instance, which would run with the wrong cwd/env.
func TestPoolCascadeAndFailures(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t, 4)
	base := baseServer(t, "fs")
	key := downstream.SessionDeriveKey("cursor:3")
	specA := downstream.Spec{ID: "fs", Command: "a"}.Derived(key, downstream.DeriveContext{})
	specB := downstream.Spec{ID: "gh", Command: "b"}.Derived(key, downstream.DeriveContext{})

	la, err := f.pool.Acquire(context.Background(), base, specA, key)
	if err != nil {
		t.Fatalf("acquire fs: %v", err)
	}
	lb, err := f.pool.Acquire(context.Background(), base, specB, key)
	if err != nil {
		t.Fatalf("acquire gh: %v", err)
	}
	if n := f.pool.CloseKey(key); n != 2 {
		t.Fatalf("CloseKey closed %d instances, want 2 (both servers)", n)
	}
	la.Release()
	lb.Release()
	if len(f.pool.Instances()) != 0 {
		t.Fatalf("instances after cascade = %+v", f.pool.Instances())
	}

	f.mu.Lock()
	f.failFor[key] = errors.New("spawn refused")
	f.mu.Unlock()
	if _, err := f.pool.Acquire(context.Background(), base, specA, key); err == nil {
		t.Fatal("a derivation that cannot connect must fail, not fall back to the base instance")
	}
	// A failed dial is not cached: the entry is gone, so the next attempt
	// tries again.
	f.mu.Lock()
	delete(f.failFor, key)
	f.mu.Unlock()
	l, err := f.pool.Acquire(context.Background(), base, specA, key)
	if err != nil {
		t.Fatalf("acquire after a failed dial: %v", err)
	}
	l.Release()
}

// TestPoolBreakerIsolation: each derived instance owns its circuit breaker
// and call queue, so a failing derivation cannot open the breaker of the
// base instance or of a sibling.
func TestPoolBreakerIsolation(t *testing.T) {
	t.Parallel()
	base := baseServer(t, "fs")
	// The derived instance's connection dies on its first tools/call; the
	// base one is healthy. Cooldown is an hour, so no half-open probe can
	// rescue it inside the test.
	script := fakemcp.Minimal("echo")
	script.Rules = []fakemcp.Rule{{
		Method:  "tools/call",
		Actions: []fakemcp.Action{{Kind: fakemcp.ActCrash}},
	}}
	pool := downstream.NewPool(downstream.PoolOptions{
		SweepInterval: -1,
		Connect: func(ctx context.Context, spec downstream.Spec) (*downstream.Server, error) {
			return downstream.Connect(ctx, spec, downstream.Deps{
				Breaker: downstream.BreakerConfig{FailureThreshold: 1, Cooldown: time.Hour},
				Dial: func(context.Context, downstream.Spec) (transport.Transport, error) {
					return fakemcp.Connect(script)
				},
			})
		},
	})
	t.Cleanup(pool.Close)

	key := downstream.SessionDeriveKey("s1")
	spec := downstream.Spec{ID: "fs"}.Derived(key, downstream.DeriveContext{})
	l, err := pool.Acquire(context.Background(), base, spec, key)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer l.Release()

	if _, err := l.Server.Call(context.Background(), "echo", nil); err == nil {
		t.Fatal("the crashing derived instance answered a call")
	}
	if _, err := l.Server.Call(context.Background(), "echo", nil); !errors.Is(err, downstream.ErrCircuitOpen) {
		t.Fatalf("derived breaker did not open: %v", err)
	}
	// The base instance is untouched: separate *Server, separate breaker,
	// separate call queue.
	if _, err := base.Call(context.Background(), "echo", nil); err != nil {
		t.Fatalf("a derived instance's failure leaked into the base instance: %v", err)
	}
}
