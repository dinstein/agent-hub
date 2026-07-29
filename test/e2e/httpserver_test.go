package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// fakeHTTPServer is an MCP Streamable HTTP front end over the SAME script
// interpreter the stdio fake uses (internal/testutil/fakemcp): each POST is
// one JSON-RPC message, fed to fakemcp.Serve as a one-frame stream, and the
// frames it writes back become the response body.
//
// Reusing the interpreter is the point — the fault scripts written for the
// stdio suite mean the same thing here, and there is no second, subtly
// different fake server to keep honest.
//
// Deliberate limits (this is a test fixture, not a server implementation):
// only application/json answers, no SSE stream, no session id, no
// server-initiated requests. GET is answered 405, which the transport
// treats as "this server offers no notification stream" and leaves alone.
type fakeHTTPServer struct {
	srv    *httptest.Server
	script *fakemcp.Script

	// requireToken, when non-empty, makes every request without a matching
	// bearer token answer 401. It is fixed at construction: the handler
	// runs on the http server's goroutines, so a later write would be a
	// data race, not a knob.
	requireToken string
	calls        atomic.Int64
}

func newFakeHTTPServer(t *testing.T, script *fakemcp.Script) *fakeHTTPServer {
	return newFakeHTTPServerAuth(t, script, "")
}

