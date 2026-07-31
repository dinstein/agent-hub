package downstream

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// breakerState is the classic three-state circuit breaker state.
type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// String names a state for the log. The three words are what a reader greps
// for, so they are part of the line's contract rather than a debug rendering.
func (s breakerState) String() string {
	switch s {
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// breaker implements the per-server circuit breaker: threshold consecutive
// health failures open it, cooldown gates the transition to half-open, and
// half-open admits exactly one probe at a time.
//
// The verdict (allow) is taken BEFORE a request is posted to the calls
// channel, so during cooldown callers fail fast and never queue. Outcome
// recording is serialized by the owner goroutine, but allow may be called
// concurrently by many callers — hence the mutex.
//
// Every state change is logged, because the breaker is otherwise the one
// thing in this package that can reject every call of a server while saying
// nothing: for the cooldown's duration each caller is failed at enqueue,
// before the owner queue and therefore before any other line this package
// writes. Read from the outside that is "the server broke for twenty seconds
// and healed", with a log that only ever mentions the respawns — a different
// event, on a different path, that does not have to happen at all.
type breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time // test seam; time.Now in production
	log       *slog.Logger

	mu       sync.Mutex
	state    breakerState
	failures int // consecutive health failures while closed
	openedAt time.Time
	probing  bool // a half-open probe is in flight
}

// newBreaker builds the breaker for one connection. log is the server's
// bound logger (it already carries the server, and the derived instance when
// there is one); nil means no logging, which is what the unit tests use.
func newBreaker(cfg BreakerConfig, log *slog.Logger) *breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 20 * time.Second
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &breaker{threshold: cfg.FailureThreshold, cooldown: cfg.Cooldown, now: time.Now, log: log}
}

// transition logs one state change. It is called AFTER the mutex is
// released — a handler writing to a file must never be reached while holding
// the lock every caller of allow() contends on — and is a no-op when the
// state did not actually move, so each caller can report unconditionally.
//
// Opening is a warning: from that instant every call to this server is
// rejected without reaching it. The other two are progress and go to Info.
func (b *breaker) transition(from, to breakerState, failures int) {
	if from == to {
		return
	}
	fields := []any{"from", from.String(), "to", to.String()}
	switch to {
	case stateOpen:
		b.log.Warn("circuit opened; calls to this server are rejected until the cooldown ends",
			append(fields, "failures", failures, "cooldown", b.cooldown.String())...)
	case stateHalfOpen:
		b.log.Info("circuit half-open; admitting one probe", fields...)
	default:
		b.log.Info("circuit closed; the server answered again", fields...)
	}
}

// allow reports whether a call may proceed. probe is true when this call is
// the single half-open probe; the caller must later report the outcome via
// recordSuccess / recordFailure, or releaseProbe for a neutral outcome
// (context cancellation before completion).
func (b *breaker) allow() (probe bool, err error) {
	b.mu.Lock()
	before := b.state
	switch b.state {
	case stateClosed:
		b.mu.Unlock()
		return false, nil
	case stateOpen:
		remain := b.cooldown - b.now().Sub(b.openedAt)
		if remain > 0 {
			b.mu.Unlock()
			return false, fmt.Errorf("%w (cooling down, retry in %s)", ErrCircuitOpen, remain.Round(time.Millisecond))
		}
		b.state = stateHalfOpen
		b.probing = true
		b.mu.Unlock()
		b.transition(before, stateHalfOpen, b.threshold)
		return true, nil
	default: // stateHalfOpen
		if b.probing {
			b.mu.Unlock()
			return false, fmt.Errorf("%w (half-open probe in flight)", ErrCircuitOpen)
		}
		b.probing = true
		b.mu.Unlock()
		return true, nil
	}
}

// recordSuccess closes the breaker and resets the failure streak. An
// ordinary error response (ClassFatal) is recorded here too: the server
// answered, which proves the connection is healthy.
func (b *breaker) recordSuccess() {
	b.mu.Lock()
	before := b.state
	b.state = stateClosed
	b.failures = 0
	b.probing = false
	b.mu.Unlock()
	b.transition(before, stateClosed, 0)
}

// recordFailure records one health failure (ClassUnavailable). A failed
// half-open probe reopens with a fresh cooldown; the threshold'th
// consecutive failure while closed opens the breaker.
func (b *breaker) recordFailure() {
	b.mu.Lock()
	before := b.state
	switch b.state {
	case stateHalfOpen:
		b.state = stateOpen
		b.openedAt = b.now()
		b.failures = b.threshold
		b.probing = false
	case stateClosed:
		b.failures++
		if b.failures >= b.threshold {
			b.state = stateOpen
			b.openedAt = b.now()
		}
	case stateOpen:
		// A straggler admitted before the breaker opened. Keep the existing
		// cooldown window: refreshing openedAt here would let a burst of
		// stragglers extend the outage indefinitely.
	}
	after, failures := b.state, b.failures
	b.mu.Unlock()
	b.transition(before, after, failures)
}

// releaseProbe reports a neutral probe outcome (caller's context cancelled
// before the call completed). The breaker stays half-open with no probe in
// flight, so the next caller may probe immediately.
func (b *breaker) releaseProbe() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == stateHalfOpen {
		b.probing = false
	}
}
