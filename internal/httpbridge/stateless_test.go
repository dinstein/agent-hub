package httpbridge_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/mcp"
)

const discoverFrame = `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`

const metaListFrame = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`

const metaCallFrame = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"name":"srv__echo","arguments":{}}}`

// rpcError decodes the JSON-RPC error object of a response body.
func rpcError(t *testing.T, res *http.Response) *mcp.Error {
	t.Helper()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var resp mcp.Response
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("body %q is not a JSON-RPC response: %v", body, err)
	}
	return resp.Error
}

// 2026-07-28 removed the session header from the wire: the stateless shapes
// pass with no session and no session is minted for them, while a legacy
// request without a session keeps its 404 (a stateful client skipping
// initialize is still an error).
func TestStatelessShapesNeedNoSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})

	for name, frame := range map[string]string{
		"discover":        discoverFrame,
		"tools/list+meta": metaListFrame,
		"tools/call+meta": metaCallFrame,
	} {
		res := h.post(t, "", "", frame, httpbridge.MethodHeader, methodOf(frame))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 without a session", name, res.StatusCode)
		}
		if sid := res.Header.Get(httpbridge.SessionHeader); sid != "" {
			t.Fatalf("%s: a session id %q was minted; 2026-07-28 has no session header", name, sid)
		}
	}
	if n := h.bridge.Sessions(); n != 0 {
		t.Fatalf("session table holds %d entries after stateless traffic, want 0", n)
	}
	if n := h.disp.count(); n != 3 {
		t.Fatalf("dispatcher saw %d requests, want 3", n)
	}
}

// methodOf extracts the method member for the Mcp-Method header.
func methodOf(frame string) string {
	var m struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal([]byte(frame), &m)
	return m.Method
}

// A legacy request without a session stays a 404: sessionless entry is a
// property of the 2026 shapes, not a general bypass.
func TestLegacyRequestStillNeedsSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})
	res := h.post(t, "", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a sessionless legacy request", res.StatusCode)
	}
}

// A lying Mcp-Method header is -32020 with HTTP 400, whatever the session's
// protocol generation.
func TestMcpMethodHeaderMismatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})
	res := h.post(t, "", "", discoverFrame, httpbridge.MethodHeader, "tools/call")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if e := rpcError(t, res); e == nil || e.Code != mcp.CodeHeaderMismatch {
		t.Fatalf("error = %+v, want CodeHeaderMismatch", e)
	}
}

// A request carrying the 2026 _meta owes the Mcp-Method header; a stateful
// request owes nothing.
func TestMcpMethodHeaderRequiredOnlyWithMeta(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})

	res := h.post(t, "", "", metaListFrame) // _meta, no Mcp-Method
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for _meta without Mcp-Method", res.StatusCode)
	}
	if e := rpcError(t, res); e == nil || e.Code != mcp.CodeHeaderMismatch {
		t.Fatalf("error = %+v, want CodeHeaderMismatch", e)
	}

	sid := h.initSession(t, "")
	if res := h.post(t, "", sid, `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("legacy request without headers: status = %d, want 200", res.StatusCode)
	}
}

// Mcp-Name must agree with the params name when both are present.
func TestMcpNameHeaderMismatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})
	res := h.post(t, "", "", metaCallFrame,
		httpbridge.MethodHeader, "tools/call", httpbridge.NameHeader, "srv__other")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	e := rpcError(t, res)
	if e == nil || e.Code != mcp.CodeHeaderMismatch || !strings.Contains(e.Message, "srv__other") {
		t.Fatalf("error = %+v, want CodeHeaderMismatch naming the header value", e)
	}
}
