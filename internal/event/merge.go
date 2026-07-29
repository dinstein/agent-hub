package event

import (
	"sync"
	"time"
)

// Merger windows.
const (
	// CoalesceWindow is the default coalescer window: a change storm is
	// merged into one event at most this much later than its first change.
	CoalesceWindow = 50 * time.Millisecond
	// SettleWindow is the default settled-debounce window: one terminal
	// event fires after the stream has been quiet this long.
	SettleWindow = 750 * time.Millisecond
)

// timerFactory starts a one-shot timer that runs fn after d on its own
// goroutine. The returned stop reports whether it prevented fn from running
// (time.AfterFunc semantics). Injected by tests for determinism.
type timerFactory func(d time.Duration, fn func()) (stop func() bool)

func afterFunc(d time.Duration, fn func()) (stop func() bool) {
	return time.AfterFunc(d, fn).Stop
}

// Merger merges per-key event storms into single events with lazily built
// payloads. Two modes share the implementation:
//
//   - Coalescer (NewCoalescer): the window is anchored at the FIRST Add of a
//     key — K adds within the window become one event, published at most one
//     window after the first add (throttle; bounded latency).
//   - Settler (NewSettler): every Add RESETS the window — one terminal
//     "settled" event fires only after the key has been quiet for a full
//     window (debounce; replaces a whole lifecycle event stream).
//
// The payload is built lazily: only the LATEST builder passed to Add is
// invoked, exactly once, at fire time (K bursts build the
// expensive payload once). Builders therefore must capture state by
// reference or be cheap closures; they run on the timer goroutine (or the
// Flush caller) without any Merger lock held.
type Merger struct {
	publish func(Event)
	window  time.Duration
	reset   bool // true = settler (Add resets the timer)
	timer   timerFactory

	mu      sync.Mutex
	pending map[mergeKey]*mergeEntry
	closed  bool
}

type mergeKey struct {
	topic Topic
	key   string
}

type mergeEntry struct {
	build func() any
	stop  func() bool
	// gen guards the settler reset race: a timer that already started firing
	// cannot be stopped, so each (re)armed timer captures the entry's gen and
	// fire ignores callbacks whose gen is stale.
	gen uint64
}

// NewCoalescer returns a Merger in coalesce mode (window <= 0 uses
// CoalesceWindow). publish is typically (*Bus).Publish and must be non-nil.
func NewCoalescer(publish func(Event), window time.Duration) *Merger {
	return newMerger(publish, window, CoalesceWindow, false)
}

// NewSettler returns a Merger in settled-debounce mode (window <= 0 uses
// SettleWindow). publish is typically (*Bus).Publish and must be non-nil.
func NewSettler(publish func(Event), window time.Duration) *Merger {
	return newMerger(publish, window, SettleWindow, true)
}

func newMerger(publish func(Event), window, def time.Duration, reset bool) *Merger {
	if publish == nil {
		panic("event: Merger publish func is required")
	}
	if window <= 0 {
		window = def
	}
	return &Merger{
		publish: publish,
		window:  window,
		reset:   reset,
		timer:   afterFunc,
		pending: make(map[mergeKey]*mergeEntry),
	}
}

// Add records one occurrence of (topic, key). build produces the event
// payload and MAY be nil for payload-less notifications; the latest non-nil
// or nil builder simply replaces the previous one. After Close, Add is a
// no-op (shutdown drops pending merged notifications — consumers recover by
// re-reading state, see package doc).
func (m *Merger) Add(topic Topic, key string, build func() any) {
	k := mergeKey{topic: topic, key: key}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if e, ok := m.pending[k]; ok {
		e.build = build // latest builder wins; earlier ones are never invoked
		if m.reset {
			// Debounce: restart the window. Stopping may fail if the old
			// timer is already mid-fire; the gen bump makes that stale
			// callback a no-op.
			e.stop()
			e.gen++
			gen := e.gen
			e.stop = m.timer(m.window, func() { m.fire(k, gen) })
		}
		return
	}
	e := &mergeEntry{build: build}
	m.pending[k] = e
	gen := e.gen // captured by value: a later reset must strand THIS timer
	e.stop = m.timer(m.window, func() { m.fire(k, gen) })
}

// fire is the timer callback: detach the entry, then build + publish
// OUTSIDE the lock (payload construction may be expensive and publish may
// call back into arbitrary code).
func (m *Merger) fire(k mergeKey, gen uint64) {
	m.mu.Lock()
	e, ok := m.pending[k]
	if ok && e.gen == gen {
		delete(m.pending, k)
	} else {
		ok = false // stale timer (entry reset, flushed, or dropped)
	}
	closed := m.closed
	m.mu.Unlock()
	if !ok || closed {
		return
	}
	m.emit(k, e)
}

// Flush immediately fires every pending key (in unspecified order),
// canceling their timers. Useful for graceful shutdown and tests.
func (m *Merger) Flush() {
	m.mu.Lock()
	detached := make(map[mergeKey]*mergeEntry, len(m.pending))
	for k, e := range m.pending {
		e.stop()
		detached[k] = e
	}
	clear(m.pending)
	m.mu.Unlock()
	for k, e := range detached {
		m.emit(k, e)
	}
}

// Close cancels all pending timers and DROPS their events (fail direction:
// on shutdown a merged notification may be lost; the bus contract already
// requires consumers to re-read state on loss). Idempotent.
func (m *Merger) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, e := range m.pending {
		e.stop()
	}
	clear(m.pending)
}

func (m *Merger) emit(k mergeKey, e *mergeEntry) {
	var payload any
	if e.build != nil {
		payload = e.build() // lazy: built exactly once, at fire time
	}
	m.publish(Event{Topic: k.topic, Key: k.key, Payload: payload})
}
