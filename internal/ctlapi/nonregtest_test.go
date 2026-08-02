package ctlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/registry"
)

// nrConn is an in-process TestConn: the self-test suite must never spawn a
// child process.
type nrConn struct {
	init    *mcp.InitializeResult
	tools   []mcp.ToolDef
	result  *mcp.CallResult
	callErr error
	gotTool string
	gotArgs json.RawMessage
	closed  bool
}

func (c *nrConn) InitializeResult() *mcp.InitializeResult { return c.init }
func (c *nrConn) Tools() []mcp.ToolDef                    { return c.tools }
func (c *nrConn) Close()                                  { c.closed = true }

func (c *nrConn) Call(_ context.Context, tool string, args json.RawMessage) (*mcp.CallResult, error) {
	c.gotTool, c.gotArgs = tool, args
	return c.result, c.callErr
}

// nrConnector records what it was asked to dial.
type nrConnector struct {
	conn    *nrConn
	err     error
	gotSpec downstream.Spec
	gotDeps downstream.Deps
	dialed  int
}

func (c *nrConnector) connect(_ context.Context, spec downstream.Spec, deps downstream.Deps) (TestConn, error) {
	c.dialed++
	c.gotSpec, c.gotDeps = spec, deps
	if c.err != nil {
		return nil, c.err
	}
	return c.conn, nil
}

func nrTestConn() *nrConn {
	return &nrConn{
		init: &mcp.InitializeResult{
			ProtocolVersion: "2025-06-18",
			ServerInfo:      mcp.Implementation{Name: "fake", Version: "1.2.3"},
		},
		tools: []mcp.ToolDef{{Name: "search"}, {Name: "create_issue"}},
		result: &mcp.CallResult{
			Content: json.RawMessage(`[{"type":"text","text":"ok"},{"type":"image","data":"…"}]`),
		},
	}
}

func TestServerTestConnects(t *testing.T) {
	conn := nrTestConn()
	c := &nrConnector{conn: conn}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	seedServer(t, env.reg, "github", true)

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test", ServerTestRequest{})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out ServerTestWire
	nrData(t, body, &out)
	if out.Server != "github" || out.Transport != "stdio" {
		t.Errorf("out = %+v", out)
	}
	if out.ServerInfo != "fake 1.2.3" || out.ProtocolVersion != "2025-06-18" {
		t.Errorf("handshake = %+v", out)
	}
	if out.ToolCount != 2 || len(out.Tools) != 2 || out.Tools[0] != "search" {
		t.Errorf("tools = %+v", out.Tools)
	}
	if out.Call != nil {
		t.Errorf("no tool was requested, yet a call was reported: %+v", out.Call)
	}
	if !conn.closed {
		t.Errorf("the self-test connection was not closed")
	}
	if c.gotSpec.ID != "github" || c.gotSpec.Command != "fake" {
		t.Errorf("spec = %+v", c.gotSpec)
	}
}

func TestServerTestCallsTool(t *testing.T) {
	conn := nrTestConn()
	c := &nrConnector{conn: conn}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	seedServer(t, env.reg, "github", true)

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test",
		ServerTestRequest{Tool: "search", Args: json.RawMessage(`{"q":"x"}`)})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out ServerTestWire
	nrData(t, body, &out)
	if out.Call == nil || out.Call.Tool != "search" || out.Call.IsError {
		t.Fatalf("call = %+v", out.Call)
	}
	// Text items are flattened; a non-text item is NAMED, not dumped.
	if !strings.Contains(out.Call.Text, "ok") || !strings.Contains(out.Call.Text, "<image content>") {
		t.Errorf("text = %q", out.Call.Text)
	}
	if conn.gotTool != "search" || string(conn.gotArgs) != `{"q":"x"}` {
		t.Errorf("call args = %q %q", conn.gotTool, conn.gotArgs)
	}
}

// The definitions are OPT-IN, and when asked for they carry the downstream's
// own schema bytes. Both halves matter: a self-test that always shipped every
// schema would answer the common "does this connect" question with a payload
// proportional to the catalog, and a schema this endpoint re-encoded would no
// longer be the thing the operator is trying to inspect.
func TestServerTestDefinitionsAreOptIn(t *testing.T) {
	conn := nrTestConn()
	conn.tools = []mcp.ToolDef{{
		Name:        "search",
		Description: "find things",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
	}}
	c := &nrConnector{conn: conn}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	seedServer(t, env.reg, "github", true)

	_, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test", ServerTestRequest{})
	var bare ServerTestWire
	nrData(t, body, &bare)
	if len(bare.ToolDefs) != 0 {
		t.Fatalf("definitions were shipped without being asked for: %+v", bare.ToolDefs)
	}
	if len(bare.Tools) != 1 || bare.Tools[0] != "search" {
		t.Fatalf("the bare name list must be unaffected: %+v", bare.Tools)
	}

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test",
		ServerTestRequest{Definitions: true})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out ServerTestWire
	nrData(t, body, &out)
	if len(out.ToolDefs) != 1 {
		t.Fatalf("tool_defs = %+v", out.ToolDefs)
	}
	def := out.ToolDefs[0]
	if def.Name != "search" || def.Description != "find things" {
		t.Errorf("def = %+v", def)
	}
	// The compact signature is the grammar the AGENT sees, not a second one.
	if !strings.Contains(def.Signature, "search(") || !strings.Contains(def.Signature, "q:str") {
		t.Errorf("signature = %q", def.Signature)
	}
	// Verbatim: the bytes the downstream sent, not a re-encoding.
	if string(def.InputSchema) != string(conn.tools[0].InputSchema) {
		t.Errorf("input_schema = %s", def.InputSchema)
	}
}

