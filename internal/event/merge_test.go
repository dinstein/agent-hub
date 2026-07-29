package event

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTimers is a deterministic timerFactory: timers never fire on their
// own; tests fire them explicitly.
type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	fn      func()
	stopped bool
	fired   bool
}

func (f *fakeTimers) factory(d time.Duration, fn func()) (stop func() bool) {
	ft := &fakeTimer{d: d, fn: fn}
	f.mu.Lock()
	f.timers = append(f.timers, ft)
	f.mu.Unlock()
	return func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if ft.fired || ft.stopped {
			return false
		}
		ft.stopped = true
		return true
	}
}

// fire runs timer i's callback (like the deadline elapsing), regardless of
// a prior stop when force is set — used to model the "already mid-fire"
// race.
func (f *fakeTimers) fire(t *testing.T, i int, force bool) {
	t.Helper()
	f.mu.Lock()
	if i >= len(f.timers) {
		f.mu.Unlock()
		t.Fatalf("no timer %d (have %d)", i, len(f.timers))
	}
	ft := f.timers[i]
	if ft.fired || (ft.stopped && !force) {
		f.mu.Unlock()
		t.Fatalf("timer %d already consumed", i)
	}
	ft.fired = true
	fn := ft.fn
	f.mu.Unlock()
	fn()
}

func (f *fakeTimers) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

type capture struct {
	mu     sync.Mutex
	events []Event
}

func (c *capture) publish(ev Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *capture) all() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

func newFakeCoalescer(out *capture) (*Merger, *fakeTimers) {
	ft := &fakeTimers{}
	m := NewCoalescer(out.publish, CoalesceWindow)
	m.timer = ft.factory
	return m, ft
}

func newFakeSettler(out *capture) (*Merger, *fakeTimers) {
	ft := &fakeTimers{}
	m := NewSettler(out.publish, SettleWindow)
	m.timer = ft.factory
	return m, ft
}

func TestCoalescerMergesStormAndBuildsOnce(t *testing.T) {
	var out capture
	m, ft := newFakeCoalescer(&out)
	defer m.Close()

	var builds atomic.Int64
	const k = 100
	for i := 0; i < k; i++ {
		i := i
		m.Add("servers.changed", "srv", func() any {
			builds.Add(1)
			return i
		})
	}
	if got := ft.count(); got != 1 {
		t.Fatalf("coalescer armed %d timers for one key, want 1 (window must not reset)", got)
	}
	if builds.Load() != 0 {
		t.Fatal("payload built before fire — must be lazy")
	}
	ft.fire(t, 0, false)

	evs := out.all()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Payload != k-1 {
		t.Fatalf("payload = %v, want latest builder's %d", evs[0].Payload, k-1)
	}
	if builds.Load() != 1 {
		t.Fatalf("build invoked %d times, want exactly 1", builds.Load())
	}
}

func TestCoalescerSeparateKeysSeparateEvents(t *testing.T) {
	var out capture
	m, ft := newFakeCoalescer(&out)
	defer m.Close()

	m.Add("t", "a", func() any { return "a" })
	m.Add("t", "b", func() any { return "b" })
	m.Add("u", "a", func() any { return "ua" }) // same key, different topic
	if got := ft.count(); got != 3 {
		t.Fatalf("timers = %d, want 3", got)
	}
	for i := 0; i < 3; i++ {
		ft.fire(t, i, false)
	}
	if got := len(out.all()); got != 3 {
		t.Fatalf("events = %d, want 3", got)
	}
}

func TestCoalescerNewWindowAfterFire(t *testing.T) {
	var out capture
	m, ft := newFakeCoalescer(&out)
	defer m.Close()

	m.Add("t", "k", func() any { return 1 })
	ft.fire(t, 0, false)
	m.Add("t", "k", func() any { return 2 })
	ft.fire(t, 1, false)

	evs := out.all()
	if len(evs) != 2 || evs[0].Payload != 1 || evs[1].Payload != 2 {
		t.Fatalf("events = %+v, want [1 2]", evs)
	}
}

