package downstream_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// mrtr_test.go covers the retry loop thoroughly, and covers it against a
// hand-written transport seam: the scripted server answers whatever the test
// says regardless of protocol, so what those cases prove is the loop's logic
// — how many rounds, which shapes fail closed, what the retry carries.
//
// What none of them touch is the loop running for real. docs/status/mcp-2026-07-28.md
// §7.7 records that as two of its `none` rows: "MRTR over stdio, in any
// form", and "downstream.Connect reaching 2026 over stdio". Both are about
// the same missing thing — an actual child process, negotiated at
// 2026-07-28, whose answers travel the same pipe and the same transport.conn
// a real downstream uses.
//
// This file closes them with one case, because they are only separable on
// paper: MRTR is a 2026 feature, so reaching it over stdio requires having
// negotiated 2026 over stdio first.

// connect2026 spawns this test binary as a fakemcp child speaking the
// stateless protocol and connects a downstream.Server to it.
//
// The child is a real process reached through transport.Stdio — not
// fakemcp.Connect. That is forced rather than chosen: transport.Handshake
// refuses to negotiate 2026 over a transport that cannot inject the
// per-request _meta, and the interface saying so has an unexported method,
// so the in-process pipe cannot implement it at all.
func connect2026(t *testing.T, script *fakemcp.Script, deps downstream.Deps) *downstream.Server {
	t.Helper()
	script.SupportedVersions = []string{mcp.Version2026}
	data, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	if deps.ConnectTimeout == 0 {
		deps.ConnectTimeout = 30 * time.Second
	}
	s, err := downstream.Connect(context.Background(), downstream.Spec{
		ID:      "sub2026",
		Kind:    transport.Stdio,
		Command: exe,
		Env:     map[string]string{fakemcp.ScriptEnv: string(data)},
	}, deps)
	if err != nil {
		t.Fatalf("Connect (2026 subprocess): %v", err)
	}
	t.Cleanup(s.Close)
	// Asserted here rather than left to be inferred. A successful Connect
	// already implies it — the fake refuses every post-handshake request
	// without a conformant _meta, so the tools/list inside Connect would
	// have failed — but "the connection came up" and "it came up on the
	// protocol this file is about" are different claims, and only one of
	// them is what a reader of a failure needs to see.
	if got := s.InitializeResult().ProtocolVersion; got != mcp.Version2026 {
		t.Fatalf("negotiated %q over stdio, want %q", got, mcp.Version2026)
	}
	return s
}

// TestSubprocess2026CallResolvesAnInputRound is the stdio counterpart of
// TestCallResolvesOneInputRound, with nothing stubbed between the loop and
// the wire.
//
// The fake is strict in both directions, which is what makes a passing run
// mean something. It refuses any post-handshake request without a
// conformant per-request _meta, so both the original call and the retry
// prove the client injected one — including the retry, which is issued from
// inside the loop rather than by the caller and is exactly where a
// hand-built params object would forget. And it refuses a retry whose
// requestState is not echoed back verbatim, so the opaque blob is checked by
// the peer that minted it rather than by the test that wrote it.
func TestSubprocess2026CallResolvesAnInputRound(t *testing.T) {
	t.Parallel()
	const rootURI = "file:///mrtr-over-stdio"

	script := fakemcp.Minimal("echo").With(fakemcp.Rule{
		Method:  mcp.MethodToolsCall,
		Call:    1, // the first call only; the retry falls through to normal handling
		Actions: []fakemcp.Action{{Kind: fakemcp.ActInputRequired}},
	})
	s := connect2026(t, script, downstream.Deps{})
	s.OnPeerRequest(rootsPeer(t, rootURI))

	res, err := s.Call(testCtx(t), "echo", json.RawMessage(`{"marker":"mrtr-stdio"}`))
	if err != nil {
		t.Fatalf("Call through one MRTR round over stdio: %v", err)
	}

	// The caller sees a COMPLETE result: the loop lives below this seam, so
	// an input_required must never reach it.
	if res.ResultType == mcp.ResultTypeInputRequired {
		t.Fatalf("an input_required result reached the caller: %+v", res)
	}
	// The original arguments survived the retry unchanged.
	if !strings.Contains(string(res.Content), "mrtr-stdio") {
		t.Errorf("retry lost the original arguments: %s", res.Content)
	}
	// And the collected answer made the round trip. The fake echoes the
	// inputResponses it received, so this is the downstream's own account of
	// what arrived rather than the test restating what it sent.
	var structured struct {
		InputResponses map[string]json.RawMessage `json:"inputResponses"`
	}
	if len(res.StructuredContent) == 0 {
		t.Fatalf("the retry carried no inputResponses: %s", res.Content)
	}
	if err := json.Unmarshal(res.StructuredContent, &structured); err != nil {
		t.Fatal(err)
	}
	answer, ok := structured.InputResponses["roots"]
	if !ok {
		t.Fatalf("no answer under the key the server asked with: %v", structured.InputResponses)
	}
	if !strings.Contains(string(answer), rootURI) {
		t.Errorf("the roots/list answer is not the peer handler's: %s", answer)
	}
}

// TestSubprocess2026PlainCallNeedsNoRound is the control, and it is the
// half that keeps the case above honest.
//
// Everything the loop does is invisible from the outside: a complete result
// looks the same whether it arrived first time or after a round trip. So a
// run where the fake never asks for input must also pass, or the case above
// would be equally green against a client that ignored input_required and a
// fake that never sent one.
func TestSubprocess2026PlainCallNeedsNoRound(t *testing.T) {
	t.Parallel()
	s := connect2026(t, fakemcp.Minimal("echo"), downstream.Deps{})

	res, err := s.Call(testCtx(t), "echo", json.RawMessage(`{"marker":"plain"}`))
	if err != nil {
		t.Fatalf("Call over a 2026 stdio downstream: %v", err)
	}
	if !strings.Contains(string(res.Content), "plain") {
		t.Errorf("echo = %s", res.Content)
	}
	if len(res.StructuredContent) != 0 {
		t.Errorf("a call with no input round carried inputResponses: %s", res.StructuredContent)
	}
}
