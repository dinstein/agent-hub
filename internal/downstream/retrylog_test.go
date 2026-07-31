package downstream_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// A retried call and a slow downstream are the same observation from
// outside: one call, one answer, several seconds. Without a line per attempt
// the backoff is invisible, and "why did that take three seconds" has no
// answer in the log.
func TestRetryLeavesALine(t *testing.T) {
	t.Parallel()
	script := fakemcp.Minimal("echo").With(fakemcp.Rule{
		Method: mcp.MethodToolsCall,
		Call:   1,
		Actions: []fakemcp.Action{{
			Kind:  fakemcp.ActError,
			Error: &mcp.Error{Code: 429, Message: "rate limited"},
		}},
	})
	dial, _ := inProcessDial(script)

	sink := &boundLog{}
	srv, err := downstream.Connect(context.Background(), downstream.Spec{ID: "fake"}, downstream.Deps{
		// Debug: the line is deliberately below the default level — a retry
		// that works says nothing about the system, and the call's own total
		// is reported by the caller.
		Log:   slog.New(&boundHandler{sink: sink}),
		Dial:  dial,
		Retry: downstream.RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(srv.Close)

	if _, err := srv.Call(context.Background(), "echo", json.RawMessage(`"x"`)); err != nil {
		t.Fatalf("call: %v (the 429 should have been retried)", err)
	}

	recs := sink.records("retrying a downstream call")
	if len(recs) != 1 {
		t.Fatalf("got %d retry records, want exactly 1 (one retried attempt)", len(recs))
	}
	rec := recs[0]
	if rec["method"] != mcp.MethodToolsCall {
		t.Fatalf("method = %q, want %q", rec["method"], mcp.MethodToolsCall)
	}
	if rec["attempt"] != "1" || rec["of"] != "3" {
		t.Fatalf("attempt %q of %q, want 1 of 3: %v", rec["attempt"], rec["of"], rec)
	}
	if rec["backoff"] == "" {
		t.Fatalf("no backoff on the line, so it cannot explain the delay it caused: %v", rec)
	}
}

// "downstream connected" had no counterpart, so a server removed from the
// config, redefined, or taken down with the process left a log whose last
// word on it was that it connected.
func TestClosingAConnectionLeavesALine(t *testing.T) {
	t.Parallel()
	sink := &boundLog{}
	srv, err := downstream.Connect(context.Background(), downstream.Spec{ID: "fake"}, downstream.Deps{
		Log: slog.New(&boundHandler{sink: sink}),
		Dial: func(context.Context, downstream.Spec) (transport.Transport, error) {
			return fakemcp.Connect(fakemcp.Minimal("echo"))
		},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	srv.Close()
	srv.Close() // idempotent: and so is the line

	recs := sink.records("downstream connection closed")
	if len(recs) != 1 {
		t.Fatalf("got %d close records, want exactly 1", len(recs))
	}
	if recs[0]["server"] != "fake" {
		t.Fatalf("the close record does not name the server: %v", recs[0])
	}
}
