package gateway

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
)

// The re-dial plane logged the dials and nothing about the gaps between them:
// one Info per attempt, carrying an attempt count. By the rungs where the
// question actually gets asked — 45s, 135s, then five minutes forever — "it
// has given up" and "it is waiting out a backoff it earned" therefore read
// exactly alike, and the ladder's own arithmetic was the missing half.
func TestArmingTheRedialLadderIsRecorded(t *testing.T) {
	t.Parallel()
	log, sink := newCallLog()
	g := &gateway{
		redial:      newRedialParams(0), // production ladder: 5s, 15s, 45s, …
		redialAt:    map[string]time.Time{},
		redialTries: map[string]int{},
		connErr:     map[string]connectFailure{},
		servers:     map[string]*downstream.Server{},
		log:         log,
	}

	g.noteConnectResult("s", errors.New("boom"))
	g.noteConnectResult("s", errors.New("boom again"))

	rec := sink.find(t, "re-dial armed")
	if rec["level"] != slog.LevelDebug.String() {
		t.Errorf("the ladder line logged at %s, want DEBUG", rec["level"])
	}
	if rec["server"] != "s" {
		t.Errorf("server = %q, want s", rec["server"])
	}
	// Two failures, two rungs: the second must report the wait it earned, not
	// the base delay again. A ladder that logged a constant would be the exact
	// bug this line is meant to expose.
	if n := sink.count("re-dial armed"); n != 2 {
		t.Fatalf("armed %d times, want 2 — one per recorded failure", n)
	}
	second := lastRec(t, sink, "re-dial armed")
	if second["rung"] != "2" {
		t.Errorf("second arming reports rung %q, want 2", second["rung"])
	}
	if second["in_ms"] != "15000" {
		t.Errorf("second rung waits %q ms, want 15000 (the ladder's second step)", second["in_ms"])
	}
}

// A SUCCESSFUL connect clears the ladder rather than arming it, so it must
// produce no line at all: a "re-dial armed" beside a working server would
// describe a recovery that is not pending.
func TestASuccessfulConnectArmsNothing(t *testing.T) {
	t.Parallel()
	log, sink := newCallLog()
	g := &gateway{
		redial:      newRedialParams(0),
		redialAt:    map[string]time.Time{},
		redialTries: map[string]int{},
		connErr:     map[string]connectFailure{},
		servers:     map[string]*downstream.Server{},
		log:         log,
	}

	g.noteConnectResult("s", nil)

	if n := sink.count("re-dial armed"); n != 0 {
		t.Fatalf("a successful connect armed the ladder %d times, want 0", n)
	}
}

// Storing a credential for a server with no RECORDED FAILURE wakes nothing,
// and the announcement is then followed by no re-dial at all. Unexplained,
// that sequence reads as a lost announcement and sends the reader after a
// broken watcher instead of at a server that was never broken.
func TestACredentialForAHealthyServerSaysItWokeNothing(t *testing.T) {
	t.Parallel()
	log, sink := newCallLog()
	g := &gateway{
		redialAt:    map[string]time.Time{},
		redialTries: map[string]int{},
		connErr:     map[string]connectFailure{}, // no failure on record
		servers:     map[string]*downstream.Server{},
		credEpochs:  newCredEpochs(),
		log:         log,
	}

	g.onCredentialChanged("s")

	rec := sink.find(t, "credential announcement woke no re-dial: no recorded failure to recover from")
	if rec["level"] != slog.LevelDebug.String() {
		t.Errorf("logged at %s, want DEBUG", rec["level"])
	}

	// With a failure on record the wake DOES happen, and the line must not
	// appear — otherwise it would fire on the path it exists to distinguish.
	g.connErr["s"] = connectFailure{detail: "boom"}
	g.onCredentialChanged("s")
	if n := sink.count("credential announcement woke no re-dial: no recorded failure to recover from"); n != 1 {
		t.Fatalf("the no-op line fired %d times; a recorded failure must wake the ladder silently", n)
	}
}

// lastRec returns the most recent record with msg, for assertions about a
// line that is written more than once.
func lastRec(t *testing.T, sink *callLog, msg string) map[string]string {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for i := len(sink.recs) - 1; i >= 0; i-- {
		if sink.recs[i]["msg"] == msg {
			return sink.recs[i]
		}
	}
	t.Fatalf("no %q record; logged: %v", msg, sink.seen)
	return nil
}

// TestDownstreamDepsOpensTheNotificationStream: the gateway asks for the
// server→client stream on every downstream it dials.
//
// This is one field, and it was off for the whole of this project's life.
// Catalog refresh has no trigger but tools/list_changed — no poll, no TTL,
// no re-list except on a reconnect a healthy server never performs — and on
// streamable-http that notification has no other channel to arrive by. So a
// hosted downstream's catalog was fixed at connect, invisibly. The same
// field gates the 2026-07-28 subscriptions/listen path, which made that dead
// code in the shipped binary too.
//
// The assertion is deliberately on downstreamDeps rather than on a dialled
// connection: this is the single description of how the gateway and its
// derived pool dial, and the defect was that it said nothing here.
func TestDownstreamDepsOpensTheNotificationStream(t *testing.T) {
	g := &gateway{log: slog.New(slog.DiscardHandler)}
	if !g.downstreamDeps().NotificationStream {
		t.Fatal("the gateway dials without a server→client stream; " +
			"a streamable-http downstream then has no way to report a tool-set change")
	}
}
