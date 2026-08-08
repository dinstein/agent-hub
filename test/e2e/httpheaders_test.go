package e2e_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// The HTTP face's protocol headers had no end-to-end coverage. httpplane and
// httpplanegates drive authentication, tiers, allowlists and revocation —
// everything about WHO is calling — and nothing about the three headers MCP
// requires a caller to put on the request itself.
//
// They are worth reaching through a real daemon rather than a recording
// dispatcher, because a header is the one part of a request that intermediates
// can rewrite. The rule the implementation gives the threat for is that an
// intermediary routing on `Mcp-Method` while the body says something else must
// not be able to make those two disagree — which only means something if the
// refusal survives the whole stack that a proxy would sit in front of.
//
// The version header carries a second rule the `_meta` comparison structurally
// cannot see, and it is the one that was missing once: a header naming a
// version this server does not speak is refused on EVERY verb, including
// DELETE, which carries no body to compare against at all.
//
// That verb also answers in a different SHAPE, and finding out why is half of
// what these cases record. See TestTheVersionHeaderRuleAlsoBindsDelete.

// headerFixture starts a daemon serving one downstream and returns a client
// holding a valid token, so every refusal below is about the header under test
// and not about authorization.
func headerFixture(t *testing.T) *httpPlaneClient {
	t.Helper()
	dataDir, socket, env := sandbox(t)
	runAgenthubEnv(t, env, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "alpha", "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")

	token := mintToken(t, env, "headers")
	url := startHTTPDaemon(t, dataDir, socket, env)
	c := &httpPlaneClient{t: t, url: url, token: token}
	c.waitReady(30 * time.Second)
	c.waitForTool("alpha__echo", 45*time.Second)
	return c
}

// rpcErrorOf decodes the JSON-RPC error out of a refusal body.
func rpcErrorOf(t *testing.T, raw []byte) *rpcError {
	t.Helper()
	var m rpcMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("refusal body is not JSON-RPC: %v\n%s", err, raw)
	}
	if m.Error == nil {
		t.Fatalf("refusal body carries no error: %s", raw)
	}
	return m.Error
}

