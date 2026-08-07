package e2e_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The daemon's MCP data plane is the product's SECOND face: an AI agent that
// speaks HTTP rather than spawning a child reaches agenthub here, graded by an
// agent token instead of by which client config it was written into.
//
// internal/httpbridge is covered thoroughly in-process, but every one of those
// tests hands its transport a recording dispatcher, so what has never been
// shown is that the face is CONNECTED: that a real `daemon start --http-addr`
// binds, that a token minted by `agenthub token create` authenticates against
// the store the daemon opened, and that a request arriving over HTTP reaches
// the same pipeline and the same downstream a stdio client would. Those are
// four processes and two files agreeing, which is the shape only this suite
// can put together.
//
// Everything here is loopback and every listener is ephemeral.

// httpPlaneClient is a hand-written MCP Streamable HTTP client, written on
// net/http and encoding/json alone for the same reason gatewayClient avoids
// internal/mcp: the wire format is the thing under test, and a client built
// out of the server's own types cannot disagree with it.
type httpPlaneClient struct {
	t       *testing.T
	url     string
	token   string
	session string
	nextID  int
}

// freeLoopbackAddr reserves a loopback port by binding and immediately
// releasing it. The daemon takes the address on its command line and reports
// the bound address nowhere a caller can read — neither run/daemon.json nor
// `daemon status` carries it — so the port has to be decided before the start
// rather than discovered after it.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a loopback port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

// startHTTPDaemon starts a headless daemon serving the data plane on a fresh
// loopback port and returns the MCP endpoint URL. The daemon is killed on
// cleanup whether or not the test got as far as using it.
func startHTTPDaemon(t *testing.T, dataDir, socket string, env []string, extra ...string) string {
	t.Helper()
	addr := freeLoopbackAddr(t)
	args := append([]string{"daemon", "start", "--headless", "--http-addr", addr}, extra...)
	runAgenthubEnv(t, env, "", args...)
	h := &daemonEnv{dataDir: dataDir, socket: socket, env: env}
	t.Cleanup(func() { h.killDaemon(t) })
	return "http://" + addr + httpMCPPath
}

// httpMCPPath is httpbridge.DefaultPath, written out rather than imported.
// This suite drives the endpoint from the outside, and the path an external
// agent has to be told is exactly the kind of thing that must not change
// quietly — the same reason lazy_test.go spells out the meta-tool list.
const httpMCPPath = "/mcp"

// mintToken runs `token create` and returns the value, which is printed once
// and never again.
func mintToken(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	out, _ := runAgenthubEnv(t, env, "", append([]string{"token", "create", name, "--json"}, args...)...)
	e := lastEnvelope(t, out)
	if !e.OK {
		t.Fatalf("token create %s: %s", name, out)
	}
	var res struct {
		Value string `json:"value"`
		Token struct {
			Tier  string `json:"tier"`
			State string `json:"state"`
		} `json:"token"`
	}
	if err := json.Unmarshal(e.Data, &res); err != nil {
		t.Fatalf("token create data: %v\n%s", err, e.Data)
	}
	if !strings.HasPrefix(res.Value, "agt_") {
		t.Fatalf("token value has no agt_ prefix: %q", res.Value)
	}
	if res.Token.State != "active" {
		t.Fatalf("a freshly minted token is %q, want active", res.Token.State)
	}
	return res.Value
}

// do sends one JSON-RPC message and returns the HTTP status, the response
// headers and the raw body. It is the low level every other method sits on,
// and the one the refusal cases use: a rejection here is an HTTP status, not
// a JSON-RPC error.
func (c *httpPlaneClient) do(body []byte) (int, http.Header, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		c.t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		c.t.Fatalf("POST %s: %v", c.url, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		c.t.Fatalf("reading response: %v", err)
	}
	return res.StatusCode, res.Header, raw
}

