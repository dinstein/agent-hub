package daemon

import (
	"testing"
	"time"
)

// The idle reaper closes a credential's gateway after httpConnIdle, and
// lastUsed only advances when a REQUEST goes past. A client that is being
// pushed to sends none, so without the pin an open notification stream is
// reliably killed by the reaper — and killed quietly, since closing the
// gateway is the ordinary teardown and says nothing about the subscriber it
// took with it. These tests are that pin.

func readyConn(lastUsed time.Time, subs int) *httpConn {
	ready := make(chan struct{})
	close(ready)
	// conn stays nil: sweep only calls Close on a non-nil one, and what is
	// under test is which entries it decides to close at all.
	return &httpConn{ready: ready, lastUsed: lastUsed, subs: subs}
}

func TestSweepSkipsGatewaysCarryingAnOpenStream(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p := newHTTPPlane(httpPlaneDeps{Now: func() time.Time { return now }})
	p.conns["pinned"] = readyConn(now.Add(-2*httpConnIdle), 1)

	p.sweep(now)

	if _, ok := p.conns["pinned"]; !ok {
		t.Fatal("the reaper closed a gateway with an open notification stream")
	}
}

func TestSweepStillReapsAnIdleGatewayWithNoStream(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p := newHTTPPlane(httpPlaneDeps{Now: func() time.Time { return now }})
	p.conns["idle"] = readyConn(now.Add(-2*httpConnIdle), 0)

	p.sweep(now)

	if _, ok := p.conns["idle"]; ok {
		t.Fatal("the pin held a gateway that had no stream; the reaper must still collect it")
	}
}

// TestUnpinRestartsTheIdleClock: a stream that just closed is evidence the
// client was there a moment ago. Reaping on the next tick would punish
// exactly the client that had been listening.
func TestUnpinRestartsTheIdleClock(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p := newHTTPPlane(httpPlaneDeps{Now: func() time.Time { return now }})
	p.conns["k"] = readyConn(now.Add(-2*httpConnIdle), 1)

	p.unpin("k")

	p.sweep(now)
	if _, ok := p.conns["k"]; !ok {
		t.Fatal("the gateway was reaped immediately after its stream closed")
	}
	if got := p.conns["k"].subs; got != 0 {
		t.Fatalf("subs = %d after unpin, want 0", got)
	}
}

// TestPinRefusesAVanishedGateway: fail-closed. A subscription pinned to a
// connection that is already gone would report success and then deliver
// nothing for the life of the stream.
func TestPinRefusesAVanishedGateway(t *testing.T) {
	t.Parallel()
	p := newHTTPPlane(httpPlaneDeps{})
	if p.pin("never-assembled") {
		t.Fatal("pin claimed a gateway that is not in the table")
	}
}

func TestPinRefusesAClosedPlane(t *testing.T) {
	t.Parallel()
	now := time.Now()
	p := newHTTPPlane(httpPlaneDeps{Now: func() time.Time { return now }})
	p.conns["k"] = readyConn(now, 0)
	p.closed = true
	if p.pin("k") {
		t.Fatal("pin succeeded on a plane that is shutting down")
	}
}