// TestServerTestToolErrorIsAnAnswer: a tool-level failure is a valid ANSWER
// (200 with is_error), a transport failure is not.
func TestServerTestToolErrorIsAnAnswer(t *testing.T) {
	conn := nrTestConn()
	conn.result = &mcp.CallResult{IsError: true, Content: json.RawMessage(`[{"type":"text","text":"no such repo"}]`)}
	c := &nrConnector{conn: conn}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	seedServer(t, env.reg, "github", true)

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test",
		ServerTestRequest{Tool: "search"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var out ServerTestWire
	nrData(t, body, &out)
	if out.Call == nil || !out.Call.IsError {
		t.Fatalf("call = %+v", out.Call)
	}
}

func TestServerTestCallTransportFailure(t *testing.T) {
	conn := nrTestConn()
	conn.callErr = errors.New("stream closed")
	c := &nrConnector{conn: conn}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	seedServer(t, env.reg, "github", true)

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test",
		ServerTestRequest{Tool: "search"})
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if !conn.closed {
		t.Errorf("the connection leaked on the failure path")
	}
}

func TestServerTestConnectFailure(t *testing.T) {
	// A *transport.Error carrying the status, which is what a real connector
	// returns: verified by dialing a 401 through downstream.Connect, where the
	// typed error survives every wrapping layer.
	//
	// The fake used to be a bare errors.New("http 401 unauthorized"). That
	// passed only because connectHint searched the message for a substring —
	// an unrealistic fake propped up by an implementation with the same flaw.
	c := &nrConnector{err: fmt.Errorf(`server "github": initialize: %w`, &transport.Error{
		Class: transport.ClassFatal, StatusCode: http.StatusUnauthorized,
		Err: errors.New(`POST https://api.github.test/mcp: http 401: {"error":"unauthorized"}`),
	})}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	seedServer(t, env.reg, "github", true)

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test", ServerTestRequest{})
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if code := nrErrCode(t, body); code != CodeAuthRequired {
		t.Errorf("code = %q, want %q: %s", code, CodeAuthRequired, body)
	}
	// The auth hint is the one case where the fix is a login, so it is named.
	if !nrContains(body, "login") {
		t.Errorf("a 401 must hint at re-authorizing: %s", body)
	}
}

