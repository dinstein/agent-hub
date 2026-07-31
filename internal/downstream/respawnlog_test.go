package downstream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// A respawn has two automatic causes and they call for opposite fixes: a
// half-open probe failing says the downstream is unwell, a pre-send dead
// connection says the stream between here and it did not survive being idle.
// Both used to log the same "respawned after failed half-open probe" with no
// field to tell them apart and no trace of the error that triggered either,
// so a log full of respawns could not say which had happened. These tests
// pin the fields that answer it.

// respawnLog collects records for assertions.
type respawnLog struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *respawnLog) Enabled(context.Context, slog.Level) bool { return true }

func (h *respawnLog) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *respawnLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *respawnLog) WithGroup(string) slog.Handler      { return h }

// attrs returns the attributes of the first record with this message.
func (h *respawnLog) attrs(t *testing.T, msg string) map[string]string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var seen []string
	for _, r := range h.recs {
		seen = append(seen, r.Message)
		if r.Message != msg {
			continue
		}
		out := map[string]string{}
		r.Attrs(func(a slog.Attr) bool {
			out[a.Key] = a.Value.String()
			return true
		})
		return out
	}
	t.Fatalf("no %q record; logged: %s", msg, strings.Join(seen, " | "))
	return nil
}

func (h *respawnLog) logger() *slog.Logger { return slog.New(h) }

