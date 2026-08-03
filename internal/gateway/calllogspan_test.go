package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

func ledgerTestKey() []byte { return bytes.Repeat([]byte{0x5a}, 32) }

func ledgerSecretResolver(key []byte) secrets.Resolver {
	encoded := base64.RawStdEncoding.EncodeToString(key)
	return func(_ context.Context, ref secrets.Ref) (string, bool, error) {
		if ref == secrets.CallsEncryptionRef() {
			return encoded, true, nil
		}
		return "", false, nil
	}
}

func setCallsPolicy(t *testing.T, resolver *platform.Resolver, mutate func(*registry.CallsPolicy)) {
	t.Helper()
	keyID, err := calllog.KeyID(ledgerTestKey())
	if err != nil {
		t.Fatal(err)
	}
	st := externalRegistry(t, resolver)
	updateRegistry(t, st, func(tx *registry.Tx) {
		p := registry.CallsPolicy{
			Enabled: true, Durability: "sync",
			ResultMode: "truncated", ResultBytes: registry.DefaultCallsResultBytes,
			RetentionDays: registry.DefaultCallsRetentionDays,
			MaxBytes:      registry.DefaultCallsMaxBytes, MinFreeBytes: registry.DefaultCallsMinFree,
			KeyID: keyID,
		}
		mutate(&p)
		tx.Governance.V.Audit = &registry.Doc[registry.CallsPolicy]{V: p}
	})
}

func readLedgerEvents(t *testing.T, resolver *platform.Resolver) []calllog.Event {
	t.Helper()
	root, err := calllog.DefaultDir(resolver)
	if err != nil {
		t.Fatal(err)
	}
	events, skipped, err := calllog.ReadEvents(root)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("skipped %d malformed audit events", skipped)
	}
	return events
}

