package services

import (
	"context"
	"errors"
	"testing"
)

// TestOwnershipClaimDoesNotOutliveItsDaemon is the regression for the
// finding the 2026-07-31 sweep confirmed.
//
// h.spawned was only ever written true. dialOrStart answers the ownership
// question both ways and the false half was discarded; dropClient cleared
// every field except this one. So a GUI that started a daemon, lost it, and
// reconnected by plain dial to one somebody else had started still believed
// the daemon was its own — and SIGTERMed it on window close, ending another
// client's session to tidy up after ours.
func TestOwnershipClaimDoesNotOutliveItsDaemon(t *testing.T) {
	d := newFakeDaemon(t, pingMux(t))
	h, dl := newHub(t, d, nil)

	// 1. No daemon running: the GUI starts one, so it owns it.
	dl.setSpawns(true)
	h.start(context.Background())
	waitFor(t, "daemon connected", func() bool { return h.Status().Connected })
	h.mu.Lock()
	ours := h.spawned
	h.mu.Unlock()
	if !ours {
		t.Fatal("precondition: a daemon we spawned was not claimed")
	}

	// 2. That daemon is lost at transport level.
	h.dropClient(errors.New("connection reset by peer"))
	h.mu.Lock()
	stillOurs := h.spawned
	h.mu.Unlock()
	if stillOurs {
		t.Error("the ownership claim survived the loss of the daemon it named")
	}

	// 3. The GUI reconnects with a plain dial, which cannot spawn: whatever
	//    answers now was started by somebody else.
	if _, err := h.connect(context.Background(), false); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	h.mu.Lock()
	claimed := h.spawned
	h.mu.Unlock()
	if claimed {
		t.Fatal("a daemon reached by plain dial was claimed as ours; stop would SIGTERM it")
	}
}

// TestConnectRecordsTheDialersAnswerBothWays pins the narrower half: when
// dialOrStart reports it merely FOUND a daemon, that answer must be written
// down rather than left to whatever the field already held.
func TestConnectRecordsTheDialersAnswerBothWays(t *testing.T) {
	d := newFakeDaemon(t, pingMux(t))
	h, dl := newHub(t, d, nil)

	dl.setSpawns(true)
	if _, err := h.connect(context.Background(), true); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	h.mu.Lock()
	h.client = nil // force the next connect to do real work
	first := h.spawned
	h.mu.Unlock()
	if !first {
		t.Fatal("precondition: a spawned daemon was not claimed")
	}

	// The same Hub, now finding a daemon instead of starting one.
	dl.setSpawns(false)
	if _, err := h.connect(context.Background(), true); err != nil {
		t.Fatalf("second connect: %v", err)
	}
	h.mu.Lock()
	second := h.spawned
	h.mu.Unlock()
	if second {
		t.Error("dialOrStart reported it found a running daemon, and the claim stayed set")
	}
}
