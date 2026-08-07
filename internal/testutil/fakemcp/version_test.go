package fakemcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// Which protocol generation the fake speaks used to be an assumption rather
// than a setting: it answered initialize and nothing else, so every stdio
// test in the tree ran the pre-2026 path and the 2026 one was proven only by
// test/mcpstub, which is an httptest.Server and therefore proves things about
// streamable HTTP (docs/mcp-2026-07-28.md §7.7).
//
// Script.SupportedVersions is that setting. The cases below cover the three
// outcomes a discover can have and the adversarial half of the stateless
// protocol, over a REAL child process — the same transport.conn that
// downstream.Connect wires a downstream into.
//
// The subprocess is not a detail. transport.Handshake refuses to negotiate
// 2026 over a transport that cannot inject the per-request _meta, and
// negotiatedSetter's method is unexported, so no type outside package
// transport can implement it — fakemcp.Connect's in-process pipe included.
// 2026 over stdio is reachable only through SpawnStdio, which is why every
// case here spawns.

// TestSubprocessNegotiates2026 is the stateless acceptance path: a fake that
// advertises 2026-07-28 and then REQUIRES the per-request _meta on
// everything afterwards, driven by the real client.
//
// The strictness is what makes it a test rather than a demonstration. The
// fake refuses any post-handshake request whose _meta is missing, whose
// clientCapabilities key is absent, or whose version it does not speak — so
// tools/list and tools/call answering at all is proof the client injected a
// conformant _meta into each of them, on a transport where nothing else
// would have noticed if it had not.
func TestSubprocessNegotiates2026(t *testing.T) {
	ctx := testCtx(t)
	sc := fakemcp.Minimal()
	sc.SupportedVersions = []string{mcp.Version2026}
	tr := spawn(t, sc)

	res, err := transport.Handshake(ctx, tr, clientInfo)
	if err != nil {
		t.Fatalf("handshake: %v (stderr: %q)", err, tr.Stderr())
	}
	if res.Version != mcp.Version2026 {
		t.Fatalf("negotiated %q, want %q", res.Version, mcp.Version2026)
	}
	// The identity came out of the discover result's _meta, which is the only
	// place a 2026 server puts it — a fake still answering the legacy shape
	// would leave this empty.
	if res.ServerInfo.Name != "fakemcp" {
		t.Fatalf("serverInfo %+v, want the identity from the discover _meta", res.ServerInfo)
	}

	raw, err := tr.Call(ctx, mcp.MethodToolsList, nil)
	if err != nil {
		t.Fatalf("tools/list on a 2026 session: %v (stderr: %q)", err, tr.Stderr())
	}
	var list mcp.ListToolsResult
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want one tool named echo", list.Tools)
	}

	raw, err = tr.Call(ctx, mcp.MethodToolsCall,
		mcp.CallToolParams{Name: "echo", Arguments: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatalf("tools/call on a 2026 session: %v (stderr: %q)", err, tr.Stderr())
	}
	var cr mcp.CallResult
	if err := json.Unmarshal(raw, &cr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cr.Content), `{\"x\":1}`) {
		t.Fatalf("echo content %s does not carry the arguments", cr.Content)
	}
}

// TestSubprocessDiscoverNegotiatesLegacy covers the middle outcome, which is
// the one with no obvious shape: the server ANSWERS server/discover — so it
// is not an old server — but the version negotiated out of its list still
// requires the stateful handshake, and initialize must run after the discover
// rather than instead of it.
//
// The script pins that with a discriminator rather than by inspecting
// traffic: discover advertises 2025-11-25 while initialize echoes
// 2025-06-18. Those are both supported versions, so whichever handshake
// actually decided the outcome is readable in the negotiated version alone.
// A client that stopped at discover would report 2025-11-25.
func TestSubprocessDiscoverNegotiatesLegacy(t *testing.T) {
	ctx := testCtx(t)
	sc := fakemcp.Minimal()
	sc.SupportedVersions = []string{mcp.Version2025}
	sc.ProtocolVersion = "2025-06-18"
	tr := spawn(t, sc)

	res, err := transport.Handshake(ctx, tr, clientInfo)
	if err != nil {
		t.Fatalf("handshake: %v (stderr: %q)", err, tr.Stderr())
	}
	if res.Version != "2025-06-18" {
		t.Fatalf("negotiated %q, want 2025-06-18 — the initialize echo, "+
			"proving the stateful handshake ran after the discover", res.Version)
	}
}

// TestSubprocessDiscoverNoMutualVersion is the third outcome: the server
// answers discover and offers nothing this tree speaks.
//
// It must be a FATAL handshake failure rather than a fallback to initialize.
// A server that answered discover has proven it is modern, and falling back
// would send it the one method it need not implement — turning a legible
// version disagreement into a dead connection.
func TestSubprocessDiscoverNoMutualVersion(t *testing.T) {
	ctx := testCtx(t)
	sc := fakemcp.Minimal()
	sc.SupportedVersions = []string{"1999-01-01"}
	tr := spawn(t, sc)

	_, err := transport.Handshake(ctx, tr, clientInfo)
	if err == nil {
		t.Fatal("handshake succeeded against a server offering no supported version")
	}
	if !errors.Is(err, mcp.ErrUnsupportedVersion) {
		t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
	}
}

