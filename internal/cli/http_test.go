package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// --- server add: http/sse flags -----------------------------------------

func TestServerAddHTTPFlags(t *testing.T) {
	setDataDir(t)
	code, out, stderr := runCLI(t, "", "server", "add", "notion",
		"--url", "https://mcp.notion.com/mcp",
		"--header", "X-Api-Version=2025-01-01",
		"--header", "X-Key=${SECRET_NOTION}",
		"--json")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s\n%s", code, out, stderr)
	}

	code, out, _ = runCLI(t, "", "server", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("ls exit = %d", code)
	}
	var rows []ServerRow
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &rows); err != nil {
		t.Fatalf("decode rows: %v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.Transport != "http" || r.URL != "https://mcp.notion.com/mcp" {
		t.Fatalf("row = %+v", r)
	}
	// Placeholders are stored verbatim: the registry never holds a value.
	if r.Headers["X-Key"] != "${SECRET_NOTION}" {
		t.Fatalf("headers = %+v", r.Headers)
	}
	if r.Command != "" {
		t.Errorf("http entry carries a command: %+v", r)
	}
}

func TestServerAddSSEAndLocal(t *testing.T) {
	setDataDir(t)
	if code, out, _ := runCLI(t, "", "server", "add", "legacy",
		"--transport", "sse", "--url", "https://legacy.example/sse", "--json"); code != ExitOK {
		t.Fatalf("sse add exit = %d\n%s", code, out)
	}
	if code, out, _ := runCLI(t, "", "server", "add", "dev",
		"--url", "http://127.0.0.1:8931/mcp", "--local", "--json"); code != ExitOK {
		t.Fatalf("local add exit = %d\n%s", code, out)
	}
	code, out, _ := runCLI(t, "", "server", "ls", "--json")
	if code != ExitOK {
		t.Fatal(out)
	}
	var rows []ServerRow
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &rows); err != nil {
		t.Fatal(err)
	}
	byID := map[string]ServerRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if byID["legacy"].Transport != "sse" {
		t.Errorf("legacy = %+v", byID["legacy"])
	}
	if byID["dev"].Provenance != "local" {
		t.Errorf("dev = %+v (provenance must be recorded, it drives SSRF screening)", byID["dev"])
	}
}

// TestServerAddHTTPUsageErrors pins the fail-closed and
// no-silently-ignored-flag rules of the http/sse form.
func TestServerAddHTTPUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"cmd and url", []string{"server", "add", "x", "--cmd", "foo", "--url", "https://x/mcp"}},
		{"loopback without --local", []string{"server", "add", "x", "--url", "http://127.0.0.1:9/mcp"}},
		{"rfc1918 always refused", []string{"server", "add", "x", "--url", "http://10.0.0.1/mcp", "--local"}},
		{"local with public host", []string{"server", "add", "x", "--url", "https://example.com/mcp", "--local"}},
		{"http transport without url", []string{"server", "add", "x", "--transport", "http", "--cmd", "foo"}},
		{"stdio flags on http", []string{"server", "add", "x", "--url", "https://x/mcp", "--env", "A=1"}},
		{"bad header", []string{"server", "add", "x", "--url", "https://x/mcp", "--header", "NOEQUALS"}},
		{"unknown transport", []string{"server", "add", "x", "--transport", "grpc", "--url", "https://x/mcp"}},
		{"bad scheme", []string{"server", "add", "x", "--url", "ftp://x/mcp"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDataDir(t)
			code, out, stderr := runCLI(t, "", tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d\n%s%s", code, ExitUsage, out, stderr)
			}
		})
	}
}