// rpc sends one request and decodes the JSON-RPC answer, failing on any
// non-200 status: a caller reaching this method has already been authorized,
// so an HTTP-level refusal here is a real failure rather than an outcome.
func (c *httpPlaneClient) rpc(method string, params any) (json.RawMessage, *rpcError) {
	c.t.Helper()
	c.nextID++
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params,
	})
	if err != nil {
		c.t.Fatalf("encoding %s: %v", method, err)
	}
	status, header, raw := c.do(frame)
	if status != http.StatusOK {
		c.t.Fatalf("%s: HTTP %d\n%s", method, status, raw)
	}
	if id := header.Get("Mcp-Session-Id"); id != "" {
		c.session = id
	}
	var m rpcMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		c.t.Fatalf("%s: response is not JSON-RPC: %v\n%s", method, err, raw)
	}
	return m.Result, m.Error
}

// initialize performs the handshake and asserts the endpoint minted a session
// id, which is what every later request is resolved against.
func (c *httpPlaneClient) initialize() {
	c.t.Helper()
	res, rpcErr := c.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agenthub-e2e-http", "version": "0"},
	})
	if rpcErr != nil {
		c.t.Fatalf("initialize over http failed: %v", rpcErr)
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		c.t.Fatalf("initialize result: %v\n%s", err, res)
	}
	if init.ServerInfo.Name != "agenthub" {
		c.t.Fatalf("the http face does not identify as agenthub: %s", res)
	}
	if c.session == "" {
		c.t.Fatal("initialize bound no Mcp-Session-Id")
	}
}

