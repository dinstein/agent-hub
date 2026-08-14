package mcpstub_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/test/mcpstub"
)

// The stub is this project's conformance evidence for streamable HTTP
// (docs/status/mcp-2026-07-28.md §4.1, §7.7); internal/testutil/fakemcp is the stdio
// counterpart, and the header rules below have no counterpart there because
// stdio carries none. So a rule this stub does not enforce is, on its
// transport, a rule nothing enforces. These
// tests drive it with a HAND-BUILT non-conformant client, because a check the
// real client happens to satisfy proves nothing about the check itself — all
// three below pass against the real client today and exist to catch the
// refactor that stops them passing.
const meta2026 = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
	`"io.modelcontextprotocol/clientCapabilities":{}}`

func stubHeaders(method string, extra ...string) map[string]string {
	h := map[string]string{"MCP-Protocol-Version": mcp.Version2026, "Mcp-Method": method}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return h
}

// clientCapabilities is required on every request, and an empty object is
// how a client says "none" — omitting the key is not. The stub named this
// key in a rejection message before it could actually detect the difference,
// because mcp.RequestMeta decodes an absent key identically to {}.
func TestStubRefusesMissingClientCapabilities(t *testing.T) {
	stub := mcpstub.New()
	defer stub.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":` +
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	e := postFull(t, stub.URL(), body, stubHeaders("tools/list")).Error
	if e == nil || e.Code != mcp.CodeInvalidParams ||
		!strings.Contains(e.Message, "clientCapabilities") {
		t.Fatalf("error = %+v, want the missing capability refused", e)
	}
	// The same request WITH the key is accepted, so the check is about the
	// key and not about something else in the frame.
	ok := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + meta2026 + `}}`
	if e := postFull(t, stub.URL(), ok, stubHeaders("tools/list")).Error; e != nil {
		t.Fatalf("well-formed request refused: %+v", e)
	}
}

// The MRTR retry is an independent request and MUST carry a different
// JSON-RPC id. This stub is the only place in the tree that sees both ids on
// the wire, so it is the only place the rule can be observed.
func TestStubRefusesRetryReusingTheID(t *testing.T) {
	stub := mcpstub.New()
	defer stub.Close()

	call := func(id int, extra string) *mcp.Response {
		body := `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{` +
			meta2026 + `,"name":"confirm","arguments":{"a":1}` + extra + `}}`
		return postFull(t, stub.URL(), body, stubHeaders("tools/call", "Mcp-Name", "confirm"))
	}

	first := call(7, "")
	var ir mcp.InputRequiredResult
	if err := json.Unmarshal(first.Result, &ir); err != nil {
		t.Fatalf("decode input_required: %v", err)
	}
	if ir.RequestState == nil {
		t.Fatal("the stub issued no requestState")
	}
	state := `,"requestState":` + quote(*ir.RequestState) +
		`,"inputResponses":{"roots":{"roots":[]}}`

	// Same id as the original: refused.
	if r := call(7, state); r.Error == nil || !strings.Contains(r.Error.Message, "reused JSON-RPC id") {
		t.Fatalf("retry with the original id: error = %+v, want a refusal", r.Error)
	}
}

// The retry re-issues the ORIGINAL request. Arguments that changed between
// rounds mean the client rebuilt the call rather than resuming the one the
// server computed its state against.
func TestStubRefusesRetryWithChangedArguments(t *testing.T) {
	stub := mcpstub.New()
	defer stub.Close()

	body := `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{` + meta2026 +
		`,"name":"confirm","arguments":{"a":1}}}`
	first := postFull(t, stub.URL(), body, stubHeaders("tools/call", "Mcp-Name", "confirm"))
	var ir mcp.InputRequiredResult
	if err := json.Unmarshal(first.Result, &ir); err != nil {
		t.Fatalf("decode input_required: %v", err)
	}

	retry := `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{` + meta2026 +
		`,"name":"confirm","arguments":{"a":2},"requestState":` + quote(*ir.RequestState) +
		`,"inputResponses":{"roots":{"roots":[]}}}}`
	r := postFull(t, stub.URL(), retry, stubHeaders("tools/call", "Mcp-Name", "confirm"))
	if r.Error == nil || !strings.Contains(r.Error.Message, "differ from the original") {
		t.Fatalf("retry with changed arguments: error = %+v, want a refusal", r.Error)
	}
}

func postFull(t *testing.T, url, body string, headers map[string]string) *mcp.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	var out mcp.Response
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", buf.String(), err)
	}
	return &out
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// clientInfo is OPTIONAL — the specification says clients SHOULD send it
// "unless specifically configured not to do so", which contemplates a
// conformant client that omits it. A stub refusing the absence would grade
// that client as broken, so absence passes and only a malformed present one
// is refused. Strictness that outruns the spec is not strictness.
func TestStubAcceptsAbsentButNotMalformedClientInfo(t *testing.T) {
	stub := mcpstub.New()
	defer stub.Close()

	base := `"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientCapabilities":{}`
	frame := func(id int, extra string) string {
		return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/list","params":{"_meta":{` +
			base + extra + `}}}`
	}

	// Absent: accepted.
	if e := postFull(t, stub.URL(), frame(1, ""), stubHeaders("tools/list")).Error; e != nil {
		t.Fatalf("absent clientInfo refused: %+v", e)
	}
	// Present and complete: accepted.
	ok := `,"io.modelcontextprotocol/clientInfo":{"name":"agenthub","version":"test"}`
	if e := postFull(t, stub.URL(), frame(2, ok), stubHeaders("tools/list")).Error; e != nil {
		t.Fatalf("well-formed clientInfo refused: %+v", e)
	}
	// Present without version: refused. Implementation requires both.
	bad := `,"io.modelcontextprotocol/clientInfo":{"name":"agenthub"}`
	e := postFull(t, stub.URL(), frame(3, bad), stubHeaders("tools/list")).Error
	if e == nil || !strings.Contains(e.Message, "clientInfo") {
		t.Fatalf("error = %+v, want the incomplete clientInfo refused", e)
	}
}
