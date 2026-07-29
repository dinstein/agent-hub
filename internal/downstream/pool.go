package downstream

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/logx"
)

// Pool owns the DERIVED instances of downstream servers (docs/modules/dataplane.md):
// the registry keyed by (serverID, DeriveKey) whose base entry — the empty
// key — is not stored here at all but supplied by the caller.
//
// Why a separate object rather than a map inside the gateway: a derived
// instance is a full *Server, so it already has its own circuit breaker,
// its own serialized call queue and its own connection state. Isolation
// therefore needs no new mechanism — it needs a lifecycle, and that is
// exactly what this type is:
//
//   - LAZY: an instance is dialed on its first Acquire, inside that
//     caller's context, through the same Deps (spawn guard, secrets, SSRF
//     screening) as the base connection. A configured derivation that is
//     never used costs nothing.
//   - REFERENCE COUNTED with a DELAYED close: Release does not close, it
//     starts the idle clock. Sweep closes what has been idle for IdleTTL.
//     An agent alternating between two roots must not respawn a process on
//     every switch.
//   - CAPPED: at most MaxPerServer derivations per server. Over the cap
//     Acquire returns the BASE instance with Lease.Fallback set and logs a
//     warning — degraded sharing beats an unbounded process fan-out, and
//     the flag makes the degradation observable rather than mysterious.
//   - CASCADING: CloseKey drops every instance of one derivation at once
//     (a dead session takes its instances with it).
//
// Failure direction: a derivation that cannot connect is an ERROR, never a
// silent fallback to the base instance. Falling back would run the call
// with the wrong cwd/env/credential — precisely the isolation the operator
// asked for. Only the cap (an operator-set limit, not a failure) falls back.
type Pool struct {
	deps    Deps
	connect func(ctx context.Context, spec Spec) (*Server, error)
	now     func() time.Time
	log     *slog.Logger

	maxPerServer int
	idleTTL      time.Duration

	sweepStop chan struct{}
	sweepDone chan struct{}
	sweepOnce sync.Once

	mu       sync.Mutex
	closed   bool
	inst     map[string]map[DeriveKey]*derived
	overflow uint64 // cap-fallback count (metrics and tests)
}

// Sentinel errors of the pool.
var (
	// ErrPoolClosed is returned by Acquire after Close.
	ErrPoolClosed = errors.New("downstream: instance pool closed")
	// ErrNoBaseInstance reports an Acquire that would have to fall back to
	// the base instance while the caller supplied none.
	ErrNoBaseInstance = errors.New("downstream: no base instance available")
)

// Pool defaults (docs/modules/dataplane.md: 30 min idle reclaim on the session reaper's
// cadence). MaxPerServer is a fan-out guard, not a tuning knob: four live
// variants of one server is already an unusual amount of state.
const (
	DefaultMaxDerivedPerServer  = 4
	DefaultDerivedIdleTTL       = 30 * time.Minute
	DefaultDerivedSweepInterval = 5 * time.Minute
)

// PoolOptions configures NewPool. Every field has a production default.
type PoolOptions struct {
	// Deps is handed to Connect for every derived instance. AuthFor (not
	// Auth) is what gives a derivation its own credential.
	Deps Deps
	// Connect overrides instance creation (tests, in-process fakes). nil
	// selects downstream.Connect over Deps.
	Connect func(ctx context.Context, spec Spec) (*Server, error)
	// MaxPerServer caps derived instances per server (0 = default 4).
	MaxPerServer int
	// IdleTTL is how long an unreferenced instance survives (0 = 30 min).
	IdleTTL time.Duration
	// SweepInterval drives the background reclaim ticker. 0 selects the
	// default; a NEGATIVE value disables the goroutine entirely and leaves
	// reclaim to explicit Sweep calls (tests, or a host that already owns a
	// reaper tick).
	SweepInterval time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
	// Log receives instance lifecycle events. nil discards.
	Log *slog.Logger
}

// derived is one pooled instance. ready is closed exactly once, by the
// goroutine that created the entry, after srv/err are set; every other
// acquirer waits on it instead of dialing a second connection.
type derived struct {
	serverID  string
	key       DeriveKey
	spec      Spec
	ready     chan struct{}
	srv       *Server
	err       error
	refs      int
	idleSince time.Time
	createdAt time.Time
	closed    bool
}

