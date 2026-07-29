package downstream

import (
	"errors"
	"testing"
	"time"
)

// fakeClock is a manual clock for deterministic cooldown tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newTestBreaker(cfg BreakerConfig) (*breaker, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	b := newBreaker(cfg)
	b.now = clk.now
	return b, clk
}

func mustAllow(t *testing.T, b *breaker, wantProbe bool) {
	t.Helper()
	probe, err := b.allow()
	if err != nil {
		t.Fatalf("allow: unexpected error %v", err)
	}
	if probe != wantProbe {
		t.Fatalf("allow: probe = %v, want %v", probe, wantProbe)
	}
}

func mustDeny(t *testing.T, b *breaker) {
	t.Helper()
	if _, err := b.allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("allow: err = %v, want ErrCircuitOpen", err)
	}
}

func TestBreakerDefaults(t *testing.T) {
	b := newBreaker(BreakerConfig{})
	if b.threshold != 3 || b.cooldown != 20*time.Second {
		t.Fatalf("defaults = (%d, %s), want (3, 20s)", b.threshold, b.cooldown)
	}
}

func TestBreakerOpensAfterThresholdConsecutiveFailures(t *testing.T) {
	b, _ := newTestBreaker(BreakerConfig{FailureThreshold: 3, Cooldown: time.Second})
	for range 2 {
		mustAllow(t, b, false)
		b.recordFailure()
	}
	// A success in between resets the streak.
	mustAllow(t, b, false)
	b.recordSuccess()
	for range 2 {
		mustAllow(t, b, false)
		b.recordFailure()
	}
	mustAllow(t, b, false) // still closed: only 2 consecutive failures
	b.recordFailure()      // third consecutive → open
	mustDeny(t, b)
}

func TestBreakerCooldownThenSingleProbe(t *testing.T) {
	b, clk := newTestBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Second})
	mustAllow(t, b, false)
	b.recordFailure() // open
	mustDeny(t, b)

	clk.advance(999 * time.Millisecond)
	mustDeny(t, b) // still cooling down

	clk.advance(time.Millisecond)
	mustAllow(t, b, true) // half-open: this caller is the probe
	mustDeny(t, b)        // single probe: everyone else fails fast

	b.recordSuccess() // probe succeeded → closed
	mustAllow(t, b, false)
}

func TestBreakerProbeFailureReopensWithFreshCooldown(t *testing.T) {
	b, clk := newTestBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Second})
	mustAllow(t, b, false)
	b.recordFailure()
	clk.advance(time.Second)
	mustAllow(t, b, true)
	b.recordFailure() // probe failed → open again, cooldown restarts
	mustDeny(t, b)
	clk.advance(999 * time.Millisecond)
	mustDeny(t, b)
	clk.advance(time.Millisecond)
	mustAllow(t, b, true)
}

func TestBreakerReleaseProbeAllowsNextProbeImmediately(t *testing.T) {
	b, clk := newTestBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Second})
	mustAllow(t, b, false)
	b.recordFailure()
	clk.advance(time.Second)
	mustAllow(t, b, true)
	b.releaseProbe()      // neutral outcome (ctx cancel): no verdict
	mustAllow(t, b, true) // next caller probes right away, no new cooldown
}

func TestBreakerStragglerFailureWhileOpenKeepsCooldownWindow(t *testing.T) {
	b, clk := newTestBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Second})
	mustAllow(t, b, false)
	b.recordFailure() // open at t0
	clk.advance(900 * time.Millisecond)
	b.recordFailure() // straggler admitted earlier: must NOT extend cooldown
	clk.advance(100 * time.Millisecond)
	mustAllow(t, b, true) // original window elapsed → probe
}

func TestBreakerFatalDoesNotCount(t *testing.T) {
	// ClassFatal outcomes are recorded as successes by the owner; this
	// test pins the breaker-side behavior (recordSuccess resets streak).
	b, _ := newTestBreaker(BreakerConfig{FailureThreshold: 2, Cooldown: time.Second})
	mustAllow(t, b, false)
	b.recordFailure()
	mustAllow(t, b, false)
	b.recordSuccess() // e.g. an ordinary JSON-RPC error response
	mustAllow(t, b, false)
	b.recordFailure() // streak restarted: 1 of 2
	mustAllow(t, b, false)
}
