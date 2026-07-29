package gateway

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// flakyDial fails the first n dials of every server, then delegates. It is
// the shape of every failure the re-dial ladder exists for: the downstream
// was not ready when the gateway got there.
type flakyDial struct {
	mu       sync.Mutex
	failures int
	dials    int
	inner    downstream.DialFunc
}

func (f *flakyDial) fn(ctx context.Context, spec downstream.Spec) (transport.Transport, error) {
	f.mu.Lock()
	f.dials++
	if f.failures > 0 {
		f.failures--
		f.mu.Unlock()
		return nil, fmt.Errorf("dial refused (test)")
	}
	f.mu.Unlock()
	return f.inner(ctx, spec)
}

func (f *flakyDial) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials
}

// TestRedialRecoversAFailedDownstream is the regression test for "a
// downstream that failed its handshake is never dialed again, so every
// recovery costs a client restart". Nothing here edits the registry and
// nothing restarts: the ladder alone has to bring the server back.
func TestRedialRecoversAFailedDownstream(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "alpha")
	fd := &flakyDial{
		failures: 2,
		inner:    scriptedDial(map[string]*fakemcp.Script{"alpha": fakemcp.Minimal("echo")}),
	}
	_, c, _ := startGateway(t, Config{
		ClientID:   "redial",
		Resolver:   resolver,
		Dial:       fd.fn,
		RedialBase: 20 * time.Millisecond,
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})

	waitForTools(t, c, "alpha__echo")
	callToolOK(t, c, "alpha__echo")
	if got := fd.count(); got < 3 {
		t.Errorf("alpha dialed %d times, want at least 3 (2 failures + the recovery)", got)
	}
}

// TestRedialLeavesAConnectedServerAlone: the ladder must be driven by a
// RECORDED failure, never by the tick. A gateway that re-dials healthy
// servers would respawn every stdio child on a timer.
func TestRedialLeavesAConnectedServerAlone(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "alpha")
	dials := newDialCounter(scriptedDial(map[string]*fakemcp.Script{"alpha": fakemcp.Minimal("echo")}))
	_, c, _ := startGateway(t, Config{
		ClientID:   "redial-idle",
		Resolver:   resolver,
		Dial:       dials.fn,
		RedialBase: 10 * time.Millisecond, // tick floors at 10ms: ~15 ticks below
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "alpha__echo")

	time.Sleep(150 * time.Millisecond)
	if got := dials.count("alpha"); got != 1 {
		t.Errorf("connected server dialed %d times, want 1", got)
	}
}

// TestRedialLadderBacksOffAndCaps pins the ladder itself: 5s, 15s, 45s,
// 135s, then 5 minutes forever. The cap is the load-bearing half — without
// it a permanently dead server is dialed at the base delay for the life of
// the process.
func TestRedialLadderBacksOffAndCaps(t *testing.T) {
	t.Parallel()
	g := &gateway{
		redial:      newRedialParams(0), // production ladder
		redialAt:    map[string]time.Time{},
		redialTries: map[string]int{},
	}
	now := time.Now()
	want := []time.Duration{
		5 * time.Second,
		15 * time.Second,
		45 * time.Second,
		135 * time.Second,
		5 * time.Minute,
		5 * time.Minute,
	}
	for i, w := range want {
		g.armLocked("s", now)
		if got := g.redialAt["s"].Sub(now); got != w {
			t.Errorf("rung %d due in %v, want %v", i+1, got, w)
		}
	}

	// A success clears the ladder: the NEXT failure starts at the base
	// again, rather than inheriting a rung the server has since climbed out
	// of.
	g.resetLadderLocked("s")
	g.armLocked("s", now)
	if got := g.redialAt["s"].Sub(now); got != 5*time.Second {
		t.Errorf("after a reset the first rung is %v, want 5s", got)
	}
}

// TestRedialParamsDeriveFromTheBase: one knob moves the whole ladder, so a
// shrunken test ladder can never end up with a base above its own ceiling.
func TestRedialParamsDeriveFromTheBase(t *testing.T) {
	t.Parallel()
	prod := newRedialParams(0)
	if prod.base != 5*time.Second || prod.tick != 2500*time.Millisecond || prod.cap != 5*time.Minute {
		t.Errorf("production ladder = %+v, want base 5s / tick 2.5s / cap 5m", prod)
	}
	tiny := newRedialParams(2 * time.Millisecond)
	if tiny.tick != minRedialTick {
		t.Errorf("tick %v, want the %v floor (a shrunken ladder must not spin the CPU)", tiny.tick, minRedialTick)
	}
	if tiny.cap <= tiny.base {
		t.Errorf("cap %v must stay above base %v", tiny.cap, tiny.base)
	}
}
