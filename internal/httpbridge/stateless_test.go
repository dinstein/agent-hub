package httpbridge_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
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
// request without a session is still refused (a stateful client skipping
// initialize is still an error) — with 400; see
// TestSessionMissStatusesStayDistinct.
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

// A legacy request without a session is still refused — sessionless entry is
// a property of the 2026 shapes, not a general bypass — but with 400, not
// 404. The specification asks for that split in all three ≤ 2025-11-25
// revisions, and the reason is the client rule attached to 404: "start a new
// session". A caller that omits the header therefore re-initialized and
// omitted it again, filling the session table in 256 rounds and taking the
// whole HTTP face to 503.
func TestLegacyRequestStillNeedsSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})
	res := h.post(t, "", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a legacy request that brought no session header", res.StatusCode)
	}
}

// TestSessionMissStatusesStayDistinct: the two misses the specification
// separates must not collapse back together, and the third 404 on this
// endpoint — an unknown PATH — must not move either.
//
// Presenting an id we reject is a different thing from presenting none. The
// frozen 404 body unifies every answer about an id that WAS presented, so
// the endpoint cannot be probed for which sessions exist; a request carrying
// no id probes nothing, which is why splitting it out costs that rule
// nothing.
func TestSessionMissStatusesStayDistinct(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})
	legacy := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	if res := h.post(t, "", "", legacy); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("no header: status = %d, want 400", res.StatusCode)
	}
	if res := h.post(t, "", "deadbeefdeadbeefdeadbeefdeadbeef", legacy); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unresolvable id: status = %d, want 404", res.StatusCode)
	}
	// An id that resolves still works, so the split did not break the path
	// it runs on.
	sid := h.initSession(t, "")
	if res := h.post(t, "", sid, legacy); res.StatusCode != http.StatusOK {
		t.Fatalf("live session: status = %d, want 200", res.StatusCode)
	}
	// DELETE takes the same split.
	for _, tc := range []struct {
		name, id string
		want     int
	}{
		{"no header", "", http.StatusBadRequest},
		{"unresolvable id", "deadbeefdeadbeefdeadbeefdeadbeef", http.StatusNotFound},
	} {
		req, err := http.NewRequest(http.MethodDelete, h.srv.URL+httpbridge.DefaultPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.id != "" {
			req.Header.Set(httpbridge.SessionHeader, tc.id)
		}
		res, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatalf("DELETE %s: status = %d, want %d", tc.name, res.StatusCode, tc.want)
		}
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
	// A body with no _meta has nothing to disagree with, so a header naming
	// another SUPPORTED version passes — there is no claim to contradict.
	// One this server does not speak is a different rule and is refused; see
	// TestProtocolVersionHeaderRefusesAnUnsupportedValue.
	sid := h.initSession(t, "")
	if res := h.post(t, "", sid, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`,
		httpbridge.ProtocolVersionHeader, "2025-03-26"); res.StatusCode != http.StatusOK {
		t.Fatalf("legacy body with a header: status = %d, want 200", res.StatusCode)
	}
}

// TestProtocolVersionHeaderRefusesAnUnsupportedValue: a header naming a
// version this server does not speak is 400, on every verb that can carry
// one.
//
// Every revision defining the header requires this and each words it as a
// MUST — 2025-06-18 and 2025-11-25 as a bare 400, 2026-07-28 as 400 plus the
// supported list. Until this test existed the value was never judged at all:
// the only check compared it against the version the body's _meta declares,
// and a ≤ 2025-11-25 request has no _meta, so the comparison was skipped for
// exactly the generations whose specification demands the refusal. The most
// pointed case is the first one below — a garbage version did not merely get
// answered, it got a session minted.
func TestProtocolVersionHeaderRefusesAnUnsupportedValue(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})

	t.Run("initialize mints no session", func(t *testing.T) {
		res := h.post(t, "", "", initFrame, httpbridge.ProtocolVersionHeader, "banana")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.StatusCode)
		}
		if sid := res.Header.Get(httpbridge.SessionHeader); sid != "" {
			t.Fatalf("a refused request minted session %q", sid)
		}
		e := rpcError(t, res)
		if e == nil || e.Code != mcp.CodeUnsupportedProtocolVersion {
			t.Fatalf("error = %+v, want CodeUnsupportedProtocolVersion", e)
		}
		// The payload is the whole point of answering -32022 rather than a
		// bare 400: a client told to change something must be told to what.
		var data mcp.UnsupportedVersionData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatalf("decode error data %s: %v", e.Data, err)
		}
		if data.Requested != "banana" || !slices.Equal(data.Supported, mcp.SupportedVersions) {
			t.Fatalf("data = %+v, want the refused version and the full supported list", data)
		}
	})

	t.Run("a real but unsupported revision", func(t *testing.T) {
		// 2024-11-05 is not a typo — it is the HTTP+SSE generation, which
		// this tree does not speak. "Invalid or unsupported" covers both.
		res := h.post(t, "", "", initFrame, httpbridge.ProtocolVersionHeader, "2024-11-05")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("notification", func(t *testing.T) {
		res := h.post(t, "", "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			httpbridge.ProtocolVersionHeader, "banana")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (not the 202 a notification usually earns)", res.StatusCode)
		}
	})

	t.Run("delete", func(t *testing.T) {
		// DELETE carries no body, so it checks the header on its own path.
		// "All subsequent requests" includes this verb, and a session id
		// that would otherwise resolve must not rescue a bad version.
		sid := h.initSession(t, "")
		req, err := http.NewRequest(http.MethodDelete, h.srv.URL+httpbridge.DefaultPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(httpbridge.SessionHeader, sid)
		req.Header.Set(httpbridge.ProtocolVersionHeader, "banana")
		res, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.StatusCode)
		}
	})
}

// TestProtocolVersionHeaderAcceptsEverySupportedVersion drives the list
// itself rather than a copy of it: the refusal above and the negotiation
// everywhere else must answer "do we speak this" the same way, and a version
// added to mcp.SupportedVersions without this face learning it would be
// advertised in server/discover and then refused in a header.
func TestProtocolVersionHeaderAcceptsEverySupportedVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &httpbridge.Authenticator{InsecureLoopback: true, Tokens: newStore(t)}, httpbridge.Options{})
	sid := h.initSession(t, "")
	for _, v := range mcp.SupportedVersions {
		res := h.post(t, "", sid, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`,
			httpbridge.ProtocolVersionHeader, v)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", v, res.StatusCode)
		}
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