// TestServerAddStdinHTTPEntry covers the highest-frequency action for
// remote servers: pasting a published snippet that carries only a url.
func TestServerAddStdinHTTPEntry(t *testing.T) {
	setDataDir(t)
	code, out, _ := runCLI(t, `{"mcpServers":{"linear":{"url":"https://mcp.linear.app/mcp"}}}`,
		"server", "add", "--stdin", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	var res AddedServers
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 || res.Added[0].Transport != "http" {
		t.Fatalf("added = %+v, want one http entry", res.Added)
	}
}

// --- server test ---------------------------------------------------------

// mcpHTTPTestServer is a minimal streamable-http MCP endpoint answering
// application/json, exposing one trivial "echo" tool.
func mcpHTTPTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return mcpHTTPTestServerWithTools(t, []mcp.ToolDef{
		{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})
}

// mcpHTTPTestServerWithTools is the same endpoint with a caller-chosen
// catalog, so a test about how definitions are RENDERED can ship a schema
// worth rendering without changing what the other tests connect to.
func mcpHTTPTestServerWithTools(t *testing.T, tools []mcp.ToolDef) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		msg, err := mcp.ParseMessage(raw)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		req, ok := msg.(*mcp.Request)
		if !ok {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result json.RawMessage
		switch req.Method {
		case mcp.MethodInitialize:
			result, _ = json.Marshal(mcp.InitializeResult{
				ProtocolVersion: mcp.ProtocolVersion,
				Capabilities:    json.RawMessage(`{"tools":{}}`),
				ServerInfo:      mcp.Implementation{Name: "cli-http-fake", Version: "1"},
			})
		case mcp.MethodToolsList:
			result, _ = json.Marshal(mcp.ListToolsResult{Tools: tools})
		case mcp.MethodToolsCall:
			var p mcp.CallToolParams
			_ = json.Unmarshal(req.Params, &p)
			result, _ = json.Marshal(mcp.CallResult{
				Content: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":%q}]`, string(p.Arguments))),
			})
		default:
			w.Header().Set("Content-Type", "application/json")
			data, _ := json.Marshal(mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeMethodNotFound, Message: req.Method}))
			_, _ = w.Write(data)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(mcp.NewResponse(req.ID, result))
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestServerTestHTTPConnectsAndCalls is the CLI half of the http path:
// add --url, connect, list, and make a REAL call (which is how a credential
// is verified — never by printing it, docs/modules/controlplane.md rule 5).
func TestServerTestHTTPConnectsAndCalls(t *testing.T) {
	setDataDir(t)
	srv := mcpHTTPTestServer(t)

	if code, out, _ := runCLI(t, "", "server", "add", "http-fake",
		"--url", srv.URL+"/mcp", "--transport", "http", "--local", "--json"); code != ExitOK {
		t.Fatalf("add exit = %d\n%s", code, out)
	}

	code, out, stderr := runCLI(t, "", "server", "test", "http-fake",
		"--tool", "echo", "--args", `{"marker":"cli-http"}`, "--json")
	if code != ExitOK {
		t.Fatalf("test exit = %d\n%s\n%s", code, out, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// Progress lines precede the envelope; the envelope is always last.
	var res ServerTestResult
	if err := json.Unmarshal(decodeEnvelope(t, lines[len(lines)-1]).Data, &res); err != nil {
		t.Fatalf("decode result: %v\n%s", err, out)
	}
	if res.ToolCount != 1 || res.Tools[0] != "echo" {
		t.Fatalf("result = %+v", res)
	}
	if res.Call == nil || !strings.Contains(res.Call.Text, "cli-http") {
		t.Fatalf("call = %+v", res.Call)
	}
	if !strings.Contains(res.ServerInfo, "cli-http-fake") {
		t.Errorf("serverInfo = %q", res.ServerInfo)
	}
	// The NDJSON progress stream must be parseable line by line.
	for _, l := range lines[:len(lines)-1] {
		var ev map[string]any
		if err := json.Unmarshal([]byte(l), &ev); err != nil || ev["event"] == nil {
			t.Errorf("progress line %q is not an event: %v", l, err)
		}
	}
}

func TestServerTestUnknownServer(t *testing.T) {
	setDataDir(t)
	code, out, _ := runCLI(t, "", "server", "test", "nope", "--json")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitNotFound, out)
	}
	if env := decodeEnvelope(t, out); env.Error == nil || env.Error.Code != CodeServerNotFound {
		t.Errorf("envelope = %s", out)
	}
}

// --- auth ---------------------------------------------------------------

func TestAuthStatusEmptyAndUnknown(t *testing.T) {
	setDataDir(t)
	code, out, _ := runCLI(t, "", "auth", "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	var rows AuthStatusList
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}

	code, out, _ = runCLI(t, "", "auth", "status", "ghost", "--json")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitNotFound, out)
	}
}

// TestAuthLogoutIsIdempotent: clearing credentials that do not exist must
// succeed, so cleanup scripts need not branch on state they cannot see.
func TestAuthLogoutIsIdempotent(t *testing.T) {
	setDataDir(t)
	for i := range 2 {
		code, out, stderr := runCLI(t, "", "auth", "logout", "gone", "--json")
		if code != ExitOK {
			t.Fatalf("run %d exit = %d\n%s%s", i, code, out, stderr)
		}
	}
}

// TestAuthLoginNeedsATarget: a stdio server has nothing to authorize
// against, and the error must say so rather than starting a discovery that
// cannot succeed.
func TestAuthLoginNeedsATarget(t *testing.T) {
	setDataDir(t)
	if code, out, _ := runCLI(t, "", "server", "add", "local", "--cmd", "foo"); code != ExitOK {
		t.Fatalf("add exit = %d\n%s", code, out)
	}
	code, out, _ := runCLI(t, "", "auth", "login", "local", "--json")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitUsage, out)
	}
}

func TestAuthLoginModeFlagsAreExclusive(t *testing.T) {
	setDataDir(t)
	code, _, _ := runCLI(t, "", "auth", "login", "x", "--manual", "--device")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
}