func newFakeHTTPServerAuth(t *testing.T, script *fakemcp.Script, requireToken string) *fakeHTTPServer {
	t.Helper()
	f := &fakeHTTPServer{script: script, requireToken: requireToken}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// url is the MCP endpoint the registry entry points at.
func (f *fakeHTTPServer) url() string { return f.srv.URL + "/mcp" }

func (f *fakeHTTPServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	f.calls.Add(1)
	if f.requireToken != "" && r.Header.Get("Authorization") != "Bearer "+f.requireToken {
		w.Header().Set("WWW-Authenticate", `Bearer realm="fake"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(http.MaxBytesReader(w, r.Body, 1<<20)); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// The interpreter speaks newline-delimited frames; one POST is one frame.
	in := bytes.NewReader(append(bytes.TrimRight(body.Bytes(), "\n"), '\n'))
	var out bytes.Buffer
	if err := fakemcp.Serve(context.Background(), in, &out, io.Discard, f.script); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// A notification produces no frame: the binding answers 202 Accepted.
	line := firstFrame(out.Bytes())
	if len(line) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(line)
}

// firstFrame returns the first newline-delimited frame of buf.
func firstFrame(buf []byte) []byte {
	i := bytes.IndexByte(buf, '\n')
	if i < 0 {
		return bytes.TrimSpace(buf)
	}
	return bytes.TrimSpace(buf[:i])
}

// TestHTTPDownstreamRoundTrip is the http leg of the always-on e2e path:
// a real agenthub binary registers a streamable-http downstream by URL and
// then connects, lists and calls it — the same client → agenthub →
// downstream → result chain the stdio case pins, over HTTP.
//
// It drives `server test`, which connects from the CLI process itself, so
// the case covers the transport, the SSRF provenance gate and the tools/call
// path without depending on a long-lived gateway.
func TestHTTPDownstreamRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	fake := newFakeHTTPServer(t, fakemcp.Minimal())

	// `server add --url` with --local: the endpoint is loopback, which the
	// connector refuses unless the operator declares the server local.
	out, _ := runAgenthub(t, dataDir, "", "server", "add", "remote",
		"--url", fake.url(), "--transport", "http", "--local", "--json")
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("server add envelope: %s", out)
	}
	if !strings.Contains(out, `"transport":"http"`) {
		t.Fatalf("added entry is not an http entry: %s", out)
	}

	// connect → tools/list → tools/call, all through the real binary.
	out, stderr := runAgenthub(t, dataDir, "", "server", "test", "remote",
		"--tool", "echo", "--args", `{"marker":"e2e-http-roundtrip"}`, "--json")
	env := lastEnvelope(t, out)
	if !env.OK {
		t.Fatalf("server test failed: %s\nstderr: %s", out, stderr)
	}
	var res struct {
		Transport string   `json:"transport"`
		ToolCount int      `json:"toolCount"`
		Tools     []string `json:"tools"`
		Call      *struct {
			Tool    string `json:"tool"`
			Text    string `json:"text"`
			IsError bool   `json:"isError"`
		} `json:"call"`
	}
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatalf("decode result: %v\n%s", err, out)
	}
	if res.Transport != "http" {
		t.Errorf("transport = %q, want http", res.Transport)
	}
	if res.ToolCount != 1 || res.Tools[0] != "echo" {
		t.Fatalf("tools = %v, want [echo]", res.Tools)
	}
	if res.Call == nil || res.Call.IsError || !strings.Contains(res.Call.Text, "e2e-http-roundtrip") {
		t.Fatalf("tools/call result = %+v", res.Call)
	}
	if fake.calls.Load() == 0 {
		t.Fatal("the fake http server was never contacted")
	}
	t.Logf("http downstream answered after %d requests: %s", fake.calls.Load(), res.Call.Text)
}

// TestHTTPDownstreamSecretHeader pins the credential path end to end: the
// registry holds only the PLACEHOLDER, the value comes from the vault (here
// its environment level, AGENTHUB_SECRET_<KEY>) and the downstream sees the
// resolved bearer token.
func TestHTTPDownstreamSecretHeader(t *testing.T) {
	dataDir := t.TempDir()
	fake := newFakeHTTPServerAuth(t, fakemcp.Minimal(), "e2e-token")

	out, _ := runAgenthub(t, dataDir, "", "server", "add", "remote",
		"--url", fake.url(), "--transport", "http", "--local",
		"--header", "Authorization=Bearer ${SECRET_REMOTE_TOKEN}", "--json")
	if !strings.Contains(out, `${SECRET_REMOTE_TOKEN}`) {
		t.Fatalf("the registry entry must keep the placeholder verbatim: %s", out)
	}

	env := append(testEnv(dataDir), "AGENTHUB_SECRET_REMOTE_TOKEN=e2e-token")
	out, stderr := runAgenthubEnv(t, env, "", "server", "test", "remote", "--json")
	if e := lastEnvelope(t, out); !e.OK {
		t.Fatalf("server test with a resolved secret failed: %s\n%s", out, stderr)
	}
	// And the negative: without the secret the placeholder must NOT reach
	// the wire as literal text — the connect fails before any request.
	before := fake.calls.Load()
	code, out := runAgenthubExit(t, dataDir, "", "server", "test", "remote", "--json")
	if code == 0 {
		t.Fatalf("server test succeeded without the secret: %s", out)
	}
	if fake.calls.Load() != before {
		t.Fatal("a request went out with an unresolved placeholder")
	}
}

// TestHTTPDownstreamRefusedWithoutLocalProvenance is the fail-closed half:
// the same loopback URL without --local must be refused at add time, so a
// pasted README snippet can never point agenthub at a local address.
func TestHTTPDownstreamRefusedWithoutLocalProvenance(t *testing.T) {
	dataDir := t.TempDir()
	fake := newFakeHTTPServer(t, fakemcp.Minimal())

	code, out := runAgenthubExit(t, dataDir, "", "server", "add", "remote",
		"--url", fake.url(), "--transport", "http", "--json")
	if code == 0 {
		t.Fatalf("adding a loopback endpoint without --local succeeded: %s", out)
	}
	if !strings.Contains(out, `"ok":false`) {
		t.Fatalf("envelope = %s", out)
	}
}

// e2eEnvelope mirrors the --json envelope for the e2e suite.
type e2eEnvelope struct {
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data"`
	Warnings []string        `json:"warnings"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// lastEnvelope decodes the LAST line of a --json stream: progress commands
// emit NDJSON events first and the result envelope last.
func lastEnvelope(t *testing.T, out string) e2eEnvelope {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var env e2eEnvelope
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &env); err != nil {
		t.Fatalf("last line is not a JSON envelope: %v\n%s", err, out)
	}
	return env
}