// NewPool builds a pool and starts its reclaim ticker (unless disabled).
func NewPool(opts PoolOptions) *Pool {
	p := &Pool{
		deps:         opts.Deps,
		connect:      opts.Connect,
		now:          opts.Now,
		log:          opts.Log,
		maxPerServer: opts.MaxPerServer,
		idleTTL:      opts.IdleTTL,
		inst:         make(map[string]map[DeriveKey]*derived),
	}
	if p.connect == nil {
		p.connect = func(ctx context.Context, spec Spec) (*Server, error) {
			return Connect(ctx, spec, p.deps)
		}
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.maxPerServer <= 0 {
		p.maxPerServer = DefaultMaxDerivedPerServer
	}
	if p.idleTTL <= 0 {
		p.idleTTL = DefaultDerivedIdleTTL
	}
	interval := opts.SweepInterval
	if interval == 0 {
		interval = DefaultDerivedSweepInterval
	}
	if interval > 0 {
		p.sweepStop = make(chan struct{})
		p.sweepDone = make(chan struct{})
		go p.runSweeper(interval)
	}
	return p
}

// Lease is one borrowed instance. Release MUST be called when the call
// completes — it is what starts the idle clock; a leaked lease pins a
// process forever. Release is idempotent.
type Lease struct {
	// Server is the instance to issue the call on. Never nil on success.
	Server *Server
	// Key is the derivation this lease was requested for ("" = base).
	Key DeriveKey
	// Derived reports whether Server is a derived instance. It is false for
	// a base lease AND for a cap fallback — see Fallback to tell them apart.
	Derived bool
	// Fallback reports the cap-overflow case: the derivation was requested
	// but the per-server limit was reached, so the BASE instance is served.
	// The caller may surface it; correctness does not depend on it.
	Fallback bool

	pool *Pool
	inst *derived
	once sync.Once
}

// Release returns the lease. Idempotent and safe on the zero value.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.pool != nil && l.inst != nil {
			l.pool.release(l.inst)
		}
	})
}

// Acquire returns the instance that must execute a call on server spec.ID
// for derivation key.
//
// key == "" returns base unchanged: no bookkeeping, no lifecycle, no cost —
// which is why a server without a derive policy pays nothing for this
// package existing (docs/modules/dataplane.md: "a scope with no patch naturally shares the base instance").
//
// base may be nil when the caller has no base connection (a daemon that
// only ever dials derived instances); the cap fallback then fails with
// ErrNoBaseInstance rather than silently succeeding against nothing.
func (p *Pool) Acquire(ctx context.Context, base *Server, spec Spec, key DeriveKey) (*Lease, error) {
	if key == "" {
		if base == nil {
			return nil, fmt.Errorf("%w for server %q", ErrNoBaseInstance, spec.ID)
		}
		return &Lease{Server: base}, nil
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	byKey := p.inst[spec.ID]
	d := byKey[key]
	if d == nil {
		if len(byKey) >= p.maxPerServer {
			p.overflow++
			p.mu.Unlock()
			if base == nil {
				return nil, fmt.Errorf("%w: server %q is at its derived-instance cap (%d)",
					ErrNoBaseInstance, spec.ID, p.maxPerServer)
			}
			// Degraded but serving: the call runs on the shared instance,
			// which means it does NOT get the derived cwd/env. Loud on
			// purpose — this is a configuration problem, not a hiccup.
			p.log.Warn("derived instance cap reached; reusing the base instance",
				logx.Server(spec.ID), "derive_key", string(key), "cap", p.maxPerServer)
			return &Lease{Server: base, Key: key, Fallback: true}, nil
		}
		d = &derived{
			serverID:  spec.ID,
			key:       key,
			spec:      spec,
			ready:     make(chan struct{}),
			refs:      1,
			createdAt: p.now(),
		}
		if byKey == nil {
			byKey = make(map[DeriveKey]*derived)
			p.inst[spec.ID] = byKey
		}
		byKey[key] = d
		p.mu.Unlock()
		return p.dial(ctx, d)
	}
	d.refs++
	d.idleSince = time.Time{}
	p.mu.Unlock()

	select {
	case <-d.ready:
	case <-ctx.Done():
		p.release(d)
		return nil, context.Cause(ctx)
	}
	if d.err != nil {
		// The creator's dial failed; its entry was already removed, so the
		// next Acquire retries instead of caching the failure forever.
		p.release(d)
		return nil, d.err
	}
	if d.closedNow(p) {
		p.release(d)
		return nil, ErrPoolClosed
	}
	return &Lease{Server: d.srv, Key: key, Derived: true, pool: p, inst: d}, nil
}

// dial creates the connection of a freshly registered entry. It runs
// OUTSIDE p.mu (a spawn takes seconds) and publishes the outcome by closing
// d.ready exactly once.
func (p *Pool) dial(ctx context.Context, d *derived) (*Lease, error) {
	srv, err := p.connect(ctx, d.spec)

	p.mu.Lock()
	d.srv, d.err = srv, err
	close(d.ready)
	switch {
	case err != nil:
		d.refs--
		p.dropLocked(d)
		p.mu.Unlock()
		return nil, err
	case p.closed || d.closed:
		// Close/CloseKey raced this dial: the entry is already gone from the
		// map, so nothing else can reach this connection — close it here or
		// leak the process.
		d.refs--
		p.mu.Unlock()
		srv.Close()
		return nil, ErrPoolClosed
	}
	p.mu.Unlock()

	p.log.Info("derived downstream instance connected",
		logx.Server(d.serverID), "derive_key", string(d.key),
		"tools", len(srv.Tools()), "cwd", d.spec.Cwd)
	return &Lease{Server: srv, Key: d.key, Derived: true, pool: p, inst: d}, nil
}

// release drops one reference and, at zero, starts the idle clock. It never
// closes: the delayed close is the whole point (see Pool doc).
func (p *Pool) release(d *derived) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d.refs > 0 {
		d.refs--
	}
	if d.refs == 0 && d.idleSince.IsZero() {
		d.idleSince = p.now()
	}
}