// TestSubprocessWithoutSupportedVersionsStaysLegacy is the regression guard
// on the default, and it guards more than this file.
//
// SupportedVersions is additive: every script in the tree written before it
// existed leaves it empty and must go on getting the pre-2026 handshake. The
// mechanism is that server/discover answers method-not-found, which
// transport.Handshake reads as "alive but old". If that default ever
// inverted, every stdio test in the repository would silently change which
// protocol it was exercising.
func TestSubprocessWithoutSupportedVersionsStaysLegacy(t *testing.T) {
	ctx := testCtx(t)
	tr := spawn(t, fakemcp.Minimal())

	res, err := transport.Handshake(ctx, tr, clientInfo)
	if err != nil {
		t.Fatalf("handshake: %v (stderr: %q)", err, tr.Stderr())
	}
	if res.Version != mcp.ProtocolVersion {
		t.Fatalf("negotiated %q, want the legacy %q", res.Version, mcp.ProtocolVersion)
	}
}

// --- adversarial: what a 2026 fake refuses -------------------------------
//
// These drive raw frames rather than the client, because the client is
// correct: it always injects _meta, so nothing it can send would exercise
// the refusals. What is under test here is the FAKE's strictness — the thing
// that gives the cases above their meaning, since a permissive fake would
// have passed them whatever the client sent.

// serveRaw feeds hand-written frames to one Serve call and returns the
// responses. It needs no goroutine: the input is a finite reader, so Serve
// handles every frame and returns on EOF.
func serveRaw(t *testing.T, sc *fakemcp.Script, frames ...string) []mcp.Response {
	t.Helper()
	var in bytes.Buffer
	for _, f := range frames {
		in.WriteString(f)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if err := fakemcp.Serve(context.Background(), &in, &out, io.Discard, sc); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var resps []mcp.Response
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var r mcp.Response
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("response %q: %v", line, err)
		}
		resps = append(resps, r)
	}
	return resps
}

// only2026 is the script the adversarial cases grade against.
func only2026() *fakemcp.Script {
	sc := fakemcp.Minimal()
	sc.SupportedVersions = []string{mcp.Version2026}
	return sc
}

func TestA2026ScriptRefusesBareRequests(t *testing.T) {
	const capsKey = `"io.modelcontextprotocol/clientCapabilities":{}`
	const verKey = `"io.modelcontextprotocol/protocolVersion":"2026-07-28"`

	cases := []struct {
		name   string
		params string
		code   int
	}{
		{
			// No _meta at all: the commonest way a client fails the
			// stateless protocol, because on every earlier revision this
			// request was complete as written.
			name: "no _meta", params: `{}`, code: mcp.CodeInvalidParams,
		},
		{
			// An empty clientCapabilities object is how a client says it has
			// no optional capabilities. Omitting the key is a different
			// statement, and the specification requires the key on every
			// request — so accepting the omission would let a client ship
			// the difference undetected.
			name: "no clientCapabilities", params: `{"_meta":{` + verKey + `}}`,
			code: mcp.CodeInvalidParams,
		},
		{
			// A version the server does not speak is -32022 and not a
			// generic invalid-params, because that code carries the
			// supported list the client is meant to retry from.
			name: "wrong version", params: `{"_meta":{` + capsKey +
				`,"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}`,
			code: mcp.CodeUnsupportedProtocolVersion,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resps := serveRaw(t, only2026(),
				`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":`+tc.params+`}`)
			if len(resps) != 1 {
				t.Fatalf("got %d responses, want 1", len(resps))
			}
			if resps[0].Error == nil {
				t.Fatalf("tools/list was ANSWERED: %s", resps[0].Result)
			}
			if resps[0].Error.Code != tc.code {
				t.Fatalf("error code = %d (%s), want %d",
					resps[0].Error.Code, resps[0].Error.Message, tc.code)
			}
		})
	}
}