func payloadOf(t *testing.T, resolver *platform.Resolver, ref *calllog.PayloadRef) []byte {
	t.Helper()
	if ref == nil {
		t.Fatal("missing payload reference")
	}
	root, err := calllog.DefaultDir(resolver)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := calllog.ReadPayload(root, *ref, ledgerTestKey())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func eventKind(t *testing.T, events []calllog.Event, kind calllog.EventKind) calllog.Event {
	t.Helper()
	for _, e := range events {
		if e.Kind == kind {
			return e
		}
	}
	t.Fatalf("no %s event in %+v", kind, events)
	return calllog.Event{}
}

// executedCall isolates the events of the ONE call that reached a downstream,
// and proves every other event belongs to an attempt the gateway refused as
// retryable-busy.
//
// callToolResult retries the busy error, because "downstream servers are still
// connecting" is a legitimate answer for as long as the pool is warming up —
// and the ledger records the refusal, correctly, as a two-event lifecycle. So
// the number of events in the file is a function of how many attempts the
// retry loop needed, which is a property of the machine, not of the ledger.
// Asserting on the total made this test fail on a slow or contended runner
// while the behaviour it exists to check was perfectly fine.
//
// The strictness is kept where it belongs: the executed call must have
// exactly its three events, and an event that is neither part of it nor part
// of a busy refusal still fails the test rather than being tolerated.
// toolCalls keeps the records of tools/call requests.
//
// Every upstream method is recorded now — initialize, tools/list and ping
// included — which is the point: a session that connected and then went
// quiet must not look like one that never connected. The assertions about
// ROUTING are still about tools/call, so they say so.
func toolCalls(events []calllog.Event) []calllog.Event {
	var out []calllog.Event
	for _, e := range events {
		if e.Method == mcp.MethodToolsCall {
			out = append(out, e)
		}
	}
	return out
}

func executedCall(t *testing.T, events []calllog.Event) []calllog.Event {
	t.Helper()
	events = toolCalls(events)
	var callID string
	for _, e := range events {
		if e.Kind == calllog.EventRouted {
			if callID != "" && e.CallID != callID {
				t.Fatalf("two calls routed; only one was made: %+v", events)
			}
			callID = e.CallID
		}
	}
	if callID == "" {
		t.Fatalf("no call reached a downstream: %+v", events)
	}
	var executed []calllog.Event
	for _, e := range events {
		if e.CallID == callID {
			executed = append(executed, e)
			continue
		}
		// Anything else must be a refused attempt: received, then finished
		// with the busy outcome. A third kind, or a different outcome, is an
		// unexplained record and must not pass silently.
		if e.Kind == calllog.EventReceived {
			continue
		}
		if e.Kind != calllog.EventFinished || e.Outcome != "busy" {
			t.Fatalf("unexplained audit event outside the executed call: %+v", e)
		}
	}
	if len(executed) != 3 {
		t.Fatalf("executed call has %d events, want the received/routed/finished three: %+v",
			len(executed), executed)
	}
	return executed
}

func TestAuditRecordsCompleteRequestRouteAndBoundedResult(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) {
		p.ResultMode, p.ResultBytes = "truncated", 24
	})
	_, c, _ := startGateway(t, Config{
		ClientID: "audited", Face: "http", Resolver: resolver,
		Secrets: ledgerSecretResolver(ledgerTestKey()),
		Dial:    scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")
	args := json.RawMessage(`{"password":"complete-secret","n":1}`)
	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__echo", Arguments: args})
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}

	events := toolCalls(readLedgerEvents(t, resolver))
	if len(events) != 3 {
		t.Fatalf("events = %d, want received+routed+finished: %+v", len(events), events)
	}
	received := eventKind(t, events, calllog.EventReceived)
	var request mcp.CallToolParams
	if err := json.Unmarshal(payloadOf(t, resolver, received.Request), &request); err != nil {
		t.Fatal(err)
	}
	if request.Name != "fake__echo" || !bytes.Equal(request.Arguments, args) {
		t.Fatalf("recorded request = %+v", request)
	}
	routed := eventKind(t, events, calllog.EventRouted)
	if routed.Server != "fake" || routed.Tool != "echo" || routed.Exposed != "fake__echo" || routed.Face != "http" {
		t.Fatalf("routed = %+v", routed)
	}
	if got := payloadOf(t, resolver, routed.EffectiveArgs); !bytes.Equal(got, args) {
		t.Fatalf("effective args = %s, want %s", got, args)
	}
	finished := eventKind(t, events, calllog.EventFinished)
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
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) { p.ResultMode = "none" })
	ext := externalRegistry(t, resolver)
	setGovernance(t, ext, func(g *registry.GovernanceDoc) { g.Discovery = "lazy" })
	_, c, _ := startGateway(t, Config{
		ClientID: "audit-lazy", Resolver: resolver, Secrets: ledgerSecretResolver(ledgerTestKey()),
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
	events := executedCall(t, readLedgerEvents(t, resolver))
	routed := eventKind(t, events, calllog.EventRouted)
	if routed.Exposed != discovery.MetaCallTool || routed.Server != "fake" || routed.Tool != "echo" {
		t.Fatalf("lazy route = %+v", routed)
	}
	if got := string(payloadOf(t, resolver, routed.EffectiveArgs)); got != `{"marker":"lazy-audit"}` {
		t.Fatalf("effective args = %s", got)
	}
	finished := eventKind(t, events, calllog.EventFinished)
	if finished.Result != nil || finished.ResultCapture != "none" {
		t.Fatalf("none result policy captured a result: %+v", finished)
	}
}

func TestAuditRecordsMetaAndUnroutableAttempts(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) { p.ResultMode = "errors" })
	ext := externalRegistry(t, resolver)
	setGovernance(t, ext, func(g *registry.GovernanceDoc) { g.Discovery = "lazy" })
	_, c, _ := startGateway(t, Config{
		ClientID: "audit-attempts", Resolver: resolver, Secrets: ledgerSecretResolver(ledgerTestKey()),
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
	events := toolCalls(readLedgerEvents(t, resolver))
	if len(events) != 4 {
		t.Fatalf("events = %d, want two received+finished lifecycles: %+v", len(events), events)
	}
	finished := 0
	protocolErrors := 0
	for _, e := range events {
		if e.Kind == calllog.EventRouted {
			t.Fatalf("meta/unroutable attempt unexpectedly routed: %+v", e)
		}
		if e.Kind == calllog.EventFinished {
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
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) { p.ResultMode = "errors" })
	_, c, _ := startGateway(t, Config{
		ClientID: "audit-protocol", Resolver: resolver, Secrets: ledgerSecretResolver(ledgerTestKey()),
	})
	raw := json.RawMessage(`{"name":"status","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}`)
	resp := c.call(mcp.MethodToolsCall, raw)
	if resp.Error == nil || resp.Error.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Fatalf("response = %+v, want unsupported protocol", resp)
	}
	events := readLedgerEvents(t, resolver)
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
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) {})
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
	if events := readLedgerEvents(t, resolver); len(events) != 0 {
		t.Fatalf("events without a usable key = %+v", events)
	}
}

