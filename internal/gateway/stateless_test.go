package gateway

import (
	"context"
	"encoding/json"
	"slices"
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
	if dres.ResultType != mcp.ResultTypeComplete || len(dres.SupportedVersions) == 0 ||
		dres.SupportedVersions[0] != mcp.Version2026 {
		t.Fatalf("discover result %+v", dres)
	}
	// The identity travels in _meta, not as a top-level member: a client
	// reading the 2026 shape looks nowhere else.
	if dres.ServerInfo().Name != serverName {
		t.Fatalf("serverInfo %+v", dres.ServerInfo())
	}
	if dres.TtlMs == nil || dres.CacheScope == "" {
		t.Fatalf("discover is a CacheableResult, got ttlMs=%v cacheScope=%q", dres.TtlMs, dres.CacheScope)
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

// TestStatelessNotifiesWhenTheCatalogWentLiveFirst pins the deferred
// notification at the order TestStatelessSessionEndToEnd only reaches by
// luck: the live catalog arrives BEFORE the first _meta request, so
// swapCatalog sees an uninitialized session and drops its list_changed.
// Nothing resends it on the scope path — swapCatalog re-baselines lastScope
// as it goes past, so refreshScopeAndNotify finds no content change — which
// left a 2026-07-28 client holding the cold catalog until its 60s TTL
// expired, and made the end-to-end test flaky in CI on exactly this race.
func TestStatelessNotifiesWhenTheCatalogWentLiveFirst(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	g, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})

	// No _meta has been sent yet, so the session is still uninitialized when
	// the connection completes and publishes the live catalog.
	waitFor(t, "the live catalog while the session is still uninitialized", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.ready
	})

	if resp := c.call(mcp.MethodToolsList, map[string]any{
		"_meta": meta2026(mcp.ClientCapabilities{}),
	}); resp.Error != nil {
		t.Fatalf("tools/list: %v", resp.Error)
	}
	c.waitNotification(mcp.NotificationToolsListChanged)
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
	// The payload is required, and it is the only part a client can act on:
	// "retry with one of these" is unusable without the list.
	var data mcp.UnsupportedVersionData
	if err := json.Unmarshal(resp.Error.Data, &data); err != nil {
		t.Fatalf("decode -32022 data %s: %v", resp.Error.Data, err)
	}
	if !slices.Contains(data.Supported, mcp.Version2026) || data.Requested != mcp.Version2025 {
		t.Fatalf("-32022 data = %+v", data)
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

// TestResultWithNoContentIsAnEmptyArray: `content` is a required array on a
// CallToolResult, and a nil RawMessage marshals as null. A downstream that
// omits the member entirely used to become an invalid result with agenthub's
// name on it — the hub manufacturing the violation rather than relaying one.
func TestResultWithNoContentIsAnEmptyArray(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	script := fakemcp.Minimal("echo")
	// A result with resultType and nothing else: no content member at all.
	script.Tools[0].Result = &mcp.CallResult{}
	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": script}),
	})
	// The first _meta request initializes the session; the catalog lands
	// with the list_changed that follows it.
	if resp := c.call(mcp.MethodToolsList, map[string]any{
		"_meta": meta2026(mcp.ClientCapabilities{}),
	}); resp.Error != nil {
		t.Fatalf("tools/list: %v", resp.Error)
	}
	c.waitNotification(mcp.NotificationToolsListChanged)

	resp := c.call(mcp.MethodToolsCall, map[string]any{
		"_meta": meta2026(mcp.ClientCapabilities{}),
		"name":  "fake__echo",
	})
	if resp.Error != nil {
		t.Fatalf("call: %v", resp.Error)
	}
	var wire struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire.Content) != `[]` {
		t.Fatalf("content = %s, want [] — a required array must not go out as null", wire.Content)
	}
}

// TestResultMetaTravelsMinusTheReservedNamespace: a downstream's result-level
// _meta reaches the upstream client, except for the specification's own key
// namespace, which belongs to whichever hop is speaking.
//
// Both halves are load-bearing. Dropping the whole member degraded the
// downstream's result — the same defect the tree already ruled out for tool
// members — and it was invisible because mcp.CallResult had no field at all,
// so the raw bytes went at decode and nothing downstream of that could have
// put them back. Forwarding it whole would have been the opposite mistake:
// io.modelcontextprotocol/serverInfo is the one reserved key that can
// legitimately appear here, and relaying it names the downstream as the
// server that produced a response THIS gateway produced.
func TestResultMetaTravelsMinusTheReservedNamespace(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	script := fakemcp.Minimal("echo")
	// A raw result, because a *mcp.CallResult literal could only carry what
	// the struct already models — which is the thing under test.
	script.Rules = append(script.Rules, fakemcp.Rule{
		Method: mcp.MethodToolsCall,
		Actions: []fakemcp.Action{{Kind: fakemcp.ActResult, Result: json.RawMessage(
			`{"content":[{"type":"text","text":"hi"}],"_meta":{` +
				`"com.example.tools/traceId":"abc123",` +
				`"io.modelcontextprotocol/serverInfo":{"name":"downstream-x","version":"9"}}}`)}},
	})
	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": script}),
	})
	if resp := c.call(mcp.MethodToolsList, map[string]any{
		"_meta": meta2026(mcp.ClientCapabilities{}),
	}); resp.Error != nil {
		t.Fatalf("tools/list: %v", resp.Error)
	}
	c.waitNotification(mcp.NotificationToolsListChanged)

	resp := c.call(mcp.MethodToolsCall, map[string]any{
		"_meta": meta2026(mcp.ClientCapabilities{}),
		"name":  "fake__echo",
	})
	if resp.Error != nil {
		t.Fatalf("call: %v", resp.Error)
	}
	var wire struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(resp.Result, &wire); err != nil {
		t.Fatal(err)
	}
	if got := string(wire.Meta["com.example.tools/traceId"]); got != `"abc123"` {
		t.Fatalf("the downstream's own _meta key did not survive: %s", resp.Result)
	}
	if _, ok := wire.Meta["io.modelcontextprotocol/serverInfo"]; ok {
		t.Fatalf("a reserved key was relayed across the hop: %s", resp.Result)
	}
}