// TestA2026ScriptAnswersDiscoverWithoutNegotiation pins the exemption the
// strictness must carry: server/discover arrives BEFORE any version is
// agreed, so refusing it for the version it declares would make the
// handshake unreachable — the fake would refuse the one request whose
// purpose is to find out what it speaks.
func TestA2026ScriptAnswersDiscoverWithoutNegotiation(t *testing.T) {
	resps := serveRaw(t, only2026(),
		`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("server/discover was refused: %+v", resps)
	}
	var dr mcp.DiscoverResult
	if err := json.Unmarshal(resps[0].Result, &dr); err != nil {
		t.Fatal(err)
	}
	if len(dr.SupportedVersions) != 1 || dr.SupportedVersions[0] != mcp.Version2026 {
		t.Fatalf("supportedVersions = %v", dr.SupportedVersions)
	}
	// The freshness members are required by the result's shape, and the
	// identity belongs in _meta rather than in a top-level member.
	if dr.TtlMs == nil || dr.CacheScope == "" {
		t.Errorf("discover result is not a CacheableResult: ttlMs=%v cacheScope=%q",
			dr.TtlMs, dr.CacheScope)
	}
	if dr.ServerInfo().Name != "fakemcp" {
		t.Errorf("discover _meta carries no serverInfo: %+v", dr.Meta)
	}
}

// --- scripted MRTR -------------------------------------------------------

// callFrame builds a tools/call frame carrying a conformant 2026 _meta, so
// these cases exercise the MRTR handling rather than the strictness above.
func callFrame(id int, extra string) string {
	const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientCapabilities":{}}`
	params := `{"name":"echo","arguments":{"a":1},` + meta
	if extra != "" {
		params += "," + extra
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":%s}}`, id, params)
}

// inputRequiredFirst is the script shape the MRTR cases share: answer the
// first tools/call with input_required, handle the retry normally.
func inputRequiredFirst() *fakemcp.Script {
	sc := only2026()
	return sc.With(fakemcp.Rule{
		Method:  mcp.MethodToolsCall,
		Call:    1,
		Actions: []fakemcp.Action{{Kind: fakemcp.ActInputRequired}},
	})
}

// TestInputRequiredActionWireShape pins what the interim result looks like
// on the wire. The client decides it is an MRTR round by reading resultType
// alone, so a fake that got that member wrong would be answering a complete
// result with input_required's body — and every MRTR test would pass by
// never entering the loop.
func TestInputRequiredActionWireShape(t *testing.T) {
	resps := serveRaw(t, inputRequiredFirst(), callFrame(1, ""))
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("tools/call was refused: %+v", resps)
	}
	var ir mcp.InputRequiredResult
	if err := json.Unmarshal(resps[0].Result, &ir); err != nil {
		t.Fatal(err)
	}
	if ir.ResultType != mcp.ResultTypeInputRequired {
		t.Fatalf("resultType = %q, want %q", ir.ResultType, mcp.ResultTypeInputRequired)
	}
	if ir.RequestState == nil || *ir.RequestState == "" {
		t.Fatalf("no requestState to echo: %+v", ir)
	}
	if req, ok := ir.InputRequests["roots"]; !ok || req.Method != mcp.MethodRootsList {
		t.Fatalf("inputRequests = %+v, want a roots/list under \"roots\"", ir.InputRequests)
	}
}

// TestInputRequiredRetryIsGraded is what gives every MRTR test its meaning.
//
// Two MUSTs govern the retry: echo the requestState back verbatim without
// inspecting or modifying it, and carry a NEW JSON-RPC id. Both can be
// broken while the exchange still appears to work from the client's side —
// the answers arrive either way — so if the fake accepted any retry, a
// client that dropped the blob or reused the id would leave every test above
// this layer green with nothing checking the parts the server owns.
func TestInputRequiredRetryIsGraded(t *testing.T) {
	const goodState = `"requestState":"fakemcp-request-state"`
	const answers = `"inputResponses":{"roots":{"roots":[]}}`
	cases := []struct {
		name    string
		retryID int
		retry   string // extra params members on the retry
		ok      bool
	}{
		{"echoed verbatim with a new id", 2, goodState + "," + answers, true},
		{"requestState dropped", 2, answers, false},
		{"requestState altered", 2,
			`"requestState":"not-what-was-handed-out",` + answers, false},
		// Same id as the call being retried: two distinct exchanges sharing
		// the id a response uses to find its caller.
		{"id reused", 1, goodState + "," + answers, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resps := serveRaw(t, inputRequiredFirst(),
				callFrame(1, ""), callFrame(tc.retryID, tc.retry))
			if len(resps) != 2 {
				t.Fatalf("got %d responses, want 2", len(resps))
			}
			if tc.ok {
				if resps[1].Error != nil {
					t.Fatalf("a correct retry was refused: %+v", resps[1].Error)
				}
				// The answers come back in structuredContent: the retry
				// re-sends the original arguments, so an echo of those alone
				// cannot show whether the responses arrived.
				var cr mcp.CallResult
				if err := json.Unmarshal(resps[1].Result, &cr); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(cr.StructuredContent), "roots") {
					t.Fatalf("the retry's inputResponses were not echoed: %s", cr.StructuredContent)
				}
				return
			}
			if resps[1].Error == nil {
				t.Fatalf("retry with a %s requestState was ACCEPTED: %s", tc.name, resps[1].Result)
			}
		})
	}
}

// TestALegacyScriptDemandsNoMeta is the other side of the exemption: a
// script that never declared SupportedVersions is a pre-2026 server, has no
// _meta to require, and must answer a bare request exactly as it always did.
// Without this, turning on strictness by inference would have broken every
// existing script in the tree.
func TestALegacyScriptDemandsNoMeta(t *testing.T) {
	resps := serveRaw(t, fakemcp.Minimal(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("a legacy script refused a bare tools/list: %+v", resps)
	}
	// And server/discover is method-not-found, which is what makes the real
	// client fall back to initialize.
	resps = serveRaw(t, fakemcp.Minimal(),
		`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`)
	if len(resps) != 1 || resps[0].Error == nil ||
		resps[0].Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("a legacy script did not answer discover with method-not-found: %+v", resps)
	}
}
