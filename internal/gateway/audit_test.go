package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/accesslog"
	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

func auditTestKey() []byte { return bytes.Repeat([]byte{0x5a}, 32) }

func auditSecretResolver(key []byte) secrets.Resolver {
	encoded := base64.RawStdEncoding.EncodeToString(key)
	return func(_ context.Context, ref secrets.Ref) (string, bool, error) {
		if ref == secrets.AuditEncryptionRef() {
			return encoded, true, nil
		}
		return "", false, nil
	}
}

func setAuditPolicy(t *testing.T, resolver *platform.Resolver, mutate func(*registry.AuditPolicy)) {
	t.Helper()
	keyID, err := accesslog.KeyID(auditTestKey())
	if err != nil {
		t.Fatal(err)
	}
	st := externalRegistry(t, resolver)
	updateRegistry(t, st, func(tx *registry.Tx) {
		p := registry.AuditPolicy{
			Enabled: true, Durability: "sync",
			ResultMode: "truncated", ResultBytes: registry.DefaultAuditResultBytes,
			RetentionDays: registry.DefaultAuditRetentionDays,
			MaxBytes:      registry.DefaultAuditMaxBytes, MinFreeBytes: registry.DefaultAuditMinFree,
			KeyID: keyID,
		}
		mutate(&p)
		tx.Governance.V.Audit = &registry.Doc[registry.AuditPolicy]{V: p}
	})
}

func readAuditEvents(t *testing.T, resolver *platform.Resolver) []accesslog.Event {
	t.Helper()
	root, err := accesslog.DefaultDir(resolver)
	if err != nil {
		t.Fatal(err)
	}
	events, skipped, err := accesslog.ReadEvents(root)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("skipped %d malformed audit events", skipped)
	}
	return events
}

