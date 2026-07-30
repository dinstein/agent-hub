package ctlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/discovery/toolsig"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/registry"
)

// POST /v1/servers/{id}/test — the live connection self-test (docs/modules/controlplane.md).
//
// This is the endpoint that answers "is this credential correct" WITHOUT
// printing it: it makes a real connection and, optionally, a real call
// (docs/modules/controlplane.md rule 5). Everything about a downstream server is verified
// by using it, never by rendering what we stored.
//
// The connection is short-lived and closed before the result is rendered.
// It is dialed through internal/downstream — the same translation
// (SpecFromEntry) and the same connector every other caller uses, so a
// transport field that a new release starts honoring cannot pass the test
// here and be dropped in production.

// maxTestTextBytes bounds the rendered call output. A tool that answers with
// a megabyte of JSON must not become a megabyte of control-plane response.
const maxTestTextBytes = 2 << 10

// maxTestTimeout caps the caller-supplied connect timeout. Cold npx/uvx
// caches are genuinely slow, so the ceiling is generous; the point is that a
// hostile or mistaken timeout_ms cannot pin a daemon goroutine for a day.
const maxTestTimeout = 3 * time.Minute

// ServerTestRequest is the body of POST /v1/servers/{id}/test.
type ServerTestRequest struct {
	// Tool, when set, is also CALLED after the handshake (the original
	// downstream name, not the exposed one).
	Tool string `json:"tool,omitempty"`
	// Args is the JSON arguments object for Tool.
	Args json.RawMessage `json:"args,omitempty"`
	// TimeoutMillis bounds the connection (0 = the downstream default).
	TimeoutMillis int64 `json:"timeout_ms,omitempty"`
	// Definitions asks for ToolDefs alongside the bare name list. Opt-in
	// because schemas are unbounded: the "does this connect" question is the
	// one this endpoint is asked most often, and it does not need them.
	Definitions bool `json:"defs,omitempty"`
}

