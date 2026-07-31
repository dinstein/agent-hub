package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// meta2026 builds the per-request _meta a 2026-07-28 client carries.
func meta2026(caps mcp.ClientCapabilities) *mcp.RequestMeta {
	return &mcp.RequestMeta{
		ProtocolVersion:    mcp.Version2026,
		ClientCapabilities: caps,
		ClientInfo:         &mcp.Implementation{Name: "stateless-test-client", Version: "0"},
	}
}

// TestInitializeClampsStatelessVersion pins the §7.1 rule: initialize can
// only negotiate the stateful protocol family. A client declaring
// 2026-07-28 through the handshake that 2026 removed is answered with the
// default, not an echo that would promise per-request _meta semantics the
// session does not have.
func TestInitializeClampsStatelessVersion(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	res := c.initialize(mcp.Version2026, mcp.ClientCapabilities{})
	if res.ProtocolVersion != mcp.ProtocolVersion {
		t.Fatalf("initialize echoed %q; the stateful handshake must clamp to %q",
			res.ProtocolVersion, mcp.ProtocolVersion)
	}
}

// TestStatelessSessionEndToEnd drives the whole 2026-07-28 exposure path:
// server/discover instead of initialize, per-request _meta, resultType and
// freshness hints on answers, and no reverse RPC ever reaching the client.
func TestStatelessSessionEndToEnd(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	g, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})

	// 1. Discover answers immediately with every supported version.
	resp := c.call(mcp.MethodDiscover, mcp.DiscoverParams{Meta: meta2026(mcp.ClientCapabilities{})})
	if resp.Error != nil {
		t.Fatalf("discover: %v", resp.Error)
	}
	var dres mcp.DiscoverResult
	if err := json.Unmarshal(resp.Result, &dres); err != nil {
		t.Fatal(err)
	}
	if dres.ResultType != mcp.ResultTypeComplete || len(dres.ProtocolVersions) == 0 ||
		dres.ProtocolVersions[0] != mcp.Version2026 {
		t.Fatalf("discover result %+v", dres)
	}
	if dres.ServerInfo.Name != serverName {
		t.Fatalf("serverInfo %+v", dres.ServerInfo)
	}

	// 2. The first _meta request marks the session initialized: the deferred
	// list_changed arrives once the catalog is live, with no
	// notifications/initialized ever sent.
	c.waitNotification(mcp.NotificationToolsListChanged)

	// 3. tools/list with _meta carries resultType and the freshness hints.
	lresp := c.call(mcp.MethodToolsList, map[string]any{"_meta": meta2026(mcp.ClientCapabilities{})})
	if lresp.Error != nil {
		t.Fatalf("tools/list: %v", lresp.Error)
	}
	var lres mcp.ListToolsResult
	if err := json.Unmarshal(lresp.Result, &lres); err != nil {
		t.Fatal(err)
	}
	if lres.ResultType != mcp.ResultTypeComplete {
		t.Fatalf("tools/list resultType = %q", lres.ResultType)
	}
	if lres.TtlMs == nil || *lres.TtlMs != listTTLMs || lres.CacheScope != "private" {
		t.Fatalf("tools/list freshness hints: ttlMs=%v cacheScope=%q", lres.TtlMs, lres.CacheScope)
	}
	if len(lres.Tools) != 1 || lres.Tools[0].Name != "fake__echo" {
		t.Fatalf("tools = %+v", lres.Tools)
	}

	// 4. tools/call with _meta answers with resultType complete.
	cresp := c.call(mcp.MethodToolsCall, map[string]any{
		"_meta":     meta2026(mcp.ClientCapabilities{Roots: &mcp.RootsCapability{}}),
		"name":      "fake__echo",
		"arguments": map[string]string{"s": "hi"},
	})
	if cresp.Error != nil {
		t.Fatalf("tools/call: %v", cresp.Error)
	}
	var cres mcp.CallResult
	if err := json.Unmarshal(cresp.Result, &cres); err != nil {
		t.Fatal(err)
	}
	if cres.ResultType != mcp.ResultTypeComplete {
		t.Fatalf("tools/call resultType = %q", cres.ResultType)
	}

	// 5. Even though the _meta above declared a roots capability, a
	// stateless session is never sent a reverse RPC: 2026 removed
	// server-initiated requests. Roots resolve to nothing, silently.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	roots, err := g.roots.Roots(ctx)
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("roots = %v, want none for a stateless session", roots)
	}
	if n := c.rootsCalls.Load(); n != 0 {
		t.Fatalf("client answered %d roots/list reverse RPCs; a stateless client must never see one", n)
	}
}

// TestStatelessRejectsWrongMetaVersion pins the fail-closed direction: a
// _meta declaring a version this gateway cannot serve statelessly is
// rejected with the spec's code, not answered with semantics the session
// does not have.
func TestStatelessRejectsWrongMetaVersion(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	resp := c.call(mcp.MethodToolsList, map[string]any{
		"_meta": &mcp.RequestMeta{ProtocolVersion: mcp.Version2025},
	})
	if resp.Error == nil || resp.Error.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Fatalf("resp = %+v, want CodeUnsupportedProtocolVersion", resp)
	}
	if !strings.Contains(resp.Error.Message, "initialize") {
		t.Fatalf("error %q should point the client at the initialize handshake", resp.Error.Message)
	}
}

// TestLegacySessionNeverSeesResultType pins §7.5: a stateful session's
// answers never carry the 2026 resultType member, even when the downstream
// included one.
func TestLegacySessionNeverSeesResultType(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	script := fakemcp.Minimal()
	script.Tools = []fakemcp.Tool{{
		Def: mcp.ToolDef{Name: "modern", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Result: &mcp.CallResult{
			ResultType: mcp.ResultTypeComplete, // a 2026-ish downstream
			Content:    json.RawMessage(`[{"type":"text","text":"ok"}]`),
		},
	}}
	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": script}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	c.waitNotification(mcp.NotificationToolsListChanged)

	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__modern"})
	if resp.Error != nil {
		t.Fatalf("tools/call: %v", resp.Error)
	}
	if strings.Contains(string(resp.Result), "resultType") {
		t.Fatalf("legacy session saw resultType: %s", resp.Result)
	}
}

// TestCallClientRefusesAStatelessSession pins the invariant at the level the
// documentation states it: "a stateless session is never sent a reverse RPC",
// full stop, rather than "clientRoots remembers not to".
//
// The guard used to live only in clientRoots.fetchFromClient, which is the
// single caller today. callClient takes the method as a STRING, so the second
// reverse RPC anyone adds would have inherited the exemption without a
// compiler or a test saying anything — and the symptom is a frame a
// 2026-07-28 client is entitled to treat as a protocol error, from a session
// that never agreed to receive requests at all.
//
// The gateway here is deliberately built with a nil FrameWriter: if the guard
// ever stops firing, this test does not fail on an assertion, it panics on the
// write, which is the loudest available proof that no frame was sent.
func TestCallClientRefusesAStatelessSession(t *testing.T) {
	g := &gateway{stateless: true}

	got, err := g.callClient(context.Background(), mcp.MethodRootsList, nil)
	if err == nil {
		t.Fatal("callClient answered a stateless session; it must refuse before writing a frame")
	}
	if got != nil {
		t.Fatalf("callClient returned %s alongside its refusal", got)
	}
	for _, want := range []string{mcp.MethodRootsList, "stateless"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q; an operator reading it "+
				"has to know which call was dropped and why", err, want)
		}
	}
}
