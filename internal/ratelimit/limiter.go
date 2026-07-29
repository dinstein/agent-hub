package ratelimit

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"time"
)

// Decision is the outcome of one admission check.
type Decision struct {
	// Allowed reports whether the call may proceed.
	Allowed bool
	// Rule is the ID of the rule that rejected the call ("" when allowed).
	Rule string
	// RetryAfter is how long until the rejecting rule has a token again.
	// It is always > 0 on a rejection: an agent that is told "retry after
	// 0" retries immediately and re-rejects, which is a hot loop.
	RetryAfter time.Duration
	// Degraded reports that the counter file could not be used (corrupt,
	// unreadable, unwritable) and the call was admitted WITHOUT being
	// counted. It is the observable half of "fail open, loudly".
	Degraded bool
}

// Event is the audit record of one enforcement decision. It carries
// identifiers only — never arguments, never payload (the audit rule of
// audit never records args).
type Event struct {
	Time       time.Time
	Key        Key
	Rule       string
	Allowed    bool
	RetryAfter time.Duration
	Degraded   bool
}

// Options configures New.
type Options struct {
	// Config is the rule set (governance key `rateLimits`). An empty
	// Config disables enforcement entirely.
	Config Config
	// StateDir is where the shared counter file lives, normally
	// <data>/state. Required unless Store is supplied.
	StateDir string
	// Store overrides StateDir (tests, and assemblies that already own a
	// Store).
	Store *Store
	// Logger receives the loud half of "fail open, loudly". nil =
	// slog.Default().
	Logger *slog.Logger
	// Now injects the clock (tests). nil = time.Now.
	Now func() time.Time
	// OnEvent receives one Event per DENIED or DEGRADED decision; the
	// assembling gateway forwards it to the audit stream. Allowed calls
	// are not reported: a quota that fires on every call would drown the
	// audit log in non-events. nil disables reporting.
	OnEvent func(Event)
}

// Limiter enforces a Config against the shared counter file.
type Limiter struct {
	cfg     Config
	store   *Store
	log     *slog.Logger
	now     func() time.Time
	onEvent func(Event)
}

