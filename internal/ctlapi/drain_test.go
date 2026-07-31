package ctlapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/session"
)

// This server has two long-lived SSE handlers and neither ends by itself:
// http.Server.Shutdown waits for handlers to return and never cancels their
// request contexts, so a graceful stop with both streams open spends its
// whole grace period and then force-closes exactly what it was waiting for.
// CloseStreams is the door that lets the drain finish, and these tests pin
// both halves — that it works, and that without it the drain really does
// hang (otherwise the first assertion proves nothing).

// openEventStream opens /v1/events and returns a close func. The stream
// belongs to no gateway link — this is the GUI / `-f` watcher shape.
func openEventStream(t *testing.T, sock string) func() {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://agenthub/v1/events?topics=servers", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rawClient(sock).Do(req)
	if err != nil {
		t.Fatalf("GET /v1/events: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		_ = resp.Body.Close()
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}
	return func() { _ = resp.Body.Close() }
}

// The whole point: with a gateway link AND an events watcher open, a stop
// that calls CloseStreams first drains instead of timing out.
func TestCloseStreamsLetsShutdownDrain(t *testing.T) {
	_, env := startServer(t, nil)
	fg := registerFakeGateway(t, env.sock, "codex")
	fg.openLink()
	defer fg.closeLink()
	closeEvents := openEventStream(t, env.sock)
	defer closeEvents()

	env.srv.CloseStreams()

	// Generous relative to the work (two handlers returning), tight relative
	// to the daemon's 5s grace: a drain that still needs the grace period
	// cannot pass this.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := env.srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after CloseStreams: %v (drain did not complete)", err)
	}
	t.Logf("drained in %v", time.Since(start))
}

// The control that keeps the test above honest: the same two streams, no
// CloseStreams, and the drain must NOT complete.
func TestShutdownAloneCannotDrainTheStreams(t *testing.T) {
	_, env := startServer(t, nil)
	fg := registerFakeGateway(t, env.sock, "codex")
	fg.openLink()
	defer fg.closeLink()
	closeEvents := openEventStream(t, env.sock)
	defer closeEvents()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := env.srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want DeadlineExceeded — if the streams now "+
			"end on their own, TestCloseStreamsLetsShutdownDrain proves nothing", err)
	}
	_ = env.srv.Close()
}

// CloseStreams runs on every stop, including stops with nothing to close and
// stops that happen twice.
func TestCloseStreamsIsIdempotentAndEmptySafe(t *testing.T) {
	_, env := startServer(t, nil)
	env.srv.CloseStreams()
	env.srv.CloseStreams()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := env.srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// A gateway whose link is ended by the stop must lose its session, exactly as
// it does when the connection drops: the link IS the session lifetime, and a
// session outliving the process that held it is the state /v1/servers would
// keep reporting for a gateway nobody holds.
func TestCloseStreamsClosesTheSession(t *testing.T) {
	_, env := startServer(t, nil)
	fg := registerFakeGateway(t, env.sock, "claude-code")
	fg.openLink()
	defer fg.closeLink()

	env.srv.CloseStreams()
	waitFor(t, "session close after CloseStreams", func() bool {
		_, ok := env.mgr.Get(session.SessionID(fg.sid))
		return !ok
	})
}
