package downstream

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// The circuit breaker is the one mechanism in this package that can reject
// every call to a server without any of them reaching the code that logs.
// For the cooldown's whole duration the caller is failed inside enqueue,
// ahead of the owner queue — so before this file, "the server rejected
// everything for twenty seconds and then recovered" produced not one line,
// and the only nearby evidence (a respawn) belongs to a different path that
// need not have run at all. These tests pin the three transitions.

// stateLog captures records with their attributes flattened to strings.
type stateLog struct {
	mu   sync.Mutex
	recs []map[string]string
	msgs []string
}

func (h *stateLog) Enabled(context.Context, slog.Level) bool { return true }

func (h *stateLog) Handle(_ context.Context, r slog.Record) error {
	fields := map[string]string{"msg": r.Message, "level": r.Level.String()}
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, fields)
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *stateLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *stateLog) WithGroup(string) slog.Handler      { return h }

// transitions returns the from→to pairs in the order they were logged.
func (h *stateLog) transitions() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.recs {
		if r["from"] == "" && r["to"] == "" {
			continue
		}
		out = append(out, r["from"]+"→"+r["to"])
	}
	return out
}

func (h *stateLog) find(t *testing.T, from, to string) map[string]string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r["from"] == from && r["to"] == to {
			return r
		}
	}
	t.Fatalf("no record for %s→%s; got %v", from, to, h.msgs)
	return nil
}

func newLoggedBreaker(cfg BreakerConfig) (*breaker, *fakeClock, *stateLog) {
	h := &stateLog{}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	b := newBreaker(cfg, slog.New(h))
	b.now = clk.now
	return b, clk, h
}

func TestBreakerLogsTheFullCycle(t *testing.T) {
	b, clk, h := newLoggedBreaker(BreakerConfig{FailureThreshold: 2, Cooldown: time.Second})

	mustAllow(t, b, false)
	b.recordFailure() // 1 of 2: still closed, nothing to say
	if got := h.transitions(); len(got) != 0 {
		t.Fatalf("a failure below the threshold logged %v, want nothing", got)
	}

	mustAllow(t, b, false)
	b.recordFailure() // threshold reached → open
	clk.advance(time.Second)
	mustAllow(t, b, true) // → half-open
	b.recordSuccess()     // → closed

	want := []string{"closed→open", "open→half-open", "half-open→closed"}
	got := h.transitions()
	if len(got) != len(want) {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", got, want)
		}
	}
}

// Opening is the transition that changes what every caller sees, so it is
// the one that must not be an Info line lost in the traffic — and it has to
// carry the cooldown, which is the answer to "for how long".
func TestBreakerOpeningWarnsAndNamesTheCooldown(t *testing.T) {
	b, _, h := newLoggedBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: 20 * time.Second})
	mustAllow(t, b, false)
	b.recordFailure()

	rec := h.find(t, "closed", "open")
	if rec["level"] != slog.LevelWarn.String() {
		t.Fatalf("opening logged at %s, want WARN", rec["level"])
	}
	if rec["cooldown"] != "20s" {
		t.Fatalf("cooldown = %q, want 20s", rec["cooldown"])
	}
	if rec["failures"] != "1" {
		t.Fatalf("failures = %q, want 1", rec["failures"])
	}
}

// A straggler failing while the circuit is already open is not a state
// change, and logging one per straggler would make an outage look like a
// storm of new outages.
func TestBreakerRepeatedFailuresWhileOpenLogOnce(t *testing.T) {
	b, _, h := newLoggedBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Second})
	mustAllow(t, b, false)
	b.recordFailure()
	for range 5 {
		b.recordFailure()
	}
	if got := h.transitions(); len(got) != 1 || got[0] != "closed→open" {
		t.Fatalf("transitions = %v, want exactly one closed→open", got)
	}
}

// releaseProbe is a neutral outcome — the caller went away — and leaves the
// breaker exactly where it was, so it must stay silent.
func TestBreakerNeutralProbeOutcomeIsSilent(t *testing.T) {
	b, clk, h := newLoggedBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Second})
	mustAllow(t, b, false)
	b.recordFailure()
	clk.advance(time.Second)
	mustAllow(t, b, true)
	before := len(h.transitions())
	b.releaseProbe()
	if got := h.transitions(); len(got) != before {
		t.Fatalf("releaseProbe logged %v, want nothing new", got[before:])
	}
}
