package mcpstub_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/test/mcpstub"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestDownstreamSpeaks2026 is the Phase 1 integration proof: a full
// downstream.Connect against a strict stateless server. Because the stub
// rejects any request missing _meta or the Mcp-* headers, a green run means
// the whole stack — Handshake, discover negotiation, per-request _meta
// injection, header stamping — is conformant, not just self-consistent.
func TestDownstreamSpeaks2026(t *testing.T) {
	stub := mcpstub.New()
	defer stub.Close()

	srv, err := downstream.Connect(testCtx(t), downstream.Spec{
		ID:         "stub2026",
		Kind:       transport.StreamableHTTP,
		URL:        stub.URL(),
		Provenance: downstream.ProvenanceLocal,
	}, downstream.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer srv.Close()

	ir := srv.InitializeResult()
	if ir == nil || ir.ProtocolVersion != mcp.Version2026 {
		t.Fatalf("handshake result %+v, want protocol %q", ir, mcp.Version2026)
	}
	if ir.ServerInfo.Name != "mcpstub" {
		t.Fatalf("serverInfo %+v", ir.ServerInfo)
	}

	tools := srv.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want the one echo tool", tools)
	}

	res, err := srv.Call(testCtx(t), "echo", json.RawMessage(`{"s":"hi"}`))
	if err != nil {
		t.Fatalf("call echo: %v", err)
	}
	if res.IsError {
		t.Fatalf("echo answered isError: %s", res.Content)
	}
	if !strings.Contains(string(res.Content), `{\"s\":\"hi\"}`) &&
		!strings.Contains(string(res.Content), `{"s":"hi"}`) {
		t.Fatalf("echo content %s does not carry the arguments", res.Content)
	}

	// The stateless path: discover ran, initialize never did.
	if n := stub.Calls(mcp.MethodDiscover); n != 1 {
		t.Fatalf("server/discover called %d times, want 1", n)
	}
	if n := stub.Calls(mcp.MethodInitialize); n != 0 {
		t.Fatalf("initialize called %d times on the 2026 path, want 0", n)
	}
	if n := stub.Calls(mcp.NotificationInitialized); n != 0 {
		t.Fatalf("notifications/initialized sent %d times on the 2026 path, want 0", n)
	}
}