// ServerTestWire is the self-test report.
type ServerTestWire struct {
	Server    string `json:"server"`
	Transport string `json:"transport"`
	// ServerInfo and ProtocolVersion come from the initialize handshake:
	// proof the other side really answered, not just that a socket opened.
	ServerInfo      string   `json:"server_info,omitempty"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	ConnectMillis   int64    `json:"connect_ms"`
	ToolCount       int      `json:"tool_count"`
	Tools           []string `json:"tools"`
	// ToolDefs is present only when the request asked for definitions.
	// Tools keeps holding the bare names either way.
	ToolDefs []ServerTestToolWire `json:"tool_defs,omitempty"`
	// Call is present only when the request named a tool.
	Call *ServerTestCallWire `json:"call,omitempty"`
}

// ServerTestToolWire is one tool of the live handshake.
//
// Signature is rendered with internal/discovery/toolsig — the SAME compact
// grammar an agent is shown — rather than a second format invented here: an
// operator debugging "why did the agent call this wrong" has to be looking at
// the string the agent saw.
type ServerTestToolWire struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Signature   string `json:"signature,omitempty"`
	// Lossy reports that the signature dropped information, and is why a
	// lossy signature is never presented as the whole truth.
	Lossy bool `json:"lossy,omitempty"`
	// InputSchema is the downstream's own bytes, verbatim: this endpoint
	// never re-encodes a schema, and an EMPTY schema is a fact about the
	// server that substituting "{}" would hide.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ServerTestCallWire is the outcome of the optional tool invocation.
type ServerTestCallWire struct {
	Tool string `json:"tool"`
	// IsError is a TOOL-level failure (a successful call whose tool reported
	// an error), which is a valid answer — not a transport failure.
	IsError bool `json:"is_error"`
	// Text is the concatenated text content, truncated. It is tool output,
	// never a credential: agenthub sends secrets, it does not render them.
	Text   string `json:"text,omitempty"`
	Millis int64  `json:"millis"`
}

// handleServerTest implements POST /v1/servers/{id}/test.
func (s *Server) handleServerTest(w http.ResponseWriter, r *http.Request, id string) {
	reqID := requestIDFrom(r.Context())
	var req ServerTestRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"decoding test request: "+err.Error(), "", reqID)
		return
	}
	if len(req.Args) > 0 && req.Tool == "" {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"args without tool", "name the tool the arguments belong to", reqID)
		return
	}
	if len(req.Args) > 0 && !json.Valid(req.Args) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"args is not valid JSON", "", reqID)
		return
	}

	doc, ok := s.opts.Registry.Snapshot().Servers.V.Servers[id]
	if !ok {
		// Uniform 404: an unknown server reads exactly like an unknown route.
		writeNotFound(w, r)
		return
	}
	entry := doc.V
	// A docker-runtime entry needs no special case: SpecFromEntry carries the
	// container config into Spec.Docker, and the dial spawns the container
	// rather than the host command, so the isolation the operator configured
	// is delivered by the probe path too.
	spec, err := downstream.SpecFromEntry(id, entry)
	if err != nil {
		// A definition this build cannot speak is the operator's to fix, so
		// it is a 400 and not a 500.
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(),
			"fix the server entry's transport fields", reqID)
		return
	}

	deps := downstream.Deps{Log: s.log}
	if s.opts.NonRegistry.TestDeps != nil {
		deps = s.opts.NonRegistry.TestDeps(id, spec)
	}
	if d := testTimeout(req.TimeoutMillis); d > 0 {
		deps.ConnectTimeout = d
	}

	connect := s.opts.NonRegistry.Connect
	if connect == nil {
		connect = defaultConnector
	}

	start := time.Now()
	conn, err := connect(r.Context(), spec, deps)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			fmt.Sprintf("server %q did not connect: %v", id, err),
			connectHint(err), reqID)
		return
	}
	defer conn.Close()
	out := ServerTestWire{
		Server:        id,
		Transport:     entry.TransportName(),
		ConnectMillis: time.Since(start).Milliseconds(),
		Tools:         []string{},
	}
	if ir := conn.InitializeResult(); ir != nil {
		out.ServerInfo = strings.TrimSpace(ir.ServerInfo.Name + " " + ir.ServerInfo.Version)
		out.ProtocolVersion = ir.ProtocolVersion
	}
	defs := conn.Tools()
	for _, t := range defs {
		out.Tools = append(out.Tools, t.Name)
	}
	out.ToolCount = len(out.Tools)
	if req.Definitions {
		// Everything below is ALREADY in hand from the handshake: nothing
		// here reconnects, and nothing consults the gateway's persisted tool
		// cache (which a `server test`-only workflow never writes).
		out.ToolDefs = make([]ServerTestToolWire, 0, len(defs))
		for _, t := range defs {
			sig := toolsig.Named(t.Name, t, toolsig.Options{})
			out.ToolDefs = append(out.ToolDefs, ServerTestToolWire{
				Name:        t.Name,
				Description: t.Description,
				Signature:   sig.Text,
				Lossy:       sig.Lossy,
				InputSchema: t.InputSchema,
			})
		}
	}

	if req.Tool != "" {
		callStart := time.Now()
		res, cerr := conn.Call(r.Context(), req.Tool, req.Args)
		if cerr != nil {
			writeErr(w, http.StatusInternalServerError, CodeInternal,
				fmt.Sprintf("server %q: calling %q failed: %v", id, req.Tool, cerr), "", reqID)
			return
		}
		out.Call = &ServerTestCallWire{
			Tool:    req.Tool,
			IsError: res.IsError,
			Text:    truncateText(contentText(res.Content), maxTestTextBytes),
			Millis:  time.Since(callStart).Milliseconds(),
		}
	}
	writeOK(w, http.StatusOK, out)
}

// defaultConnector is the production dialer: the real internal/downstream
// connection, adapted to the narrow TestConn face.
func defaultConnector(ctx context.Context, spec downstream.Spec, deps downstream.Deps) (TestConn, error) {
	srv, err := downstream.Connect(ctx, spec, deps)
	if err != nil {
		// Return an untyped nil: a typed nil *downstream.Server inside a
		// non-nil interface would defeat every `conn == nil` check.
		return nil, err
	}
	return srv, nil
}

// testTimeout clamps a caller-supplied timeout into the allowed band.
// A negative or zero value means "use the connection layer's default".
func testTimeout(ms int64) time.Duration {
	if ms <= 0 {
		return 0
	}
	d := time.Duration(ms) * time.Millisecond
	if d > maxTestTimeout {
		return maxTestTimeout
	}
	return d
}

// connectHint suggests the next action for a failed self-test. An
// authorization refusal is the one case where the fix is a login rather than
// a configuration change, so it is worth naming.
func connectHint(err error) string {
	// By status, not by substring — see transport.IsAuthStatus. The message
	// carries the response-body snippet, so a 502 whose body mentions an
	// upstream 401 used to be reported as a credential problem.
	if transport.IsAuthStatus(err) {
		return "the server rejected the credentials; run an OAuth login or re-set its secret"
	}
	return ""
}

// contentText flattens the text items of a call result. Non-text items are
// NAMED by type rather than dumped, so a binary result stays visible without
// becoming a wall of base64.
func contentText(raw json.RawMessage) string {
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, it := range items {
		if it.Type == "text" {
			b.WriteString(it.Text)
			b.WriteByte('\n')
			continue
		}
		fmt.Fprintf(&b, "<%s content>\n", it.Type)
	}
	return b.String()
}

func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "… (truncated)"
}

// compile-time proof that the production connector matches the injected one.
var _ Connector = defaultConnector

// compile-time proof that the registry entry face this file relies on exists
// on the concrete type (a moved method would otherwise only fail at the call
// site inside a rarely exercised branch).
var _ = registry.ServerEntry{}.TransportName