func payloadOf(t *testing.T, resolver *platform.Resolver, ref *accesslog.PayloadRef) []byte {
	t.Helper()
	if ref == nil {
		t.Fatal("missing payload reference")
	}
	root, err := accesslog.DefaultDir(resolver)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := accesslog.ReadPayload(root, *ref, auditTestKey())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func eventKind(t *testing.T, events []accesslog.Event, kind accesslog.EventKind) accesslog.Event {
	t.Helper()
	for _, e := range events {
		if e.Kind == kind {
			return e
		}
	}
	t.Fatalf("no %s event in %+v", kind, events)
	return accesslog.Event{}
}

func TestAuditRecordsCompleteRequestRouteAndBoundedResult(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	setAuditPolicy(t, resolver, func(p *registry.AuditPolicy) {
		p.ResultMode, p.ResultBytes = "truncated", 24
	})
	_, c, _ := startGateway(t, Config{
		ClientID: "audited", Face: "http", Resolver: resolver,
		Secrets: auditSecretResolver(auditTestKey()),
		Dial:    scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")
	args := json.RawMessage(`{"password":"complete-secret","n":1}`)
	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__echo", Arguments: args})
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}

	events := readAuditEvents(t, resolver)
	if len(events) != 3 {
		t.Fatalf("events = %d, want received+routed+finished: %+v", len(events), events)
	}
	received := eventKind(t, events, accesslog.EventReceived)
	var request mcp.CallToolParams
	if err := json.Unmarshal(payloadOf(t, resolver, received.Request), &request); err != nil {
		t.Fatal(err)
	}
	if request.Name != "fake__echo" || !bytes.Equal(request.Arguments, args) {
		t.Fatalf("recorded request = %+v", request)
	}
	routed := eventKind(t, events, accesslog.EventRouted)
	if routed.Server != "fake" || routed.Tool != "echo" || routed.Exposed != "fake__echo" || routed.Face != "http" {
		t.Fatalf("routed = %+v", routed)
	}
	if got := payloadOf(t, resolver, routed.EffectiveArgs); !bytes.Equal(got, args) {
		t.Fatalf("effective args = %s, want %s", got, args)
	}
	finished := eventKind(t, events, accesslog.EventFinished)
	if finished.Outcome != "success" || !finished.ResultCut || finished.Result == nil || finished.ResultBytes <= 24 {
		t.Fatalf("finished = %+v", finished)
	}
	if got := payloadOf(t, resolver, finished.Result); len(got) != 24 || !bytes.Equal(got, resp.Result[:24]) {
		t.Fatalf("captured result = %q, want first 24 response bytes", got)
	}
}

func TestAuditLazyCallToolKeepsWrapperAndEffectiveArguments(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	setAuditPolicy(t, resolver, func(p *registry.AuditPolicy) { p.ResultMode = "none" })
	ext := externalRegistry(t, resolver)
	setGovernance(t, ext, func(g *registry.GovernanceDoc) { g.Discovery = "lazy" })
	_, c, _ := startGateway(t, Config{
		ClientID: "audit-lazy", Resolver: resolver, Secrets: auditSecretResolver(auditTestKey()),
		Dial: scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, metaNames...)
	res := callToolResult(t, c, discovery.MetaCallTool, map[string]any{
		"tool": "fake__echo", "arguments": map[string]any{"marker": "lazy-audit"},
	})
	if res.IsError {
		t.Fatal(resultText(t, res))
	}
	events := readAuditEvents(t, resolver)
	if len(events) != 3 {
		t.Fatalf("events = %d, want one three-event lifecycle: %+v", len(events), events)
	}
	routed := eventKind(t, events, accesslog.EventRouted)
	if routed.Exposed != discovery.MetaCallTool || routed.Server != "fake" || routed.Tool != "echo" {
		t.Fatalf("lazy route = %+v", routed)
	}
	if got := string(payloadOf(t, resolver, routed.EffectiveArgs)); got != `{"marker":"lazy-audit"}` {
		t.Fatalf("effective args = %s", got)
	}
	finished := eventKind(t, events, accesslog.EventFinished)
	if finished.Result != nil || finished.ResultCapture != "none" {
		t.Fatalf("none result policy captured a result: %+v", finished)
	}
}

func TestAuditRecordsMetaAndUnroutableAttempts(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	setAuditPolicy(t, resolver, func(p *registry.AuditPolicy) { p.ResultMode = "errors" })
	ext := externalRegistry(t, resolver)
	setGovernance(t, ext, func(g *registry.GovernanceDoc) { g.Discovery = "lazy" })
	_, c, _ := startGateway(t, Config{
		ClientID: "audit-attempts", Resolver: resolver, Secrets: auditSecretResolver(auditTestKey()),
		Dial: scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, metaNames...)
	if resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: discovery.MetaStatus, Arguments: []byte(`{}`)}); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	if resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "does-not-exist", Arguments: []byte(`{"x":1}`)}); resp.Error == nil {
		t.Fatal("unknown tool succeeded")
	}
	events := readAuditEvents(t, resolver)
	if len(events) != 4 {
		t.Fatalf("events = %d, want two received+finished lifecycles: %+v", len(events), events)
	}
	finished := 0
	protocolErrors := 0
	for _, e := range events {
		if e.Kind == accesslog.EventRouted {
			t.Fatalf("meta/unroutable attempt unexpectedly routed: %+v", e)
		}
		if e.Kind == accesslog.EventFinished {
			finished++
			if e.Outcome == "protocol_error" {
				protocolErrors++
				if e.Result == nil {
					t.Fatal("errors mode omitted the protocol error payload")
				}
			}
		}
	}
	if finished != 2 || protocolErrors != 1 {
		t.Fatalf("finished=%d protocol_errors=%d events=%+v", finished, protocolErrors, events)
	}
}

func TestAuditRecordsUnsupportedProtocolToolAttempt(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver)
	setAuditPolicy(t, resolver, func(p *registry.AuditPolicy) { p.ResultMode = "errors" })
	_, c, _ := startGateway(t, Config{
		ClientID: "audit-protocol", Resolver: resolver, Secrets: auditSecretResolver(auditTestKey()),
	})
	raw := json.RawMessage(`{"name":"status","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}`)
	resp := c.call(mcp.MethodToolsCall, raw)
	if resp.Error == nil || resp.Error.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Fatalf("response = %+v, want unsupported protocol", resp)
	}
	events := readAuditEvents(t, resolver)
	if len(events) != 2 {
		t.Fatalf("events = %d, want received+finished: %+v", len(events), events)
	}
	if events[0].Protocol != "2099-01-01" || events[1].Outcome != "protocol_error" || events[1].Result == nil {
		t.Fatalf("protocol attempt events = %+v", events)
	}
}