// The idle-reap shape: an ordinary call finds the stream dead before the
// wire. The log must name that cause and carry the transport's own words —
// they are the only evidence of what killed the connection.
func TestRespawnLogsDeadConnectionCause(t *testing.T) {
	t.Parallel()
	rec := &respawnLog{}
	var dials int
	dial := func(context.Context, downstream.Spec) (transport.Transport, error) {
		dials++
		dead := dials == 1
		return &scriptedTransport{answer: handshakeAnswer(func(string, int) (json.RawMessage, error) {
			if dead {
				return nil, &transport.Error{
					Class: transport.ClassUnavailable,
					Err:   fmt.Errorf("%w: sse stream: unexpected EOF", transport.ErrDeadConnection),
				}
			}
			return json.RawMessage(`{"content":[{"type":"text","text":"pong"}]}`), nil
		})}, nil
	}
	s, err := downstream.Connect(context.Background(), downstream.Spec{ID: "reaped"}, downstream.Deps{
		Log:       rec.logger(),
		Dial:      dial,
		Breaker:   downstream.BreakerConfig{FailureThreshold: 5, Cooldown: time.Hour},
		Reconnect: downstream.RetryConfig{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(s.Close)

	if _, err := s.Call(context.Background(), "echo", nil); err != nil {
		t.Fatalf("call on a dead stream: %v", err)
	}
	got := rec.attrs(t, "respawned")
	if got["cause"] != "dead-connection" {
		t.Errorf("cause = %q, want %q", got["cause"], "dead-connection")
	}
	if !strings.Contains(got["trigger"], "unexpected EOF") {
		t.Errorf("trigger = %q, want the transport's failure text", got["trigger"])
	}
}

// The other cause: the single half-open probe of an open circuit fails on
// the connection it was sent to test.
func TestRespawnLogsHalfOpenProbeCause(t *testing.T) {
	t.Parallel()
	rec := &respawnLog{}
	dial, _ := inProcessDial(
		fakemcp.Minimal("echo").With(fakemcp.CrashOn(mcp.MethodToolsCall)),
		fakemcp.Minimal("echo"),
	)
	s := startServer(t, downstream.Deps{
		Log:       rec.logger(),
		Dial:      dial,
		Breaker:   downstream.BreakerConfig{FailureThreshold: 1, Cooldown: 10 * time.Millisecond},
		Reconnect: downstream.RetryConfig{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	})
	_, _ = s.Call(context.Background(), "echo", nil) // opens the breaker
	time.Sleep(30 * time.Millisecond)                // ride out the cooldown
	if _, err := s.Call(context.Background(), "echo", nil); err != nil {
		t.Fatalf("half-open probe never recovered: %v", err)
	}
	if got := rec.attrs(t, "respawned")["cause"]; got != "half-open-probe" {
		t.Errorf("cause = %q, want %q", got, "half-open-probe")
	}
}

// A manual reconnect has no failure behind it, so it must not invent one:
// the cause says so and the trigger field is absent rather than empty.
func TestManualReconnectLogsNoTrigger(t *testing.T) {
	t.Parallel()
	rec := &respawnLog{}
	dial, _ := inProcessDial(fakemcp.Minimal("echo"), fakemcp.Minimal("echo"))
	s := startServer(t, downstream.Deps{Log: rec.logger(), Dial: dial})
	if err := s.Reconnect(context.Background()); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	got := rec.attrs(t, "respawned")
	if got["cause"] != "manual" {
		t.Errorf("cause = %q, want %q", got["cause"], "manual")
	}
	if trigger, ok := got["trigger"]; ok {
		t.Errorf("trigger = %q on a manual reconnect, want the field absent", trigger)
	}
}

// A startup crash embeds the child's stderr tail in the handshake error,
// because a child that dies leaves nothing else. Mid-life the same evidence
// was dropped: the dying transport was closed and only the transport's own
// verdict — "broken pipe", "unexpected EOF" — reached the log, while the
// panic that caused it went to a closed pipe and nowhere.
func TestRespawnCarriesTheDeadChildsStderr(t *testing.T) {
	t.Parallel()
	rec := &respawnLog{}
	var dials int
	dial := func(context.Context, downstream.Spec) (transport.Transport, error) {
		dials++
		dead := dials == 1
		tr := &scriptedTransport{answer: handshakeAnswer(func(string, int) (json.RawMessage, error) {
			if dead {
				return nil, &transport.Error{
					Class: transport.ClassUnavailable,
					Err:   fmt.Errorf("%w: sse stream: unexpected EOF", transport.ErrDeadConnection),
				}
			}
			return json.RawMessage(`{"content":[{"type":"text","text":"pong"}]}`), nil
		})}
		if dead {
			tr.stderr = "starting up\npanic: nil map write\ngoroutine 1 [running]:\n"
		}
		return tr, nil
	}
	s, err := downstream.Connect(context.Background(), downstream.Spec{ID: "crasher"}, downstream.Deps{
		Log:       rec.logger(),
		Dial:      dial,
		Breaker:   downstream.BreakerConfig{FailureThreshold: 5, Cooldown: time.Hour},
		Reconnect: downstream.RetryConfig{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(s.Close)

	if _, err := s.Call(context.Background(), "echo", nil); err != nil {
		t.Fatalf("call on a dead stream: %v", err)
	}
	got := rec.attrs(t, "respawned")
	if !strings.Contains(got["child_stderr"], "panic: nil map write") {
		t.Errorf("child_stderr = %q, want the dead child's panic", got["child_stderr"])
	}
}

// A manual reconnect kills a connection that was WORKING, and a working MCP
// server's stderr is its ordinary chatter — many of them log there
// routinely. Attaching it would put a paragraph of unrelated output on every
// `agenthub server reconnect`.
func TestManualReconnectCarriesNoStderr(t *testing.T) {
	t.Parallel()
	rec := &respawnLog{}
	dial := func(context.Context, downstream.Spec) (transport.Transport, error) {
		return &scriptedTransport{
			stderr: "listening on stdio\nserving 12 tools\n",
			answer: handshakeAnswer(func(string, int) (json.RawMessage, error) {
				return json.RawMessage(`{"content":[]}`), nil
			}),
		}, nil
	}
	s, err := downstream.Connect(context.Background(), downstream.Spec{ID: "chatty"},
		downstream.Deps{Log: rec.logger(), Dial: dial})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(s.Close)

	if err := s.Reconnect(context.Background()); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if tail, ok := rec.attrs(t, "respawned")["child_stderr"]; ok {
		t.Errorf("child_stderr = %q on a manual reconnect, want the field absent", tail)
	}
}
