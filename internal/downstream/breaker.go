package downstream

import (
	"fmt"
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

// breaker implements the per-server circuit breaker: threshold consecutive
// health failures open it, cooldown gates the transition to half-open, and
// half-open admits exactly one probe at a time.
//
// The verdict (allow) is taken BEFORE a request is posted to the calls
// channel, so during cooldown callers fail fast and never queue. Outcome
// recording is serialized by the owner goroutine, but allow may be called
// concurrently by many callers — hence the mutex.
type breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time // test seam; time.Now in production

	mu       sync.Mutex
	state    breakerState
	failures int // consecutive health failures while closed
	openedAt time.Time
	probing  bool // a half-open probe is in flight
}

func newBreaker(cfg BreakerConfig) *breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 20 * time.Second
	}
	return &breaker{threshold: cfg.FailureThreshold, cooldown: cfg.Cooldown, now: time.Now}
}

// allow reports whether a call may proceed. probe is true when this call is
// the single half-open probe; the caller must later report the outcome via
// recordSuccess / recordFailure, or releaseProbe for a neutral outcome
// (context cancellation before completion).
func (b *breaker) allow() (probe bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		return false, nil
	case stateOpen:
		remain := b.cooldown - b.now().Sub(b.openedAt)
		if remain > 0 {
			return false, fmt.Errorf("%w (cooling down, retry in %s)", ErrCircuitOpen, remain.Round(time.Millisecond))
		}
		b.state = stateHalfOpen
		b.probing = true
		return true, nil
	default: // stateHalfOpen
		if b.probing {
			return false, fmt.Errorf("%w (half-open probe in flight)", ErrCircuitOpen)
		}
		b.probing = true
		return true, nil
	}
}

// recordSuccess closes the breaker and resets the failure streak. An
// ordinary error response (ClassFatal) is recorded here too: the server
// answered, which proves the connection is healthy.
func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = stateClosed
	b.failures = 0
	b.probing = false
}

// recordFailure records one health failure (ClassUnavailable). A failed
// half-open probe reopens with a fresh cooldown; the threshold'th
// consecutive failure while closed opens the breaker.
func (b *breaker) recordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
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