func TestSettlerResetsWindowAndEmitsLatest(t *testing.T) {
	var out capture
	m, ft := newFakeSettler(&out)
	defer m.Close()

	m.Add("scan", "x", func() any { return 1 })
	m.Add("scan", "x", func() any { return 2 })
	m.Add("scan", "x", func() any { return 3 })
	if got := ft.count(); got != 3 {
		t.Fatalf("settler armed %d timers, want 3 (every Add resets)", got)
	}
	// The first two timers were stopped by the resets.
	for i := 0; i < 2; i++ {
		ft.mu.Lock()
		stopped := ft.timers[i].stopped
		ft.mu.Unlock()
		if !stopped {
			t.Fatalf("timer %d not stopped on reset", i)
		}
	}
	ft.fire(t, 2, false)
	evs := out.all()
	if len(evs) != 1 || evs[0].Payload != 3 {
		t.Fatalf("events = %+v, want one settled event with payload 3", evs)
	}
}

// TestSettlerStaleFireIsIgnored models the race where an old timer already
// entered its callback when the reset tried to stop it: the gen guard must
// discard it, and only the rearmed timer may emit.
func TestSettlerStaleFireIsIgnored(t *testing.T) {
	var out capture
	m, ft := newFakeSettler(&out)
	defer m.Close()

	m.Add("scan", "x", func() any { return "old" })
	m.Add("scan", "x", func() any { return "new" }) // stops timer 0, arms timer 1

	ft.fire(t, 0, true) // stale callback runs anyway
	if got := len(out.all()); got != 0 {
		t.Fatalf("stale timer emitted %d events, want 0", got)
	}
	ft.fire(t, 1, false)
	evs := out.all()
	if len(evs) != 1 || evs[0].Payload != "new" {
		t.Fatalf("events = %+v, want single 'new'", evs)
	}
}

func TestMergerFlushEmitsPendingImmediately(t *testing.T) {
	var out capture
	m, ft := newFakeCoalescer(&out)
	defer m.Close()

	m.Add("t", "a", func() any { return "a" })
	m.Add("t", "b", nil) // nil builder = payload-less notification
	m.Flush()

	evs := out.all()
	if len(evs) != 2 {
		t.Fatalf("flush emitted %d events, want 2", len(evs))
	}
	// A late fire from an already-flushed timer must be a no-op.
	ft.fire(t, 0, true)
	if got := len(out.all()); got != 2 {
		t.Fatalf("late fire after flush emitted extra event (%d total)", got)
	}
}

func TestMergerCloseDropsPending(t *testing.T) {
	var out capture
	m, ft := newFakeCoalescer(&out)

	var builds atomic.Int64
	m.Add("t", "a", func() any { builds.Add(1); return nil })
	m.Close()
	m.Close() // idempotent
	ft.fire(t, 0, true)
	m.Add("t", "b", func() any { builds.Add(1); return nil }) // no-op after close

	if got := len(out.all()); got != 0 {
		t.Fatalf("close leaked %d events", got)
	}
	if builds.Load() != 0 {
		t.Fatal("builder invoked after Close")
	}
	if got := ft.count(); got != 1 {
		t.Fatalf("Add after Close armed a timer (%d total)", got)
	}
}

// TestCoalescerRealTimerSmoke exercises the default time.AfterFunc path.
func TestCoalescerRealTimerSmoke(t *testing.T) {
	b := NewBus()
	sub := b.Subscribe(1, "t")
	defer sub.Close()

	m := NewCoalescer(b.Publish, 5*time.Millisecond)
	defer m.Close()
	m.Add("t", "k", func() any { return 42 })

	select {
	case ev := <-sub.Events():
		if ev.Payload != 42 {
			t.Fatalf("payload = %v", ev.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coalesced event never fired")
	}
}
