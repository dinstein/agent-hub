package httpbridge_test

import (
	"context"
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

// TestProtocolVersionHeaderMismatch: the header must equal the version the
// body's _meta declares, and a disagreement is -32020 with HTTP 400. The
// specification's rationale is the one this face already applies to
// Mcp-Method — an intermediary routing on the header while the server
// executes on the body — and agenthub's non-loopback token-authed bind is
// what makes that reachable rather than hypothetical.
//
// This is the rule test/mcpstub enforces on the server role it plays, with
// the comment that a stub which skips it certifies nothing. The shipped
// server skipped it until this test existed.
func TestProtocolVersionHeaderMismatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})
	res := h.post(t, "", "", discoverFrame,
		httpbridge.MethodHeader, "server/discover",
		httpbridge.ProtocolVersionHeader, "2025-11-25")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	e := rpcError(t, res)
	if e == nil || e.Code != mcp.CodeHeaderMismatch || !strings.Contains(e.Message, "2025-11-25") {
		t.Fatalf("error = %+v, want CodeHeaderMismatch naming the header value", e)
	}
}

// TestProtocolVersionHeaderAgreementAndAbsence: the matching header passes,
// and an ABSENT one is allowed. The absence carve-out is real — this server
// also serves clients from before 2025-06-18 defined the header, and the
// specification lets such a server read absence as 2025-03-26. Only a
// disagreement is refused.
func TestProtocolVersionHeaderAgreementAndAbsence(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})

	if res := h.post(t, "", "", discoverFrame,
		httpbridge.MethodHeader, "server/discover",
		httpbridge.ProtocolVersionHeader, "2026-07-28"); res.StatusCode != http.StatusOK {
		t.Fatalf("agreeing header: status = %d, want 200", res.StatusCode)
	}
	if res := h.post(t, "", "", discoverFrame,
		httpbridge.MethodHeader, "server/discover"); res.StatusCode != http.StatusOK {
		t.Fatalf("absent header: status = %d, want 200", res.StatusCode)
	}
	// A body with no _meta has nothing to disagree with, so even a header
	// naming another version passes — there is no claim to contradict.
	sid := h.initSession(t, "")
	if res := h.post(t, "", sid, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`,
		httpbridge.ProtocolVersionHeader, "2025-03-26"); res.StatusCode != http.StatusOK {
		t.Fatalf("legacy body with a header: status = %d, want 200", res.StatusCode)
	}
}

// TestStatusFollowsTheErrorCode covers this face's mapping from a JSON-RPC
// error onto the HTTP status the binding requires. The dispatcher is stubbed
// to return the codes the gateway produces — that the gateway produces them
// is internal/gateway's own tests; what is under test here is the status.
//
// Answering 200 for these is not harmless. The backward-compatibility flow
// has a client inspect the body only ON a 400, so a 200 carrying -32022
// means a client following it never reads the supported-version list it was
// told to retry with.
func TestStatusFollowsTheErrorCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		code       int
		frame      string
		method     string
		wantStatus int
	}{
		{
			name: "unsupported version is 400", code: mcp.CodeUnsupportedProtocolVersion,
			frame: metaListFrame, method: "tools/list", wantStatus: http.StatusBadRequest,
		},
		{
			name: "unknown method on a 2026 request is 404", code: mcp.CodeMethodNotFound,
			frame: metaListFrame, method: "tools/list", wantStatus: http.StatusNotFound,
		},
		{
			// Nothing the binding assigns a status to: a JSON-RPC error is
			// still a successful HTTP exchange.
			name: "an internal error stays 200", code: mcp.CodeInternalError,
			frame: metaListFrame, method: "tools/list", wantStatus: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			disp := &recordingDispatcher{}
			disp.exec = func(_ context.Context, _ *httpbridge.Caller, req *mcp.Request) *mcp.Response {
				return mcp.NewErrorResponse(req.ID, &mcp.Error{Code: tc.code, Message: "stub"})
			}
			h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)},
				httpbridge.Options{Dispatcher: disp})
			res := h.post(t, "", "", tc.frame, httpbridge.MethodHeader, tc.method)
			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			if e := rpcError(t, res); e == nil || e.Code != tc.code {
				t.Fatalf("error = %+v, want code %d in the body", e, tc.code)
			}
		})
	}
}

// TestLegacyUnknownMethodStaysTwoHundred: the 404 rule belongs to the
// 2026-07-28 binding, and a ≤ 2025-11-25 session never agreed to it.
// agenthub's own downstream client reads an HTTP 404 as a dropped session
// and re-initializes, so answering a legacy caller's unknown method with one
// would turn "no such method" into a reconnect loop.
func TestLegacyUnknownMethodStaysTwoHundred(t *testing.T) {
	t.Parallel()
	disp := &recordingDispatcher{}
	disp.exec = func(_ context.Context, _ *httpbridge.Caller, req *mcp.Request) *mcp.Response {
		return mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeMethodNotFound, Message: "stub"})
	}
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)},
		httpbridge.Options{Dispatcher: disp})
	sid := h.initSession(t, "")
	res := h.post(t, "", sid, `{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("legacy unknown method: status = %d, want 200", res.StatusCode)
	}
	if e := rpcError(t, res); e == nil || e.Code != mcp.CodeMethodNotFound {
		t.Fatalf("error = %+v, want CodeMethodNotFound in the body", e)
	}
}