func TestAuditCapacityBlocksBeforeTheGateChain(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) { p.MaxBytes = 1 })
	g, c, _ := startGateway(t, Config{
		ClientID: "audit-full", Resolver: resolver, Secrets: ledgerSecretResolver(ledgerTestKey()),
		Dial: scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")
	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__echo", Arguments: []byte(`{"must":"not execute"}`)})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "access ledger") {
		t.Fatalf("response = %+v, want storage-pressure refusal", resp)
	}
	for stage, n := range g.pipe.Counters() {
		if n != 0 {
			t.Errorf("pipeline stage %s ran %d times after hard-cap refusal", stage, n)
		}
	}
	root, err := calllog.DefaultDir(resolver)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := calllog.Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Bytes > 1 {
		t.Fatalf("ledger used %d bytes above configured one-byte cap", usage.Bytes)
	}
}

func TestAuditRecordsGateDenial(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1", "s2")
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) { p.ResultMode = "errors" })
	ext := externalRegistry(t, resolver)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Profiles.V.Profiles["team"] = registry.Doc[registry.Profile]{V: registry.Profile{Servers: []string{"s1"}}}
		tx.Clients.V.Clients["audit-deny"] = registry.Doc[registry.ClientEntry]{V: registry.ClientEntry{Profile: "team"}}
	})
	_, c, _ := startGateway(t, Config{
		ClientID: "audit-deny", Resolver: resolver, Secrets: ledgerSecretResolver(ledgerTestKey()),
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
	events := toolCalls(readLedgerEvents(t, resolver))
	if len(events) != 3 {
		t.Fatalf("denial events = %d, want 3: %+v", len(events), events)
	}
	finished := eventKind(t, events, calllog.EventFinished)
	if finished.Outcome != "denied" || finished.Gate != "scope" || finished.Code != "E_SCOPE_DENIED" || finished.Result == nil {
		t.Fatalf("denial event = %+v", finished)
	}
}

func TestAuditRecordsCancellationWithoutReply(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "hang")
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) { p.ResultMode = "none" })
	script := fakemcp.Minimal("stuck").With(fakemcp.NeverRespond(mcp.MethodToolsCall))
	g, c, _ := startGateway(t, Config{
		ClientID: "audit-cancel", Resolver: resolver, Secrets: ledgerSecretResolver(ledgerTestKey()),
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
	events := toolCalls(readLedgerEvents(t, resolver))
	if len(events) != 3 {
		t.Fatalf("cancellation events = %d, want 3: %+v", len(events), events)
	}
	finished := eventKind(t, events, calllog.EventFinished)
	if finished.Outcome != "cancelled" || finished.Result != nil {
		t.Fatalf("cancellation event = %+v", finished)
	}
}

func TestAuditEnableHotReloadsIntoRunningGateway(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	g, c, _ := startGateway(t, Config{
		ClientID: "audit-hot", Resolver: resolver, Secrets: ledgerSecretResolver(ledgerTestKey()),
		Dial: scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) { p.ResultMode = "none" })
	waitFor(t, "audit policy hot reload", func() bool {
		g.audit.mu.Lock()
		defer g.audit.mu.Unlock()
		return g.audit.policy.Enabled && g.audit.store != nil
	})
	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__echo", Arguments: []byte(`{"hot":true}`)})
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	if events := toolCalls(readLedgerEvents(t, resolver)); len(events) != 3 {
		t.Fatalf("hot-reloaded audit events = %d, want 3: %+v", len(events), events)
	}
}

// Everything a client asks of agenthub is recorded, not only what it routes —
// and on the SAME path, so these carry payloads and outcomes exactly as a
// routed call does rather than a second, thinner shape.
//
// The question this answers is the first one anybody brings to the ledger:
// did this client reach us at all. Before it, a session that initialized,
// listed its tools and then went quiet left exactly the same trace as one
// that never connected — none.
func TestAuditRecordsEveryUpstreamMethod(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) {})
	_, c, _ := startGateway(t, Config{
		ClientID: "audit-methods", Resolver: resolver,
		Secrets: ledgerSecretResolver(ledgerTestKey()),
		Dial:    scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")
	if resp := c.call(mcp.MethodPing, nil); resp.Error != nil {
		t.Fatal(resp.Error)
	}

	events := readLedgerEvents(t, resolver)
	byMethod := map[string][]calllog.Event{}
	for _, e := range events {
		byMethod[e.Method] = append(byMethod[e.Method], e)
	}
	for _, method := range []string{mcp.MethodInitialize, mcp.MethodToolsList, mcp.MethodPing} {
		got := byMethod[method]
		if len(got) != 2 {
			t.Fatalf("%s produced %d records, want received+finished: %+v", method, len(got), got)
		}
		if got[0].Kind != calllog.EventReceived || got[1].Kind != calllog.EventFinished {
			t.Errorf("%s pair = %s, %s", method, got[0].Kind, got[1].Kind)
		}
		// No routing between them: these reach no downstream, and a `routed`
		// record would claim one was chosen. The outcome comes from the
		// response itself, on the same path a tools/call takes, so it is the
		// same vocabulary rather than a second one for these.
		if got[1].Server != "" || got[1].Outcome != "success" {
			t.Errorf("%s finished = %+v, want no server and outcome=success", method, got[1])
		}
	}
}

// Which of agenthub's OWN surfaces a call reached is recorded, because the
// exposed name cannot be read back into it: the same name means different
// things under different discovery modes, and "the client called the server"
// and "the client asked the hub, which called the server" are different
// facts about the same call id.
func TestAuditRecordsWhichSurfaceWasCalled(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) { p.ResultMode = "none" })
	setGovernance(t, externalRegistry(t, resolver), func(g *registry.GovernanceDoc) { g.Discovery = "lazy" })
	_, c, _ := startGateway(t, Config{
		ClientID: "audit-surface", Resolver: resolver,
		Secrets: ledgerSecretResolver(ledgerTestKey()),
		Dial:    scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, metaNames...)
	res := callToolResult(t, c, discovery.MetaCallTool, map[string]any{
		"tool": "fake__echo", "arguments": map[string]any{"marker": "surface"},
	})
	if res.IsError {
		t.Fatal(resultText(t, res))
	}

	events := executedCall(t, readLedgerEvents(t, resolver))
	routed := eventKind(t, events, calllog.EventRouted)
	// A meta surface AND a real downstream target under one call id: the hub
	// was asked, and the hub called the server.
	if routed.Surface != "meta" {
		t.Errorf("surface = %q, want meta", routed.Surface)
	}
	if routed.Server != "fake" || routed.Tool != "echo" {
		t.Errorf("the meta call did not record its real target: %+v", routed)
	}
}

// The client's own interaction data is stored for an unrouted request too.
// `tools/list` is the case that matters: its result is the catalog agenthub
// showed that client, which is not recoverable from anywhere else once the
// configuration moves.
func TestLedgerCapturesPayloadsForUnroutedRequests(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	setCallsPolicy(t, resolver, func(p *registry.CallsPolicy) { p.ResultMode = "full" })
	_, c, _ := startGateway(t, Config{
		ClientID: "ledger-unrouted", Resolver: resolver,
		Secrets: ledgerSecretResolver(ledgerTestKey()),
		Dial:    scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")

	var finished calllog.Event
	for _, e := range readLedgerEvents(t, resolver) {
		if e.Method == mcp.MethodToolsList && e.Kind == calllog.EventFinished {
			finished = e
		}
	}
	if finished.CallID == "" {
		t.Fatal("no finished record for tools/list")
	}
	if finished.Result == nil {
		t.Fatal("the catalog agenthub showed this client was not stored")
	}
	if body := string(payloadOf(t, resolver, finished.Result)); !strings.Contains(body, "fake__echo") {
		t.Errorf("stored tools/list result does not hold the catalog: %s", body)
	}
}
