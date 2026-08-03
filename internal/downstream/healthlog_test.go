package downstream

import (
	"errors"
	"log/slog"
	"syscall"
	"testing"
	"time"
)

// The health tracker reports STATE CHANGES. The line it replaced fired on
// every hard probe failure, so a server with its plug pulled logged one every
// probe interval for as long as it stayed unplugged — and logged nothing at
// all when it came back, because the only reporting hung off the failure
// path. These tests pin both halves.

func newLoggedHealth() (*healthTracker, *stateLog) {
	h := &stateLog{}
	return newHealthTracker(time.Unix(1000, 0), slog.New(h), serverEvents{}), h
}

func TestHealthTransientFailuresLogOnlyOnTheFlip(t *testing.T) {
	tr, h := newLoggedHealth()
	tr.success(time.Unix(1000, 0)) // connecting → connected

	base := len(h.transitions()) // the handshake line above

	now := time.Unix(1001, 0)
	for i := range HealthFailureStreak - 1 {
		tr.failure(now, errors.New("timeout"), false)
		if got := h.transitions(); len(got) != base {
			t.Fatalf("failure %d of %d logged %v, want nothing yet", i+1, HealthFailureStreak, got[base:])
		}
	}
	tr.failure(now, errors.New("timeout"), false)

	rec := h.find(t, string(ConnConnected), string(ConnError))
	if rec["level"] != slog.LevelWarn.String() {
		t.Fatalf("going down logged at %s, want WARN", rec["level"])
	}
	if rec["hard"] != "false" {
		t.Fatalf("hard = %q, want false for a transient streak", rec["hard"])
	}
}

// Down is one line, not one per probe. This is the regression test for the
// warning that used to fire on every hard failure.
func TestHealthStaysSilentWhileAlreadyDown(t *testing.T) {
	tr, h := newLoggedHealth()
	tr.success(time.Unix(1000, 0))
	base := len(h.transitions())

	now := time.Unix(1001, 0)
	for range 20 {
		tr.failure(now, syscall.ECONNREFUSED, true)
	}
	if got := h.transitions()[base:]; len(got) != 1 {
		t.Fatalf("20 hard failures logged %v, want exactly one transition", got)
	}
}

// Recovery has no other reporter: the call path only ever reports what
// fails, so without this line a log shows a server going down and never
// coming back.
func TestHealthRecoveryIsLogged(t *testing.T) {
	tr, h := newLoggedHealth()
	tr.success(time.Unix(1000, 0))
	tr.failure(time.Unix(1001, 0), syscall.ECONNREFUSED, true)
	tr.success(time.Unix(1002, 0))

	rec := h.find(t, string(ConnError), string(ConnConnected))
	if rec["level"] != slog.LevelInfo.String() {
		t.Fatalf("recovery logged at %s, want INFO", rec["level"])
	}
}

// The handshake already has a reporter one layer up ("downstream connected",
// with the tool count). Two Info lines for one event would read as two
// events, so this one is Debug.
func TestHealthFirstProbeIsDebug(t *testing.T) {
	tr, h := newLoggedHealth()
	tr.success(time.Unix(1000, 0))

	rec := h.find(t, string(ConnConnecting), string(ConnConnected))
	if rec["level"] != slog.LevelDebug.String() {
		t.Fatalf("the first probe logged at %s, want DEBUG", rec["level"])
	}
}

// A run of successes is not a run of transitions.
func TestHealthRepeatedSuccessesAreSilent(t *testing.T) {
	tr, h := newLoggedHealth()
	tr.success(time.Unix(1000, 0))
	before := len(h.transitions())
	for range 10 {
		tr.success(time.Unix(1001, 0))
	}
	if got := h.transitions(); len(got) != before {
		t.Fatalf("repeated successes logged %v, want nothing new", got[before:])
	}
}
