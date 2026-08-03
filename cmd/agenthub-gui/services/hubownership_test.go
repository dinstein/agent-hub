package services

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Ownership decides one thing: whether quitting stops the hub. Getting it
// wrong in one direction strands a process nobody asked for; in the other it
// cuts off every client of a hub somebody else is running. Both used to be
// reachable, because the claim was a bool written from "did my dial start
// it" — a fact about a past call rather than about the hub in front of us.
// It is now the process handle itself.

// startOwned brings up a Hub that had to start its own hub: nothing answers a
// dial, so connect goes on to supervise one.
func startOwned(t *testing.T) (*Hub, *testDialer) {
	t.Helper()
	d := newFakeDaemon(t, pingMux(t))
	h, dl := newHub(t, d, nil)
	// Milliseconds rather than the production seconds: what is under test is
	// the ladder's SHAPE — it doubles, it is bounded — not its wall-clock.
	h.backoff = time.Millisecond
	dl.setDialErr(errors.New("connect: no such file or directory"))
	h.start(context.Background())
	waitFor(t, "the hub to come up", func() bool { return h.Status().Connected })
	if !h.OwnsDaemon() {
		t.Fatal("precondition: a hub this application started was not claimed")
	}
	return h, dl
}

func TestAHubWeStartedIsStoppedOnTheWayOut(t *testing.T) {
	h, dl := startOwned(t)
	proc := dl.lastProcess()

	h.stop()

	if proc.stopCount() != 1 {
		t.Fatalf("the hub we started was stopped %d times, want exactly 1", proc.stopCount())
	}
	if h.OwnsDaemon() {
		t.Error("stop left the ownership claim standing")
	}
	// Idempotent: ServiceShutdown and a signal handler can both arrive.
	h.stop()
	if proc.stopCount() != 1 {
		t.Errorf("a second stop signalled the hub again (%d)", proc.stopCount())
	}
}

func TestAHubSomebodyElseRunsIsNeverStopped(t *testing.T) {
	// The dial succeeds, which is what a headless hub or another AgentHub
	// window looks like from here.
	d := newFakeDaemon(t, pingMux(t))
	h, dl := newHub(t, d, nil)
	h.start(context.Background())
	waitFor(t, "the hub to answer", func() bool { return h.Status().Connected })

	if h.OwnsDaemon() {
		t.Fatal("a hub we only dialled was claimed as ours; quitting would end it")
	}
	if _, starts := dl.counts(); starts != 0 {
		t.Errorf("a hub was already answering and we started %d more", starts)
	}
	h.stop()
	if p := dl.lastProcess(); p != nil {
		t.Fatalf("something was supervised after all (%d stops)", p.stopCount())
	}
}

// A transport failure says the CONNECTION is gone, not the process. The claim
// must survive it: a hub that is briefly unreachable is still ours to stop,
// and disowning it there is how an application comes to quit while leaving
// its own hub running.
func TestATransportFailureDoesNotDisownTheHub(t *testing.T) {
	h, _ := startOwned(t)

	h.dropClient(errors.New("connection reset by peer"))

	if !h.OwnsDaemon() {
		t.Fatal("a dropped connection cleared the ownership claim; quitting would strand the hub")
	}
}

// The one event that really does end ownership: the process exiting.
func TestOwnershipEndsWhenTheProcessDoes(t *testing.T) {
	h, dl := startOwned(t)
	// Nothing can be restarted, so the Hub settles into "not ours" instead of
	// coming back with a new process while this test is looking.
	dl.setSuperviseErr(errors.New("hub refuses to start"))

	dl.lastProcess().die()

	waitFor(t, "ownership to end with the process", func() bool { return !h.OwnsDaemon() })
}

func TestTheHubIsRestartedAfterAnUnexpectedExit(t *testing.T) {
	h, dl := startOwned(t)
	first := dl.lastProcess()

	first.die()

	waitFor(t, "a replacement hub", func() bool {
		p := dl.lastProcess()
		return p != nil && p != first
	})
	waitFor(t, "the replacement to be owned", h.OwnsDaemon)
	if _, starts := dl.counts(); starts != 2 {
		t.Errorf("supervise attempts = %d, want 2 (the original and its replacement)", starts)
	}
}

// A hub that will not start must not be retried forever: a process per
// interval buries the first failure — the one that says why — under
// thousands of identical ones, and the user is left with a log instead of an
// error on screen.
func TestRestartsGiveUpRatherThanLoopForever(t *testing.T) {
	h, dl := startOwned(t)
	dl.setSuperviseErr(errors.New("hub refuses to start"))
	dl.lastProcess().die()

	waitFor(t, "the restarts to be exhausted", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.restarts >= restartLimit
	})
	// Give the loop a chance to keep going, if it were going to.
	time.Sleep(50 * time.Millisecond)
	h.mu.Lock()
	got := h.restarts
	h.mu.Unlock()
	if got > restartLimit {
		t.Fatalf("restarts = %d, want no more than %d", got, restartLimit)
	}
	if h.Status().Connected {
		t.Error("status still reads connected after the hub was lost for good")
	}
}

// A deliberate shutdown is not a fall: stopping the hub must not trip the
// supervisor into starting another one on the way out.
func TestQuittingDoesNotRestartTheHub(t *testing.T) {
	h, dl := startOwned(t)

	h.stop()

	time.Sleep(50 * time.Millisecond)
	if _, starts := dl.counts(); starts != 1 {
		t.Fatalf("supervise attempts = %d after a deliberate stop, want 1", starts)
	}
}
