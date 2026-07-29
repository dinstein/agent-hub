package downstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// --- tools/list leader/waiter merging ------------------

// N concurrent RefreshTools on a SLOW server must cost ONE round trip: the
// leader performs it, the waiters share its outcome. Without merging they
// would queue on the owner goroutine and each pay the full latency.
func TestConcurrentRefreshMergesIntoOneRoundTrip(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var listCalls atomic.Int64
	s, _ := scriptedServer(t, downstream.Deps{}, func(method string, _ int) (json.RawMessage, error) {
		if method != mcp.MethodToolsList {
			return json.RawMessage(`{}`), nil
		}
		listCalls.Add(1)
		<-release // hold the leader inside the round trip
		return json.RawMessage(`{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}`), nil
	})

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	started := make(chan struct{}, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started <- struct{}{}
			errs[i] = s.RefreshTools(context.Background())
		}(i)
	}
	for i := 0; i < callers; i++ {
		<-started
	}
	// Give every caller a chance to reach the merger before releasing.
	waitFor(t, 2*time.Second, func() bool { return listCalls.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if n := listCalls.Load(); n != 1 {
		t.Fatalf("tools/list round trips = %d, want 1 (%d concurrent callers must merge)", n, callers)
	}
}

// A waiter that gives up must not disturb the leader: the leader's round
// trip belongs to the server, not to whoever started it.
func TestRefreshWaiterCancelDoesNotAbortLeader(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var listCalls atomic.Int64
	s, _ := scriptedServer(t, downstream.Deps{}, func(method string, _ int) (json.RawMessage, error) {
		if method != mcp.MethodToolsList {
			return json.RawMessage(`{}`), nil
		}
		listCalls.Add(1)
		<-release
		return json.RawMessage(`{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}`), nil
	})

	leaderDone := make(chan error, 1)
	go func() { leaderDone <- s.RefreshTools(context.Background()) }()
	waitFor(t, 2*time.Second, func() bool { return listCalls.Load() == 1 })

	wctx, wcancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- s.RefreshTools(wctx) }()
	time.Sleep(50 * time.Millisecond)
	wcancel()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled waiter never returned")
	}

	close(release)
	select {
	case err := <-leaderDone:
		if err != nil {
			t.Fatalf("leader failed after the waiter left: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader never finished; a departing waiter aborted it")
	}
}

// --- reconnect ladder ("reconnect preserves retryCount") -------------