func TestAuditUnavailableBlocksBeforeTheGateChain(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	setAuditPolicy(t, resolver, func(p *registry.AuditPolicy) {})
	g, c, _ := startGateway(t, Config{
		ClientID: "audit-missing-key", Resolver: resolver,
		Secrets: func(context.Context, secrets.Ref) (string, bool, error) { return "", false, nil },
		Dial:    scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")
	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__echo", Arguments: []byte(`{}`)})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "access ledger") {
		t.Fatalf("response = %+v, want audit refusal", resp)
	}
	for stage, n := range g.pipe.Counters() {
		if n != 0 {
			t.Errorf("pipeline stage %s ran %d times despite pre-execution audit failure", stage, n)
		}
	}
	if events := readAuditEvents(t, resolver); len(events) != 0 {
		t.Fatalf("events without a usable key = %+v", events)
	}
}

func TestAuditRecordsGateDenial(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1", "s2")
	setAuditPolicy(t, resolver, func(p *registry.AuditPolicy) { p.ResultMode = "errors" })
	ext := externalRegistry(t, resolver)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Profiles.V.Profiles["team"] = registry.Doc[registry.Profile]{V: registry.Profile{Servers: []string{"s1"}}}
		tx.Clients.V.Clients["audit-deny"] = registry.Doc[registry.ClientEntry]{V: registry.ClientEntry{Profile: "team"}}
	})
	_, c, _ := startGateway(t, Config{
		ClientID: "audit-deny", Resolver: resolver, Secrets: auditSecretResolver(auditTestKey()),
		Dial: scriptedDial(map[string]*fakemcp.Script{
			"s1": fakemcp.Minimal("echo"), "s2": fakemcp.Minimal("echo"),
		}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "s1__echo")
	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "s2__echo", Arguments: []byte(`{"denied":true}`)})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "E_SCOPE_DENIED") {
		t.Fatalf("denial response = %+v", resp)
	}
	events := readAuditEvents(t, resolver)
	if len(events) != 3 {
		t.Fatalf("denial events = %d, want 3: %+v", len(events), events)
	}
	finished := eventKind(t, events, accesslog.EventFinished)
	if finished.Outcome != "denied" || finished.Gate != "scope" || finished.Code != "E_SCOPE_DENIED" || finished.Result == nil {
		t.Fatalf("denial event = %+v", finished)
	}
}

func TestAuditRecordsCancellationWithoutReply(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "hang")
	setAuditPolicy(t, resolver, func(p *registry.AuditPolicy) { p.ResultMode = "none" })
	script := fakemcp.Minimal("stuck").With(fakemcp.NeverRespond(mcp.MethodToolsCall))
	g, c, _ := startGateway(t, Config{
		ClientID: "audit-cancel", Resolver: resolver, Secrets: auditSecretResolver(auditTestKey()),
		Dial: scriptedDial(map[string]*fakemcp.Script{"hang": script}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "hang__stuck")
	id := c.send(mcp.MethodToolsCall, mcp.CallToolParams{Name: "hang__stuck", Arguments: []byte(`{"wait":true}`)})
	waitFor(t, "audited call in flight", func() bool { return g.inflightLen() == 1 })
	c.notify(mcp.NotificationCancelled, mcp.CancelledParams{RequestID: id, Reason: "test"})
	waitFor(t, "audited call cancelled", func() bool { return g.inflightLen() == 0 })
	if c.hasResponse(id) {
		t.Fatal("cancelled call received a response")
	}
	events := readAuditEvents(t, resolver)
	if len(events) != 3 {
		t.Fatalf("cancellation events = %d, want 3: %+v", len(events), events)
	}
	finished := eventKind(t, events, accesslog.EventFinished)
	if finished.Outcome != "cancelled" || finished.Result != nil {
		t.Fatalf("cancellation event = %+v", finished)
	}
}

func TestAuditEnableHotReloadsIntoRunningGateway(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	g, c, _ := startGateway(t, Config{
		ClientID: "audit-hot", Resolver: resolver, Secrets: auditSecretResolver(auditTestKey()),
		Dial: scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")
	setAuditPolicy(t, resolver, func(p *registry.AuditPolicy) { p.ResultMode = "none" })
	waitFor(t, "audit policy hot reload", func() bool {
		g.audit.mu.Lock()
		defer g.audit.mu.Unlock()
		return g.audit.policy.Enabled && g.audit.store != nil
	})
	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__echo", Arguments: []byte(`{"hot":true}`)})
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	if events := readAuditEvents(t, resolver); len(events) != 3 {
		t.Fatalf("hot-reloaded audit events = %d, want 3: %+v", len(events), events)
	}
}