// listBody is a well-formed tools/list request, used as the neutral payload
// the header cases vary around.
func listBody(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAVersionHeaderNamingAnUnknownVersionIsRefused covers the rule on a POST,
// where the caller may be a 2026 client and is owed the machine-readable
// refusal its revision defines.
//
// The list is the same promise `server/discover` advertises and `initialize`
// echoes, so it is asserted exactly: a version this server would negotiate
// must never be refused in a header, and one it would not negotiate must never
// be accepted in one.
func TestAVersionHeaderNamingAnUnknownVersionIsRefused(t *testing.T) {
	c := headerFixture(t)

	status, _, raw := c.doWith(http.MethodPost, listBody(t),
		map[string]string{"MCP-Protocol-Version": "1999-01-01"})
	if status != http.StatusBadRequest {
		t.Fatalf("an unknown version header answered HTTP %d, want 400\n%s", status, raw)
	}
	if e := rpcErrorOf(t, raw); e.Code != -32022 {
		t.Fatalf("error code = %d, want -32022 (UnsupportedProtocolVersion): %s", e.Code, raw)
	}
	// The payload is what makes the refusal actionable: the client is told to
	// retry with a version from the list, so the list has to be in the answer.
	var body struct {
		Error struct {
			Data struct {
				Supported []string `json:"supported"`
				Requested string   `json:"requested"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("refusal body: %v\n%s", err, raw)
	}
	want := []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}
	if !slices.Equal(body.Error.Data.Supported, want) {
		t.Errorf("supported = %v, want %v", body.Error.Data.Supported, want)
	}
	if body.Error.Data.Requested != "1999-01-01" {
		t.Errorf("requested = %q, want the value that was refused", body.Error.Data.Requested)
	}
}

// TestTheVersionHeaderRuleAlsoBindsDelete is the half that proves the rule is
// not the body's `_meta` comparison wearing a different name. DELETE carries
// no body at all, so nothing about it can be checked against one — and the
// specification still requires the refusal.
//
// **The refusal shape differs from the POST above, and that is correct.**
// DELETE exists to end a session, and 2026-07-28 removed both `Mcp-Session-Id`
// and DELETE-on-close — so the only caller that ever sends this verb is a
// ≤ 2025-11-25 client, whose revisions require the 400 and say nothing about
// the body. It gets the daemon's ordinary control-plane error envelope rather
// than a JSON-RPC `-32022` with a `supported` list it was never promised and
// could not read. Asserted here, rather than left to be discovered, because a
// reader who found the two verbs disagreeing would reasonably suspect a bug.
func TestTheVersionHeaderRuleAlsoBindsDelete(t *testing.T) {
	c := headerFixture(t)

	status, _, raw := c.doWith(http.MethodDelete, nil,
		map[string]string{"MCP-Protocol-Version": "1999-01-01"})
	if status != http.StatusBadRequest {
		t.Fatalf("DELETE with an unknown version header answered HTTP %d, want 400\n%s", status, raw)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("DELETE refusal body: %v\n%s", err, raw)
	}
	if envelope.Error.Code != "bad_request" {
		t.Errorf("DELETE refusal code = %q, want the control-plane bad_request: %s",
			envelope.Error.Code, raw)
	}
	if !strings.Contains(envelope.Error.Message, "1999-01-01") {
		t.Errorf("the refusal does not name the version it refused: %s", raw)
	}

	// Without the header the same DELETE is served, so the 400 above is the
	// version rule and not this verb being unsupported.
	if status, _, raw = c.doWith(http.MethodDelete, nil, nil); status != http.StatusNoContent {
		t.Fatalf("a plain DELETE answered HTTP %d, want 204\n%s", status, raw)
	}
}

// TestAVersionHeaderThisServerSpeaksIsAccepted is the other half, and the one
// that keeps the case above from passing on a server that refuses the header
// outright.
//
// Absence is included deliberately: it is a SEPARATE rule, not a lenient
// reading of this one. The header postdates 2025-03-26, and the specification
// tells a server to read its absence as that version — so refusing it would
// lock out every client older than the header itself.
func TestAVersionHeaderThisServerSpeaksIsAccepted(t *testing.T) {
	c := headerFixture(t)

	for _, v := range []string{"", "2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"} {
		name := v
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			extra := map[string]string{}
			if v != "" {
				extra["MCP-Protocol-Version"] = v
			}
			status, _, raw := c.doWith(http.MethodPost, listBody(t), extra)
			if status != http.StatusOK {
				t.Fatalf("version header %q answered HTTP %d, want 200\n%s", v, status, raw)
			}
		})
	}
}

// TestALyingMcpMethodHeaderIsRefused pins the rule the implementation gives
// the threat model for: an intermediary routes on the header while the body
// says something else, so the two disagreeing is never tolerable — "whatever
// the session's protocol generation", which is why this case sends no `_meta`
// at all and still expects a refusal.
func TestALyingMcpMethodHeaderIsRefused(t *testing.T) {
	c := headerFixture(t)

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "alpha__echo", "arguments": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _, raw := c.doWith(http.MethodPost, body,
		map[string]string{"Mcp-Method": "tools/list"})
	if status != http.StatusBadRequest {
		t.Fatalf("a header/body method disagreement answered HTTP %d, want 400\n%s", status, raw)
	}
	if e := rpcErrorOf(t, raw); e.Code != -32020 {
		t.Fatalf("error code = %d, want -32020 (HeaderMismatch): %s", e.Code, raw)
	}

	// And the truthful header is accepted, so the refusal above is about the
	// disagreement rather than about the header being present at all.
	status, _, raw = c.doWith(http.MethodPost, body,
		map[string]string{"Mcp-Method": "tools/call"})
	if status != http.StatusOK {
		t.Fatalf("a truthful Mcp-Method answered HTTP %d, want 200\n%s", status, raw)
	}
}

// TestAnMcpNameHeaderThatDisagreesWithTheParamsIsRefused is the same rule one
// level down: the header names the tool, and an intermediary that routed or
// audited on it must not be able to see a different name than the pipeline
// does.
func TestAnMcpNameHeaderThatDisagreesWithTheParamsIsRefused(t *testing.T) {
	c := headerFixture(t)

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "alpha__echo", "arguments": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _, raw := c.doWith(http.MethodPost, body, map[string]string{
		"Mcp-Method": "tools/call",
		"Mcp-Name":   "alpha__something_else",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("a header/body name disagreement answered HTTP %d, want 400\n%s", status, raw)
	}
	if e := rpcErrorOf(t, raw); e.Code != -32020 {
		t.Fatalf("error code = %d, want -32020 (HeaderMismatch): %s", e.Code, raw)
	}

	status, _, raw = c.doWith(http.MethodPost, body, map[string]string{
		"Mcp-Method": "tools/call",
		"Mcp-Name":   "alpha__echo",
	})
	if status != http.StatusOK {
		t.Fatalf("a truthful Mcp-Name answered HTTP %d, want 200\n%s", status, raw)
	}
}

// TestARequestCarryingMetaMustCarryMcpMethod covers the presence rule, which
// only binds the 2026 shape: a stateful session owes no headers, so the same
// body without `_meta` must go through untouched.
//
// The pair is what makes it a rule rather than a coincidence — a server that
// simply required Mcp-Method on everything would pass the first half and
// break every pre-2026 client.
func TestARequestCarryingMetaMustCarryMcpMethod(t *testing.T) {
	c := headerFixture(t)

	withMeta, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
		"params": map[string]any{"_meta": meta2026()},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _, raw := c.doWith(http.MethodPost, withMeta, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("a _meta request without Mcp-Method answered HTTP %d, want 400\n%s", status, raw)
	}
	if e := rpcErrorOf(t, raw); e.Code != -32020 {
		t.Fatalf("error code = %d, want -32020: %s", e.Code, raw)
	}

	// With the header it is served.
	status, _, raw = c.doWith(http.MethodPost, withMeta,
		map[string]string{"Mcp-Method": "tools/list"})
	if status != http.StatusOK {
		t.Fatalf("a _meta request WITH Mcp-Method answered HTTP %d, want 200\n%s", status, raw)
	}

	// And the same body minus _meta owes nothing: a stateful caller that
	// never heard of these headers must not be refused for omitting them.
	status, _, raw = c.doWith(http.MethodPost, listBody(t), nil)
	if status != http.StatusOK {
		t.Fatalf("a stateful request without Mcp-Method answered HTTP %d, want 200\n%s", status, raw)
	}
}