// The reconnect counter must SURVIVE a successful reconnect: a server that
// dies right after every respawn has to climb the ladder instead of
// re-dialing at the base delay forever.
func TestReconnectCounterSurvivesSuccessfulRespawn(t *testing.T) {
	t.Parallel()
	dial, dials := inProcessDial(
		fakemcp.Minimal("echo").With(fakemcp.CrashOn(mcp.MethodToolsCall)),
		fakemcp.Minimal("echo").With(fakemcp.CrashOn(mcp.MethodToolsCall)),
		fakemcp.Minimal("echo"),
	)
	s := startServer(t, downstream.Deps{
		Dial:      dial,
		Breaker:   downstream.BreakerConfig{FailureThreshold: 1, Cooldown: 10 * time.Millisecond},
		Reconnect: downstream.RetryConfig{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	})
	if n := s.Reconnects(); n != 0 {
		t.Fatalf("fresh server reports %d reconnects, want 0", n)
	}

	// Two crash/probe cycles, each of which respawns once.
	for cycle := 0; cycle < 2; cycle++ {
		_, _ = s.Call(context.Background(), "echo", nil) // opens the breaker
		time.Sleep(30 * time.Millisecond)                // ride out the cooldown
		_, _ = s.Call(context.Background(), "echo", nil) // half-open probe -> respawn
	}
	if n := s.Reconnects(); n < 2 {
		t.Fatalf("reconnects = %d after two respawns, want >= 2 (the counter was reset)", n)
	}
	if *dials < 3 {
		t.Fatalf("dials = %d, want >= 3", *dials)
	}

	// Only an explicit user reconnect zeroes the ladder.
	if err := s.Reconnect(context.Background()); err != nil {
		t.Fatalf("manual Reconnect: %v", err)
	}
	if n := s.Reconnects(); n != 0 {
		t.Fatalf("reconnects = %d after a manual Reconnect, want 0", n)
	}
}

// A manual reconnect also clears the breaker: the user asked for a working
// connection, not for a fresh connection behind an open breaker.
func TestManualReconnectClosesBreaker(t *testing.T) {
	t.Parallel()
	dial, _ := inProcessDial(
		fakemcp.Minimal("echo").With(fakemcp.CrashOn(mcp.MethodToolsCall)),
		fakemcp.Minimal("echo"),
	)
	s := startServer(t, downstream.Deps{
		Dial:      dial,
		Breaker:   downstream.BreakerConfig{FailureThreshold: 1, Cooldown: time.Hour},
		Reconnect: downstream.RetryConfig{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	_, _ = s.Call(context.Background(), "echo", nil)
	if _, err := s.Call(context.Background(), "echo", nil); !errors.Is(err, downstream.ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if err := s.Reconnect(context.Background()); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if _, err := s.Call(context.Background(), "echo", json.RawMessage(`"ok"`)); err != nil {
		t.Fatalf("call after manual reconnect: %v", err)
	}
}

// --- HTTP 410 Gone (ErrEndpointMoved) --------------------

// 410 is terminal: no retry, no respawn, and the error must tell the human
// the only thing that fixes it.
func TestEndpointMovedIsTerminalAndHinted(t *testing.T) {
	t.Parallel()
	var dials atomic.Int64
	tr := &scriptedTransport{answer: handshakeAnswer(func(string, int) (json.RawMessage, error) {
		return nil, &transport.Error{Class: transport.ClassUnavailable,
			Err: fmt.Errorf("%w: https://old.example/mcp", transport.ErrEndpointMoved)}
	})}
	s, err := downstream.Connect(context.Background(), downstream.Spec{ID: "moved"}, downstream.Deps{
		Dial: func(context.Context, downstream.Spec) (transport.Transport, error) {
			dials.Add(1)
			return tr, nil
		},
		Breaker: downstream.BreakerConfig{FailureThreshold: 1, Cooldown: 10 * time.Millisecond},
		Retry:   downstream.RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(s.Close)

	_, cerr := s.Call(context.Background(), "echo", nil)
	if cerr == nil {
		t.Fatal("call succeeded against a 410 endpoint")
	}
	if !errors.Is(cerr, downstream.ErrEndpointMoved) {
		t.Fatalf("err = %v, want errors.Is(..., downstream.ErrEndpointMoved)", cerr)
	}
	if !strings.Contains(cerr.Error(), "agenthub server add") {
		t.Fatalf("410 error %q carries no URL remediation hint", cerr)
	}
	if n := tr.count(mcp.MethodToolsCall); n != 1 {
		t.Fatalf("tools/call attempts = %d, want 1 (410 must never be retried)", n)
	}

	// Open the breaker, then probe: even the half-open probe must not
	// respawn onto a URL that is permanently gone.
	dialsBefore := dials.Load()
	time.Sleep(30 * time.Millisecond)
	_, _ = s.Call(context.Background(), "echo", nil)
	if got := dials.Load(); got != dialsBefore {
		t.Fatalf("dials went %d -> %d: a 410 probe respawned", dialsBefore, got)
	}
}

// A 410 is also a HARD health failure: it flips the state on the first
// probe rather than after three.
func TestEndpointMovedIsAHardHealthFailure(t *testing.T) {
	t.Parallel()
	s, _ := scriptedServer(t, downstream.Deps{}, func(string, int) (json.RawMessage, error) {
		return nil, &transport.Error{Class: transport.ClassUnavailable,
			Err: fmt.Errorf("%w: https://old.example/mcp", transport.ErrEndpointMoved)}
	})
	_ = s.Ping(context.Background())
	if got := s.Health().State; got != downstream.ConnError {
		t.Fatalf("state after one 410 probe = %q, want %q", got, downstream.ConnError)
	}
}

// --- stderr line ring ----------------------------------

// A child that dies during its own handshake must leave the operator its
// stderr LINES, not just "deadline exceeded". The window is line-oriented
// and capped, so a chatty child cannot flood the error.
func TestInitFailureEmbedsBoundedStderrLines(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "line-%02d\n", i)
	}
	script := fakemcp.Minimal("echo").With(fakemcp.CrashOn(mcp.MethodInitialize))
	script.StderrBanner = b.String()
	dial, _ := inProcessDial(script)
	_, err := downstream.Connect(context.Background(), downstream.Spec{ID: "noisy"}, downstream.Deps{Dial: dial})
	if err == nil {
		t.Fatal("Connect succeeded, want handshake failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "line-59") {
		t.Fatalf("error %q does not carry the LAST stderr line", msg)
	}
	// 60 lines were written, at most StderrRingLines may appear: the first
	// ones must have been dropped.
	if strings.Contains(msg, "line-00") {
		t.Fatalf("error %q carries the FIRST of 60 lines; the ring is not bounded", msg)
	}
	kept := strings.Count(msg, "line-")
	if kept > downstream.StderrRingLines {
		t.Fatalf("error embeds %d stderr lines, want at most %d", kept, downstream.StderrRingLines)
	}
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition never held within %s", timeout)
}

// --- pre-send dead connection reconnects (SSE stream died between calls) --

// A long-lived SSE stream that dies BETWEEN calls leaves every later call
// failing pre-send forever, because nothing on the ordinary call path
// reconnects it. A non-probe call that fails with ErrDeadConnection must
// rebuild the connection once and replay, since the request never reached
// the wire.
func TestPreSendDeadConnectionReconnectsOnOrdinaryCall(t *testing.T) {
	t.Parallel()
	var dials atomic.Int64
	// First connection: the stream is already dead, so the call is rejected
	// pre-send. Second connection: healthy.
	tr1 := &scriptedTransport{answer: handshakeAnswer(func(string, int) (json.RawMessage, error) {
		return nil, &transport.Error{
			Class: transport.ClassUnavailable,
			Err:   fmt.Errorf("%w: sse stream: unexpected EOF", transport.ErrDeadConnection),
		}
	})}
	tr2 := &scriptedTransport{answer: handshakeAnswer(func(string, int) (json.RawMessage, error) {
		return json.RawMessage(`{"content":[{"type":"text","text":"pong"}]}`), nil
	})}
	s, err := downstream.Connect(context.Background(), downstream.Spec{ID: "sse"}, downstream.Deps{
		Dial: func(context.Context, downstream.Spec) (transport.Transport, error) {
			if dials.Add(1) == 1 {
				return tr1, nil
			}
			return tr2, nil
		},
		Breaker:   downstream.BreakerConfig{FailureThreshold: 5, Cooldown: time.Hour},
		Reconnect: downstream.RetryConfig{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(s.Close)

	// Breaker is closed, so this is an ordinary call, NOT a half-open probe.
	raw, err := s.Call(context.Background(), "echo", nil)
	if err != nil {
		t.Fatalf("call on a dead stream did not reconnect: %v", err)
	}
	if !strings.Contains(string(raw.Content), "pong") {
		t.Fatalf("result content = %s, want the fresh connection's \"pong\"", raw.Content)
	}
	if n := dials.Load(); n != 2 {
		t.Fatalf("dials = %d, want 2 (one initial + one reconnect)", n)
	}
}

// The safety half: a stream that dies AFTER the request was written may
// have executed it, so the call must fail rather than silently replay a
// non-idempotent tools/call. Such an error carries no ErrDeadConnection
// marker, and the reconnect must not fire.
func TestPostSendConnectionDeathIsNotReplayed(t *testing.T) {
	t.Parallel()
	var dials atomic.Int64
	var calls atomic.Int64
	tr := &scriptedTransport{answer: handshakeAnswer(func(method string, _ int) (json.RawMessage, error) {
		if method == mcp.MethodToolsCall || method == "echo" {
			calls.Add(1)
		}
		// No ErrDeadConnection marker: the reply was lost post-send.
		return nil, &transport.Error{
			Class: transport.ClassUnavailable,
			Err:   errors.New("sse stream: unexpected EOF"),
		}
	})}
	s, err := downstream.Connect(context.Background(), downstream.Spec{ID: "postsend"}, downstream.Deps{
		Dial: func(context.Context, downstream.Spec) (transport.Transport, error) {
			dials.Add(1)
			return tr, nil
		},
		Breaker:   downstream.BreakerConfig{FailureThreshold: 5, Cooldown: time.Hour},
		Reconnect: downstream.RetryConfig{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(s.Close)

	if _, err := s.Call(context.Background(), "echo", nil); err == nil {
		t.Fatal("post-send stream death returned success, want an error")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("downstream saw %d executions, want exactly 1 (the call was replayed)", n)
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("dials = %d, want 1 (a possibly-executed call must not reconnect+replay)", n)
	}
}