func (d *derived) closedNow(p *Pool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed || d.closed
}

// dropLocked removes an entry from the registry. Caller holds p.mu.
func (p *Pool) dropLocked(d *derived) {
	byKey := p.inst[d.serverID]
	if byKey[d.key] == d {
		delete(byKey, d.key)
	}
	if len(byKey) == 0 {
		delete(p.inst, d.serverID)
	}
}

// Sweep closes every unreferenced instance whose idle time has reached
// IdleTTL and returns how many it closed. It is exported so a host with its
// own reaper tick (the daemon's session reaper) can drive reclamation
// instead of running a second ticker.
func (p *Pool) Sweep() int {
	now := p.now()
	var victims []*derived
	p.mu.Lock()
	for _, byKey := range p.inst {
		for _, d := range byKey {
			if d.refs > 0 || d.idleSince.IsZero() || d.closed {
				continue
			}
			if now.Sub(d.idleSince) < p.idleTTL {
				continue
			}
			d.closed = true
			p.dropLocked(d)
			victims = append(victims, d)
		}
	}
	p.mu.Unlock()

	for _, d := range victims {
		p.closeInstance(d, "idle")
	}
	return len(victims)
}

// CloseKey closes every instance of one derivation across all servers and
// returns how many it closed — the cascade of a dying session or a
// vanished root.
//
// It closes even REFERENCED instances: the session those calls belong to is
// gone, so waiting for them would keep a process alive for a client that
// can no longer receive the answer. In-flight callers see ErrServerClosed.
func (p *Pool) CloseKey(key DeriveKey) int {
	if key == "" {
		return 0
	}
	var victims []*derived
	p.mu.Lock()
	for _, byKey := range p.inst {
		if d := byKey[key]; d != nil && !d.closed {
			d.closed = true
			p.dropLocked(d)
			victims = append(victims, d)
		}
	}
	p.mu.Unlock()

	for _, d := range victims {
		p.closeInstance(d, "cascade")
	}
	return len(victims)
}