// waitReady retries the handshake until the listener answers. `daemon start`
// returns once the CONTROL socket is up, and the data plane is a second bind:
// dialing immediately can lose the race on a loaded machine.
func (c *httpPlaneClient) waitReady(budget time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(budget)
	for {
		req, _ := http.NewRequest(http.MethodPost, c.url, strings.NewReader("{}"))
		res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err == nil {
			_ = res.Body.Close()
			c.initialize()
			return
		}
		var netErr net.Error
		if !errors.As(err, &netErr) && !strings.Contains(err.Error(), "connection refused") {
			c.t.Fatalf("dialing %s: %v", c.url, err)
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("the data plane never listened on %s within %s (last: %v)", c.url, budget, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// listTools returns the exposed tool names.
func (c *httpPlaneClient) listTools() []string {
	c.t.Helper()
	res, rpcErr := c.rpc("tools/list", map[string]any{})
	if rpcErr != nil {
		c.t.Fatalf("tools/list over http failed: %v", rpcErr)
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		c.t.Fatalf("tools/list result: %v\n%s", err, res)
	}
	names := make([]string, 0, len(list.Tools))
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// waitForTool polls tools/list until name shows up. The gateway behind this
// face answers from its cache first and fills the live catalog in afterwards,
// exactly as it does for a stdio client.
func (c *httpPlaneClient) waitForTool(name string, budget time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(budget)
	var last []string
	for time.Now().Before(deadline) {
		last = c.listTools()
		for _, n := range last {
			if n == name {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.t.Fatalf("tool %q never appeared over http within %s; last: %v", name, budget, last)
}

// callTool invokes one tool, retrying the transient "still connecting" error
// the gateway raises before its downstreams are up.
func (c *httpPlaneClient) callTool(name string, args any, budget time.Duration) (json.RawMessage, *rpcError) {
	c.t.Helper()
	deadline := time.Now().Add(budget)
	for {
		res, rpcErr := c.rpc("tools/call", map[string]any{"name": name, "arguments": args})
		if rpcErr == nil || rpcErr.Code != codeRetryBusy || !time.Now().Before(deadline) {
			return res, rpcErr
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// text flattens a tools/call result, asserting the tool did not report one of
// its own errors.
func (c *httpPlaneClient) text(res json.RawMessage) string {
	c.t.Helper()
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		c.t.Fatalf("tools/call result: %v\n%s", err, res)
	}
	if out.IsError {
		c.t.Fatalf("tool reported isError: %s", res)
	}
	var sb strings.Builder
	for _, item := range out.Content {
		if item.Type == "text" {
			sb.WriteString(item.Text)
		}
	}
	return sb.String()
}

// TestHTTPPlaneRoundTripWithAnAgentToken is the always-on acceptance case for
// the HTTP face: one real daemon, one real token, one real downstream, and the
// whole chain agent → httpbridge → gateway → pipeline → server → result.
//
// It also pins the two joins nothing else can see. The token is minted by a
// SEPARATE process before the daemon starts, so a passing handshake proves the
// daemon opened the same store `token create` wrote; and the tool it calls is
// the fixture downstream, so the answer proves this face reaches the same
// pipeline rather than a parallel assembly of its own.
func TestHTTPPlaneRoundTripWithAnAgentToken(t *testing.T) {
	dataDir, socket, env := sandbox(t)

	runAgenthubEnv(t, env, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "alpha", "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")

	// Destructive tier: the fixture's tool declares no annotations, and an
	// unannotated tool counts as destructive (internal/tier.ToolTier fails
	// closed). The tier gate itself is the next test's subject.
	token := mintToken(t, env, "agent", "--tier", "destructive")

	url := startHTTPDaemon(t, dataDir, socket, env)
	c := &httpPlaneClient{t: t, url: url, token: token}
	c.waitReady(30 * time.Second)

	c.waitForTool("alpha__echo", 45*time.Second)

	res, rpcErr := c.callTool("alpha__echo", map[string]any{"marker": "e2e-http-plane"}, 45*time.Second)
	if rpcErr != nil {
		t.Fatalf("tools/call over http failed: %v", rpcErr)
	}
	if got := c.text(res); !strings.Contains(got, "e2e-http-plane") {
		t.Fatalf("the downstream answer did not come back over http: %q", got)
	}

	// The session the endpoint minted is the one the daemon accounts for: a
	// data plane whose sessions were invisible to `session ls` would leave an
	// operator unable to see, or kill, an agent that was misbehaving.
	rows := listSessions(t, env, "")
	if len(rows) == 0 {
		t.Fatal("the http session is in no `session ls` listing")
	}
	t.Logf("http session(s) visible to the operator: %+v", rows)
}

// TestHTTPPlaneRefusesAnUnauthenticatedCaller is the credential half of the
// same round trip: the endpoint is up and serving, and a request without a
// bearer token gets nowhere near the pipeline.
//
// The token is what grades this face, so "no token" must be an HTTP-level
// refusal carrying a challenge — not a JSON-RPC error, and never a default
// identity. It runs against the SAME live endpoint shape as the case above so
// that a pass cannot come from a listener that was simply broken.
func TestHTTPPlaneRefusesAnUnauthenticatedCaller(t *testing.T) {
	dataDir, socket, env := sandbox(t)

	runAgenthubEnv(t, env, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "alpha", "--no-probe")
	token := mintToken(t, env, "agent", "--tier", "destructive")

	url := startHTTPDaemon(t, dataDir, socket, env)
	authed := &httpPlaneClient{t: t, url: url, token: token}
	authed.waitReady(30 * time.Second)

	anon := &httpPlaneClient{t: t, url: url}
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	status, header, body := anon.do(frame)
	if status != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated POST answered HTTP %d, want 401\n%s", status, body)
	}
	if !strings.Contains(header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("the refusal carries no Bearer challenge: %q", header.Get("WWW-Authenticate"))
	}

	// A wrong token is the same refusal. Distinguishing the two would tell a
	// caller whether a guessed value existed.
	wrong := &httpPlaneClient{t: t, url: url, token: "agt_" + strings.Repeat("0", 64)}
	if status, _, body = wrong.do(frame); status != http.StatusUnauthorized {
		t.Fatalf("a wrong token answered HTTP %d, want 401\n%s", status, body)
	}
}