// New validates the configuration and returns a Limiter.
//
// Configuration errors are fatal here (they are NOT failed open): a rule
// set that cannot be understood must be fixed, and starting with silently
// no quotas would look exactly like a working limiter with generous limits.
//
// Two more startup refusals share that direction, and both are conditional
// on rules actually being configured — a limiter with no rules is a no-op
// that never touches the filesystem:
//
//   - A build without a cross-process file lock cannot count correctly
//     (flock_stub.go). Enforcing a number that silently multiplies by the
//     number of gateway processes is worse than saying so.
//   - A counter file that cannot be locked, read or replaced RIGHT NOW is
//     probed for once, here, rather than discovered per call. Both are the
//     "a configuration that claims something must honour it or report it"
//     rule; a file that breaks LATER still fails open loudly (see Allow).
func New(opts Options) (*Limiter, error) {
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	if len(opts.Config.Rules) > 0 && !crossProcessLockSupported {
		return nil, fmt.Errorf(
			"ratelimit: %d rule(s) configured but this build (%s) has no cross-process file lock: "+
				"refusing to run a quota that would be under-counted once a second gateway process starts",
			len(opts.Config.Rules), runtime.GOOS)
	}
	store := opts.Store
	if store == nil {
		if opts.StateDir == "" {
			return nil, errors.New("ratelimit: Options needs StateDir or Store")
		}
		var err error
		if store, err = NewStore(opts.StateDir); err != nil {
			return nil, err
		}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	l := &Limiter{cfg: opts.Config, store: store, log: log, now: now, onEvent: opts.OnEvent}
	if l.Enabled() {
		if err := l.probe(); err != nil {
			return nil, fmt.Errorf("ratelimit: %d rule(s) configured but the counter file is unusable: %w",
				len(opts.Config.Rules), err)
		}
	}
	return l, nil
}

// probe runs one no-change read-decide-write cycle so an unusable counter
// file is reported at ASSEMBLY time instead of turning into a permanently
// degraded (uncounted) call path nobody looks at.
//
// Failure direction: fail closed — the caller refuses to start. A quota that
// cannot count at startup is a configuration claim this process cannot
// honour, and the operator is right there to fix it. Corruption is NOT a
// probe failure: it is recoverable (the file is quarantined and counting
// restarts from empty) and is reported through the same loud warning the
// call path uses.
func (l *Limiter) probe() error {
	return l.store.update(l.now(), l.warnCorrupt, func(*state) bool { return true })
}

// warnCorrupt is the single corruption reporter, shared by probe and Allow.
func (l *Limiter) warnCorrupt(err error, quarantined string) {
	l.log.Warn("ratelimit: counter file corrupt, restarting counters (quotas not enforced for this call)",
		"error", err, "path", l.store.Path(), "quarantined", quarantined)
}

// Enabled reports whether any rule is configured. Assemblies use it to skip
// wrapping calls entirely when quotas are off.
func (l *Limiter) Enabled() bool { return len(l.cfg.Rules) > 0 }

// Allow spends one token from every rule matching key, all or nothing.
//
// All or nothing is the point: if rule A has a token and rule B does not,
// spending A's token would charge a call that never happened, and a long
// enough rejection streak would starve A forever.
func (l *Limiter) Allow(key Key) Decision {
	rules := l.cfg.match(key)
	if len(rules) == 0 {
		return Decision{Allowed: true}
	}
	now := l.now()
	nowMs := now.UnixMilli()

	dec := Decision{Allowed: true}
	var corrupt error
	err := l.store.update(now,
		func(err error, quarantined string) {
			corrupt = err
			l.warnCorrupt(err, quarantined)
		},
		func(st *state) bool {
			// Pass 1: decide against the state just read from disk.
			balances := make(map[string]int64, len(rules))
			for _, r := range rules {
				capacity := int64(r.Limit) * tokenScale
				window := time.Duration(r.Window)
				name := r.counterKey(key)
				b, ok := st.Buckets[name]
				tokens := refill(b, ok, capacity, window, now)
				if tokens < tokenScale {
					dec.Allowed = false
					dec.Rule = r.ID()
					dec.RetryAfter = retryAfter(tokens, capacity, window)
					return false // nothing consumed, nothing written
				}
				balances[name] = tokens
			}
			// Pass 2: consume. Reached only when every rule had a token.
			for name, tokens := range balances {
				st.Buckets[name] = bucket{Tokens: tokens - tokenScale, Updated: nowMs}
			}
			return true
		})

	switch {
	case err != nil:
		// Lock or write failure: admit the call uncounted (fail open) and
		// say so. A broken counter file must not take every agent on the
		// machine offline.
		//
		// "And say so" is the load-bearing half. The state an attacker wants
		// from a limiter is a SILENT admission — counters unreadable, calls
		// flowing, nothing anywhere recording that the quota stopped
		// applying. So this branch always warns AND always emits an Event
		// with Degraded set (see below); an assembly wires both to its log
		// and audit sinks. A quota that never fires and a quota that is not
		// running must never look alike.
		l.log.Warn("ratelimit: counter file unusable, admitting call uncounted",
			"error", err, "path", l.store.Path(), "key", key.String())
		dec = Decision{Allowed: true, Degraded: true}
	case corrupt != nil:
		// The cycle recovered (a fresh file was written), but this call was
		// decided against an empty state — report it as degraded so the
		// audit trail shows the gap rather than implying enforcement.
		dec.Degraded = true
	}

	if !dec.Allowed || dec.Degraded {
		l.emit(Event{
			Time:       now,
			Key:        key,
			Rule:       dec.Rule,
			Allowed:    dec.Allowed,
			RetryAfter: dec.RetryAfter,
			Degraded:   dec.Degraded,
		})
	}
	return dec
}

func (l *Limiter) emit(ev Event) {
	if l.onEvent == nil {
		return
	}
	l.onEvent(ev)
}

// retryAfter is the time until the bucket holds one whole token again,
// rounded UP to the next millisecond and never zero: a retry hint of 0
// invites the immediate retry that produced the rejection.
func retryAfter(tokens, capacity int64, window time.Duration) time.Duration {
	missing := int64(tokenScale) - tokens
	if missing <= 0 {
		return time.Millisecond
	}
	windowMs := window.Milliseconds()
	// Inverse of the refill: tokens gained per ms = capacity/windowMs.
	ms := (missing*windowMs + capacity - 1) / capacity
	if ms < 1 {
		ms = 1
	}
	return time.Duration(ms) * time.Millisecond
}

// String renders a decision for logs.
func (d Decision) String() string {
	if d.Allowed {
		if d.Degraded {
			return "allowed (degraded)"
		}
		return "allowed"
	}
	return fmt.Sprintf("denied by %s, retry after %s", d.Rule, d.RetryAfter)
}
