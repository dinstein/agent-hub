package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/scope"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
	"github.com/dinstein/agent-hub/internal/tier"
)

// openConn assembles an in-process gateway (the shape the daemon's HTTP data
// plane uses) over one fakemcp downstream and completes the handshake.
func openConn(t *testing.T, cfg Config) *Conn {
	t.Helper()
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	conn, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(conn.Close)

	res := conn.Do(context.Background(), mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize,
		marshalParams(t, mcp.InitializeParams{
			ProtocolVersion: mcp.ProtocolVersion,
			ClientInfo:      mcp.Implementation{Name: "inproc-test", Version: "0"},
		})))
	if res.Error != nil {
		t.Fatalf("initialize over the in-process conn: %v", res.Error)
	}
	if err := conn.Notify(mcp.NewNotification(mcp.NotificationInitialized, nil)); err != nil {
		t.Fatalf("notifications/initialized: %v", err)
	}
	return conn
}

// connCallOK drives one successful tools/call, retrying the transient busy
// error while the downstream connects.
func connCallOK(t *testing.T, conn *Conn, tool string) mcp.CallResult {
	t.Helper()
	var out mcp.CallResult
	waitFor(t, "tools/call over the in-process conn", func() bool {
		res := conn.Do(context.Background(), mcp.NewRequest(mcp.NewIntID(99), mcp.MethodToolsCall,
			marshalParams(t, mcp.CallToolParams{Name: tool, Arguments: []byte(`{"ok":true}`)})))
		if res.Error != nil {
			if res.Error.Code != codeRetryBusy {
				t.Fatalf("tools/call %s: %v", tool, res.Error)
			}
			return false
		}
		if err := json.Unmarshal(res.Result, &out); err != nil {
			t.Fatalf("decode call result: %v", err)
		}
		return true
	})
	return out
}

