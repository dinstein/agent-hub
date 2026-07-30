package downstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// scriptedTransport is a Transport whose Call outcome is driven per method
// by an injected function. It exists because the failure MODES this file
// pins (ECONNREFUSED, an unanswered ping, a 410 Gone) are transport-level
// conditions a scripted fake MCP server cannot produce.
type scriptedTransport struct {
	mu     sync.Mutex
	calls  map[string]int
	answer func(method string, n int) (json.RawMessage, error)
	closed bool
	stderr string
}

func (t *scriptedTransport) Call(ctx context.Context, method string, _ any) (json.RawMessage, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, &transport.Error{Class: transport.ClassUnavailable, Err: transport.ErrClosed}
	}
	if t.calls == nil {
		t.calls = map[string]int{}
	}
	t.calls[method]++
	n := t.calls[method]
	answer := t.answer
	t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return answer(method, n)
}

func (t *scriptedTransport) count(method string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls[method]
}

func (t *scriptedTransport) Notify(context.Context, string, any) error { return nil }
func (t *scriptedTransport) OnPeerRequest(transport.PeerHandler)       {}
func (t *scriptedTransport) OnListChanged(func(transport.ChangeMask))  {}
func (t *scriptedTransport) Stderr() string                            { return t.stderr }
func (t *scriptedTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

// handshakeAnswer serves initialize and the CONNECT-TIME tools/list so
// Connect succeeds, then delegates everything else — including every later
// tools/list — to next. server/discover is answered like a pre-2026 server
// (method-not-found), driving Handshake down the legacy initialize path.
func handshakeAnswer(next func(method string, n int) (json.RawMessage, error)) func(string, int) (json.RawMessage, error) {
	return func(method string, n int) (json.RawMessage, error) {
		switch {
		case method == mcp.MethodDiscover:
			return nil, &transport.Error{Class: transport.ClassFatal, Err: &mcp.Error{
				Code: mcp.CodeMethodNotFound, Message: "scripted server predates server/discover",
			}}
		case method == mcp.MethodInitialize:
			return json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{},` +
				`"serverInfo":{"name":"scripted","version":"1"}}`), nil
		case method == mcp.MethodToolsList && n == 1:
			return json.RawMessage(`{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}`), nil
		}
		return next(method, n)
	}
}

func scriptedServer(t *testing.T, deps downstream.Deps, answer func(string, int) (json.RawMessage, error)) (*downstream.Server, *scriptedTransport) {
	t.Helper()
	tr := &scriptedTransport{answer: handshakeAnswer(answer)}
	deps.Dial = func(context.Context, downstream.Spec) (transport.Transport, error) { return tr, nil }
	s, err := downstream.Connect(context.Background(), downstream.Spec{ID: "scripted"}, deps)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s, tr
}

// A fresh connection is healthy: the handshake itself is the first liveness
// proof, so nothing has to wait for the first probe tick.
func TestHealthStartsConnected(t *testing.T) {
	t.Parallel()
	s := startServer(t, downstream.Deps{}, fakemcp.Minimal("echo"))
	if got := s.Health().State; got != downstream.ConnConnected {
		t.Fatalf("initial health = %q, want %q", got, downstream.ConnConnected)
	}
}

// A server that answers ping with method-not-found is still ALIVE: the round
// trip completed, which is all a liveness probe may conclude. Health must
// stay green (this is the common case — ping is optional in MCP).
func TestPingMethodNotFoundCountsAsAlive(t *testing.T) {
	t.Parallel()
	s, _ := scriptedServer(t, downstream.Deps{}, func(string, int) (json.RawMessage, error) {
		return nil, &transport.Error{Class: transport.ClassFatal,
			Err: &mcp.Error{Code: mcp.CodeMethodNotFound, Message: "unknown method ping"}}
	})
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping returned %v, want nil (an answered error proves liveness)", err)
	}
	h := s.Health()
	if h.State != downstream.ConnConnected || h.Failures != 0 {
		t.Fatalf("health = %+v, want connected with no failure streak", h)
	}
}

// Design 7.11: three CONSECUTIVE transient failures — not one, not two —
// flip the state to error.
func TestHealthNeedsThreeTransientFailures(t *testing.T) {
	t.Parallel()
	s, _ := scriptedServer(t, downstream.Deps{}, func(string, int) (json.RawMessage, error) {
		return nil, &transport.Error{Class: transport.ClassUnavailable, Err: errors.New("stream stalled")}
	})
	ctx := context.Background()
	for i := 1; i < downstream.HealthFailureStreak; i++ {
		_ = s.Ping(ctx)
		if got := s.Health().State; got != downstream.ConnConnected {
			t.Fatalf("after %d transient failures state = %q, want still %q",
				i, got, downstream.ConnConnected)
		}
	}
	_ = s.Ping(ctx)
	h := s.Health()
	if h.State != downstream.ConnError {
		t.Fatalf("after %d transient failures state = %q, want %q",
			downstream.HealthFailureStreak, h.State, downstream.ConnError)
	}
	if h.Detail == "" {
		t.Error("error state carries no detail; the operator needs the last probe error")
	}
}

// A successful probe anywhere in the streak resets it, so a flaky-but-alive
// server never accumulates its way to red.
func TestHealthStreakResetsOnSuccess(t *testing.T) {
	t.Parallel()
	var fail bool
	var mu sync.Mutex
	s, _ := scriptedServer(t, downstream.Deps{}, func(string, int) (json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, &transport.Error{Class: transport.ClassUnavailable, Err: errors.New("blip")}
		}
		return json.RawMessage(`{}`), nil
	})
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		mu.Lock()
		fail = i%2 == 0 // fail, ok, fail, ok, ... never two in a row
		mu.Unlock()
		_ = s.Ping(ctx)
	}
	if got := s.Health().State; got != downstream.ConnConnected {
		t.Fatalf("alternating failures flipped health to %q; the streak did not reset", got)
	}
}

// Design 7.11: a connection-refused class error flips at once — waiting two
// more probe periods to say "refused" helps nobody.
func TestHealthHardErrorFlipsImmediately(t *testing.T) {
	t.Parallel()
	s, _ := scriptedServer(t, downstream.Deps{}, func(string, int) (json.RawMessage, error) {
		return nil, &transport.Error{Class: transport.ClassUnavailable,
			Err: fmt.Errorf("dial tcp 127.0.0.1:1: %w", syscall.ECONNREFUSED)}
	})
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded against a refusing server")
	}
	h := s.Health()
	if h.State != downstream.ConnError {
		t.Fatalf("state = %q after one hard failure, want %q", h.State, downstream.ConnError)
	}
	if h.Failures != 1 {
		t.Fatalf("failures = %d, want 1 (flipped on the first, not the third)", h.Failures)
	}
}

// The background prober actually runs and converges without any caller.
func TestBackgroundProbeFlipsHealth(t *testing.T) {
	t.Parallel()
	s, _ := scriptedServer(t, downstream.Deps{PingInterval: 5 * time.Millisecond},
		func(string, int) (json.RawMessage, error) {
			return nil, &transport.Error{Class: transport.ClassUnavailable,
				Err: fmt.Errorf("gone: %w", syscall.ECONNREFUSED)}
		})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Health().State == downstream.ConnError {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("background prober never flipped health; last = %+v", s.Health())
}

// The health probe must not be gated by the circuit breaker: a breaker that
// is open because tool calls fail is exactly when the probe has to run.
func TestPingBypassesOpenBreaker(t *testing.T) {
	t.Parallel()
	s, tr := scriptedServer(t, downstream.Deps{
		Breaker: downstream.BreakerConfig{FailureThreshold: 1, Cooldown: time.Hour},
	}, func(method string, _ int) (json.RawMessage, error) {
		if method == mcp.MethodToolsCall {
			return nil, &transport.Error{Class: transport.ClassUnavailable, Err: errors.New("dead")}
		}
		return json.RawMessage(`{}`), nil
	})
	if _, err := s.Call(context.Background(), "echo", nil); err == nil {
		t.Fatal("call succeeded, want the failure that opens the breaker")
	}
	if _, err := s.Call(context.Background(), "echo", nil); !errors.Is(err, downstream.ErrCircuitOpen) {
		t.Fatalf("second call err = %v, want ErrCircuitOpen", err)
	}
	before := tr.count(mcp.MethodPing)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping through an open breaker: %v", err)
	}
	if got := tr.count(mcp.MethodPing) - before; got != 1 {
		t.Fatalf("ping round trips while open = %d, want 1 (the breaker gates CALLS, not probes)", got)
	}
}