func TestServerTestMissingSecretIsActionable(t *testing.T) {
	c := &nrConnector{err: fmt.Errorf("prepare environment: %w", &downstream.UnresolvedSecretError{
		ServerID: "brave-search",
		Key:      "BRAVE_API_KEY",
	})}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	seedServer(t, env.reg, "brave-search", true)

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/brave-search/test", ServerTestRequest{})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", status, body)
	}
	var envelope struct {
		Error struct {
			Code           string   `json:"code"`
			Message        string   `json:"message"`
			MissingSecrets []string `json:"missingSecrets"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Error.Code != CodeSecretRequired || len(envelope.Error.MissingSecrets) != 1 ||
		envelope.Error.MissingSecrets[0] != "BRAVE_API_KEY" {
		t.Fatalf("error = %+v", envelope.Error)
	}
	if strings.Contains(envelope.Error.Message, "BRAVE_API_KEY") {
		t.Fatalf("key belongs in structured metadata, not display copy: %s", body)
	}
}

// TestServerTestConnectFailureIsNotBlamedOnCredentials is the other half: a
// failure that merely MENTIONS 401 must not be reported as a credential
// problem. A proxy answering 502 explains itself in the body, the transport
// folds that body into the error message, and the old substring match then
// told the operator to re-run a login for a hop they cannot even see.
func TestServerTestConnectFailureIsNotBlamedOnCredentials(t *testing.T) {
	c := &nrConnector{err: fmt.Errorf(`server "github": initialize: %w`, &transport.Error{
		Class: transport.ClassUnavailable, StatusCode: http.StatusBadGateway,
		Err: errors.New("POST https://api.github.test/mcp: http 502: upstream returned http 401"),
	})}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	seedServer(t, env.reg, "github", true)

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test", ServerTestRequest{})
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if code := nrErrCode(t, body); code != CodeInternal {
		t.Errorf("code = %q, want %q: %s", code, CodeInternal, body)
	}
	if nrContains(body, "login") {
		t.Errorf("a 502 was blamed on the credentials: %s", body)
	}
}

func TestServerTestUnknownServerIs404(t *testing.T) {
	c := &nrConnector{conn: nrTestConn()}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/nope/test", ServerTestRequest{})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
	if c.dialed != 0 {
		t.Errorf("an unknown server was dialed anyway")
	}
}

// TestServerTestProbesDockerAsContainer: a container-isolated entry is
// probed like any other, and the isolation reaches the dial. This endpoint
// used to 409 such entries, back when the dial would have spawned the
// command on the host; the assertion that matters now is not "it was
// dialed" but "what was dialed still carried the container config" —
// probing a docker entry AS A HOST PROCESS is the failure to guard against,
// and it would show up here as a nil Spec.Docker.
func TestServerTestProbesDockerAsContainer(t *testing.T) {
	c := &nrConnector{conn: nrTestConn()}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	if err := env.reg.Update(t.Context(), func(tx *registry.Tx) error {
		tx.Servers.V.Servers["boxed"] = registry.Doc[registry.ServerEntry]{V: registry.ServerEntry{
			Transport: "stdio", Command: "fake", Enabled: true, Runtime: registry.RuntimeDocker,
			Docker: &registry.DockerRuntime{Image: "ghcr.io/x/y:1"},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/boxed/test", ServerTestRequest{})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if c.dialed != 1 {
		t.Fatalf("dialed = %d, want 1", c.dialed)
	}
	if c.gotSpec.Docker == nil {
		t.Fatal("the probe dialed a docker entry with a nil Spec.Docker: it would run on the host")
	}
	if got := c.gotSpec.Docker.Image; got != "ghcr.io/x/y:1" {
		t.Errorf("Spec.Docker.Image = %q, want the configured image", got)
	}
}

func TestServerTestBadRequests(t *testing.T) {
	c := &nrConnector{conn: nrTestConn()}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	seedServer(t, env.reg, "github", true)

	// args without tool.
	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test",
		json.RawMessage(`{"args":{"q":"x"}}`))
	if status != http.StatusBadRequest {
		t.Errorf("args-without-tool: status = %d: %s", status, body)
	}
	// malformed body.
	status, body = nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test", `{"tool":`)
	if status != http.StatusBadRequest {
		t.Errorf("malformed: status = %d: %s", status, body)
	}
	if c.dialed != 0 {
		t.Errorf("a rejected request still dialed")
	}
}

// TestServerTestSpecError: a transport this build cannot speak is the
// operator's to fix, so it is a 400 and never a 500.
func TestServerTestSpecError(t *testing.T) {
	c := &nrConnector{conn: nrTestConn()}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Connect = c.connect })
	if err := env.reg.Update(t.Context(), func(tx *registry.Tx) error {
		tx.Servers.V.Servers["broken"] = registry.Doc[registry.ServerEntry]{V: registry.ServerEntry{
			Transport: "carrier-pigeon", Enabled: true,
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/servers/broken/test", ServerTestRequest{})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if c.dialed != 0 {
		t.Errorf("an untranslatable entry was dialed")
	}
}

func TestServerTestTimeoutClamp(t *testing.T) {
	if got := testTimeout(0); got != 0 {
		t.Errorf("0 = %v, want the connection layer's default", got)
	}
	if got := testTimeout(-5); got != 0 {
		t.Errorf("negative = %v, want the default", got)
	}
	if got := testTimeout(1500); got != 1500*time.Millisecond {
		t.Errorf("1500ms = %v", got)
	}
	if got := testTimeout(int64(time.Hour / time.Millisecond)); got != maxTestTimeout {
		t.Errorf("an hour = %v, want the %v ceiling", got, maxTestTimeout)
	}
}

func TestServerTestPassesTimeoutAndDeps(t *testing.T) {
	c := &nrConnector{conn: nrTestConn()}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.Connect = c.connect
		d.TestDeps = func(_ string, _ downstream.Spec) downstream.Deps {
			return downstream.Deps{NotificationStream: true}
		}
	})
	seedServer(t, env.reg, "github", true)

	status, _ := nrDo(t, env.sock, http.MethodPost, "/v1/servers/github/test",
		ServerTestRequest{TimeoutMillis: 2500})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if c.gotDeps.ConnectTimeout != 2500*time.Millisecond {
		t.Errorf("timeout = %v", c.gotDeps.ConnectTimeout)
	}
	if !c.gotDeps.NotificationStream {
		t.Errorf("the injected deps were discarded: %+v", c.gotDeps)
	}
}

func TestTruncateText(t *testing.T) {
	if got := truncateText("abc", 10); got != "abc" {
		t.Errorf("short = %q", got)
	}
	got := truncateText(strings.Repeat("x", 20), 4)
	if !strings.HasPrefix(got, "xxxx") || !strings.Contains(got, "truncated") {
		t.Errorf("long = %q", got)
	}
}