// TestInProcGateCountParity is the no-second-execution-path proof for the
// daemon's HTTP data plane: one tools/call driven through the in-process
// connection (what an HTTP request becomes) advances EVERY pipeline stage
// exactly as one tools/call driven over the stdio pipe does.
//
// The two gateways are separate assemblies with separate pipelines, so both
// counter maps start at zero and a divergence in either the set of stages or
// their counts fails the test. That is the same standard the direct /
// call_tool / code-mode parity tests hold their paths to.
func TestInProcGateCountParity(t *testing.T) {
	t.Parallel()

	stdioResolver := testResolver(t.TempDir())
	seedRegistry(t, stdioResolver, "fake")
	stdioGW, c, _ := startGateway(t, Config{
		ClientID: "stdio-client",
		Resolver: stdioResolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")
	callToolOK(t, c, "fake__echo")
	stdioCounters := stdioGW.pipe.Counters()

	httpResolver := testResolver(t.TempDir())
	seedRegistry(t, httpResolver, "fake")
	conn := openConn(t, Config{
		ClientID: "http-client",
		Resolver: httpResolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	connCallOK(t, conn, "fake__echo")
	httpCounters := conn.Counters()

	if len(stdioCounters) == 0 {
		t.Fatal("the stdio pipeline reports no counting stages")
	}
	if !maps.Equal(stdioCounters, httpCounters) {
		t.Fatalf("gate counters diverge:\n  stdio = %v\n  http  = %v\n"+
			"the HTTP face must traverse the SAME chain as stdio (canonical.md §2: one execute pipeline)",
			stdioCounters, httpCounters)
	}
	for _, stage := range []string{
		pipeline.GateScope, pipeline.GateTokenTier, pipeline.GatePrecheck,
		pipeline.StageDefendAndShape,
	} {
		if httpCounters[stage] != 1 {
			t.Errorf("stage %q counted %d over the HTTP path, want 1", stage, httpCounters[stage])
		}
	}
}

// TestInProcCallerTierGate proves the credential tier minted by the HTTP face
// is enforced by the EXISTING token tier gate: a read-only caller may not
// invoke a tool whose annotations are absent (fail-closed ⇒ destructive).
func TestInProcCallerTierGate(t *testing.T) {
	t.Parallel()

	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	conn := openConn(t, Config{
		ClientID:   "read-only-token",
		Resolver:   resolver,
		CallerTier: tier.Read,
		Dial:       scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})

	var last *mcp.Error
	waitFor(t, "the token tier gate to reject the call", func() bool {
		res := conn.Do(context.Background(), mcp.NewRequest(mcp.NewIntID(7), mcp.MethodToolsCall,
			marshalParams(t, mcp.CallToolParams{Name: "fake__echo", Arguments: []byte(`{}`)})))
		last = res.Error
		if res.Error == nil {
			t.Fatal("a read-only caller executed an unannotated (⇒ destructive) tool")
		}
		return res.Error.Code != codeRetryBusy
	})
	if last == nil || !strings.Contains(last.Message, pipeline.CodeTokenTierDenied) {
		t.Fatalf("rejection = %v, want the stable %s code", last, pipeline.CodeTokenTierDenied)
	}
}

// TestInProcCredentialScopeLayerNarrows proves an agent token's server
// allowlist joins the ORDINARY scope intersection: the tool disappears from
// tools/list and the scope gate — not some HTTP-side filter — denies the call.
func TestInProcCredentialScopeLayerNarrows(t *testing.T) {
	t.Parallel()

	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	conn := openConn(t, Config{
		ClientID: "pinned-token",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
		ScopeLayers: func() []scope.ScopeLayer {
			return []scope.ScopeLayer{{
				Kind:    scope.LayerSession,
				Origin:  "token:pinned-token",
				Servers: []string{"some-other-server"},
			}}
		},
	})

	// The catalog needs a moment to go live; until it does the answer is the
	// retryable busy error rather than a gate rejection.
	var last *mcp.Error
	waitFor(t, "the scope gate to reject the call", func() bool {
		res := conn.Do(context.Background(), mcp.NewRequest(mcp.NewIntID(11), mcp.MethodToolsCall,
			marshalParams(t, mcp.CallToolParams{Name: "fake__echo", Arguments: []byte(`{}`)})))
		last = res.Error
		if res.Error == nil {
			t.Fatal("a token allowlisting another server reached fake__echo")
		}
		return res.Error.Code != codeRetryBusy
	})
	if last == nil || !strings.Contains(last.Message, pipeline.CodeScopeDenied) {
		t.Fatalf("rejection = %v, want the stable %s code", last, pipeline.CodeScopeDenied)
	}

	res := conn.Do(context.Background(), mcp.NewRequest(mcp.NewIntID(12), mcp.MethodToolsList, nil))
	if res.Error != nil {
		t.Fatalf("tools/list: %v", res.Error)
	}
	var list mcp.ListToolsResult
	if err := json.Unmarshal(res.Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	for _, def := range list.Tools {
		if def.Name == "fake__echo" {
			t.Fatalf("tools/list still exposes %q outside the credential's scope", def.Name)
		}
	}
}

// TestInProcCloseIsIdempotent: Close must be safe to call twice (the daemon's
// reaper and its shutdown can race for the same connection).
func TestInProcCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	// NOT t.TempDir: this test closes the gateway while its background
	// connect is still in flight, so the tool-cache writer may land after
	// Close returns. t.TempDir's cleanup FAILS the test on a non-empty
	// directory, which would report a benign write-after-close as a failure
	// of whatever ran last.
	dir, err := os.MkdirTemp("", "ahgw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolver := testResolver(dir)
	seedRegistry(t, resolver, "fake")
	conn, err := Open(Config{
		ClientID: "closer",
		Resolver: resolver,
		Log:      slog.New(slog.DiscardHandler),
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	done := make(chan struct{})
	go func() { defer close(done); conn.Close() }()
	conn.Close()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("concurrent Close did not return")
	}

	res := conn.Do(context.Background(), mcp.NewRequest(mcp.NewIntID(1), mcp.MethodPing, nil))
	if res.Error == nil {
		t.Fatal("a closed conn answered a request; want an error response")
	}
}
