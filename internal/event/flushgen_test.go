package event

import (
	"testing"
	"time"
)

// A timer that has begun firing cannot be stopped, so a stranded callback can
// still run after Flush has detached its entry. The generation number is what
// makes that callback a no-op — and it has to keep working when the SAME key
// is added again, because a per-entry counter would hand the replacement the
// stranded callback's own number.
//
// The timer is driven by hand rather than by a clock: the interleaving is the
// subject, and a sleep would test the scheduler instead.
func TestStrandedTimerNeverFiresALaterEntry(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name string
		make func(func(Event)) *Merger
	}{
		{"coalescer", func(p func(Event)) *Merger { return NewCoalescer(p, time.Hour) }},
		{"settler", func(p func(Event)) *Merger { return NewSettler(p, time.Hour) }},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			var fired []any
			m := mode.make(func(ev Event) { fired = append(fired, ev.Payload) })

			// Collected in arming order: the callback that must do nothing is
			// the FIRST one, and a fake that kept only the latest would hand
			// the test the second entry's own timer instead.
			var armed []func()
			m.timer = func(_ time.Duration, fn func()) func() bool {
				armed = append(armed, fn)
				return func() bool { return false } // already mid-fire: stop fails
			}

			m.Add("t", "k", func() any { return "first" })
			m.Flush() // detaches and emits the first entry
			m.Add("t", "k", func() any { return "second" })
			armed[0]() // the callback armed for the FIRST entry runs now

			if len(fired) != 1 || fired[0] != "first" {
				t.Fatalf("published %v, want [first]: a callback stranded by Flush fired the entry "+
					"added after it, a window early", fired)
			}
		})
	}
}

// The settler's own reset race, which the generation number always covered:
// re-arming must strand the previous timer rather than let it fire the key.
func TestSettlerResetStrandsThePreviousTimer(t *testing.T) {
	t.Parallel()
	var fired []any
	m := NewSettler(func(ev Event) { fired = append(fired, ev.Payload) }, time.Hour)

	var callbacks []func()
	m.timer = func(_ time.Duration, fn func()) func() bool {
		callbacks = append(callbacks, fn)
		return func() bool { return false }
	}

	m.Add("t", "k", func() any { return "first" })
	m.Add("t", "k", func() any { return "second" }) // resets: strands callbacks[0]
	callbacks[0]()                                  // must do nothing
	if len(fired) != 0 {
		t.Fatalf("published %v after a stranded reset callback, want nothing", fired)
	}
	callbacks[1]() // the live timer fires the latest builder
	if len(fired) != 1 || fired[0] != "second" {
		t.Fatalf("published %v, want [second]", fired)
	}
}