// CloseServer closes every derived instance of one server (its registry
// entry changed or was removed — the base connection is the caller's to
// close). Returns how many were closed.
func (p *Pool) CloseServer(serverID string) int {
	var victims []*derived
	p.mu.Lock()
	for _, d := range p.inst[serverID] {
		if !d.closed {
			d.closed = true
			victims = append(victims, d)
		}
	}
	delete(p.inst, serverID)
	p.mu.Unlock()

	for _, d := range victims {
		p.closeInstance(d, "server changed")
	}
	return len(victims)
}

// Close tears down the pool: the sweeper stops, every instance is closed,
// and further Acquires fail with ErrPoolClosed. Idempotent; blocks until
// every owner goroutine has exited.
func (p *Pool) Close() {
	p.sweepOnce.Do(func() {
		if p.sweepStop != nil {
			close(p.sweepStop)
			<-p.sweepDone
		}
	})
	var victims []*derived
	p.mu.Lock()
	p.closed = true
	for _, byKey := range p.inst {
		for _, d := range byKey {
			if !d.closed {
				d.closed = true
				victims = append(victims, d)
			}
		}
	}
	p.inst = make(map[string]map[DeriveKey]*derived)
	p.mu.Unlock()

	for _, d := range victims {
		p.closeInstance(d, "shutdown")
	}
}

// closeInstance closes one instance's connection. A entry whose dial never
// finished has no connection to close; the dialing goroutine sees d.closed
// and closes what it produced.
func (p *Pool) closeInstance(d *derived, reason string) {
	select {
	case <-d.ready:
	default:
		return // still dialing; dial() cleans up when it publishes
	}
	if d.srv != nil {
		d.srv.Close()
	}
	p.log.Info("derived downstream instance closed",
		logx.Server(d.serverID), "derive_key", string(d.key), "reason", reason)
}

func (p *Pool) runSweeper(interval time.Duration) {
	defer close(p.sweepDone)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-p.sweepStop:
			return
		case <-t.C:
			p.Sweep()
		}
	}
}

// InstanceInfo is the observability projection of one derived instance: which
// server it belongs to, which derive key produced it, and whether anything
// still holds it.
//
// It has no non-test caller today. `server ls`, the GUI and doctor are where
// it was meant to surface and none of them reach it, so this type describes a
// capability that exists at the pool layer and is not switched on in the
// assembled product — the same distinction docs/modules/security.md draws for
// the integrity store, and the reason that list is written down rather than
// assumed. Say so here rather than naming consumers as though they were
// wired: a comment that overstates what is switched on is how the pool's
// ordering guarantee came to be described as CLI contract with no CLI on the
// other end of it.
//
// What does exercise it: internal/downstream and internal/gateway tests, as
// the probe for "which instances are live", which is why the projection is
// worth keeping rather than deleting.
type InstanceInfo struct {
	ServerID  string
	Key       DeriveKey
	Refs      int
	CreatedAt time.Time
	// IdleSince is the zero time while the instance is referenced.
	IdleSince time.Time
	// Connecting reports an instance whose first dial has not finished.
	Connecting bool
}

// Instances snapshots the live derived instances, sorted by (server, key).
//
// The ordering is deterministic so that whatever eventually renders this can
// be golden-tested; no such consumer exists yet (see InstanceInfo). It is
// pinned by TestInstancesOrderIsDeterministic, because an ordering nothing
// reads is exactly the kind that rots unnoticed.
func (p *Pool) Instances() []InstanceInfo {
	p.mu.Lock()
	out := make([]InstanceInfo, 0, len(p.inst))
	for _, byKey := range p.inst {
		for _, d := range byKey {
			connecting := true
			select {
			case <-d.ready:
				connecting = false
			default:
			}
			out = append(out, InstanceInfo{
				ServerID:   d.serverID,
				Key:        d.key,
				Refs:       d.refs,
				CreatedAt:  d.createdAt,
				IdleSince:  d.idleSince,
				Connecting: connecting,
			})
		}
	}
	p.mu.Unlock()

	slices.SortFunc(out, func(a, b InstanceInfo) int {
		return cmp.Or(cmp.Compare(a.ServerID, b.ServerID), cmp.Compare(a.Key, b.Key))
	})
	return out
}

// Overflows reports how many Acquires fell back to the base instance
// because of the per-server cap.
func (p *Pool) Overflows() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.overflow
}
