package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/daemon"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
	"github.com/dinstein/agent-hub/internal/tier"
)

// --- test fixtures ---------------------------------------------------------

// seedServers writes one enabled stdio server entry per id into dataDir's
// registry. It runs BEFORE the daemon starts, which is the whole reason this
// file does not reuse startDaemon's self-allocated temp dir.
func seedServers(t *testing.T, resolver *platform.Resolver, ids ...string) {
	t.Helper()
	dir, err := resolver.RegistryDir()
	if err != nil {
		t.Fatalf("RegistryDir: %v", err)
	}
	store, err := registry.Open(dir)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	err = store.Update(context.Background(), func(tx *registry.Tx) error {
		if tx.Servers.V.Servers == nil {
			tx.Servers.V.Servers = map[string]registry.Doc[registry.ServerEntry]{}
		}
		for _, id := range ids {
			tx.Servers.V.Servers[id] = registry.Doc[registry.ServerEntry]{V: registry.ServerEntry{
				Transport: "stdio", Command: "unused-in-tests", Enabled: true,
			}}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("registry.Update: %v", err)
	}
}

// mintToken creates one agent token in dataDir's store and returns its
// plaintext.
func mintToken(t *testing.T, dataDir string, spec httpbridge.CreateSpec) string {
	t.Helper()
	store, err := httpbridge.OpenStore(dataDir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	_, value, err := store.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("token create %q: %v", spec.Name, err)
	}
	return value
}

// revokeToken revokes a stored token by name.
func revokeToken(t *testing.T, dataDir, name string) {
	t.Helper()
	store, err := httpbridge.OpenStore(dataDir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if _, err := store.Revoke(context.Background(), name, time.Now()); err != nil {
		t.Fatalf("token revoke %q: %v", name, err)
	}
}

// scriptedDial serves one in-process fakemcp script per server id.
func scriptedDial(scripts map[string]*fakemcp.Script) downstream.DialFunc {
	return func(_ context.Context, spec downstream.Spec) (transport.Transport, error) {
		sc, ok := scripts[spec.ID]
		if !ok {
			return nil, fmt.Errorf("no script for server %q", spec.ID)
		}
		return fakemcp.Connect(sc)
	}
}

// httpDaemon is a running daemon plus the address of its MCP endpoint.
type httpDaemon struct {
	*daemonHandle
	addr string
}

// startHTTPDaemon starts a daemon whose data plane serves on an ephemeral
// loopback port, with a fakemcp downstream behind it.
func startHTTPDaemon(t *testing.T, mutate func(*daemon.Config)) *httpDaemon {
	t.Helper()
	dataDir := t.TempDir()
	resolver := testResolver(t, dataDir)
	seedServers(t, resolver, "fake")

	socket, err := resolver.CtlSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	runDir, err := resolver.RunDir()
	if err != nil {
		t.Fatal(err)
	}

	ready := make(chan daemon.Info, 1)
	addrs := make(chan []string, 1)
	cfg := daemon.Config{
		Version:       "test-daemon",
		Resolver:      resolver,
		Log:           slog.New(slog.DiscardHandler),
		OnReady:       func(i daemon.Info) { ready <- i },
		OnHTTPReady:   func(a []string) { addrs <- a },
		Watch:         registry.WatchOptions{Debounce: 20 * time.Millisecond, Poll: 100 * time.Millisecond},
		ShutdownGrace: 200 * time.Millisecond,
		HTTPAddr:      "127.0.0.1:0",
		Dial:          scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	}
	if mutate != nil {
		mutate(&cfg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &daemonHandle{resolver: resolver, socket: socket, runDir: runDir, cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- daemon.Run(ctx, cfg) }()
	t.Cleanup(func() { _ = h.stop(t) })

	select {
	case <-ready:
	case err := <-h.done:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("daemon never became ready")
	}

	var bound []string
	select {
	case bound = <-addrs:
	case <-time.After(testTimeout):
		t.Fatal("the MCP data plane never reported a bound address")
	}
	if len(bound) == 0 {
		t.Fatal("the MCP data plane reported no bound address")
	}
	return &httpDaemon{daemonHandle: h, addr: bound[0]}
}

// mcpPost sends one JSON-RPC message to the endpoint and returns the raw HTTP
// response (already read).
func (d *httpDaemon) mcpPost(t *testing.T, bearer, session string, msg any) (status int, hdr http.Header, body []byte) {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	url := "http://" + d.addr + httpbridge.DefaultPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if session != "" {
		req.Header.Set(httpbridge.SessionHeader, session)
	}
	client := &http.Client{Timeout: testTimeout}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(res.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res.StatusCode, res.Header, buf.Bytes()
}

// rpc sends one JSON-RPC request and decodes the answer, failing on any
// non-200.
func (d *httpDaemon) rpc(t *testing.T, bearer, session, method string, params any) *mcp.Response {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		raw = b
	}
	status, _, body := d.mcpPost(t, bearer, session, mcp.NewRequest(mcp.NewIntID(1), method, raw))
	if status != http.StatusOK {
		t.Fatalf("%s: HTTP %d (%s)", method, status, body)
	}
	var res mcp.Response
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("decode %s response: %v (%s)", method, err, body)
	}
	return &res
}

// initialize performs the handshake and returns the bound session id.
func (d *httpDaemon) initialize(t *testing.T, bearer string) string {
	t.Helper()
	raw, err := json.Marshal(mcp.InitializeParams{
		ProtocolVersion: mcp.ProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "http-test-client", Version: "0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, hdr, body := d.mcpPost(t, bearer, "",
		mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize, raw))
	if status != http.StatusOK {
		t.Fatalf("initialize: HTTP %d (%s)", status, body)
	}
	session := hdr.Get(httpbridge.SessionHeader)
	if session == "" {
		t.Fatalf("initialize returned no %s header", httpbridge.SessionHeader)
	}
	return session
}

// --- tests -----------------------------------------------------------------

// TestHTTPDataPlaneServesRealCall is the end-to-end acceptance for the wiring:
// a valid agent token initializes a session over HTTP and executes a real
// downstream tool call through the daemon.
func TestHTTPDataPlaneServesRealCall(t *testing.T) {
	var value string
	d := startHTTPDaemon(t, func(cfg *daemon.Config) {
		// The token must exist before the bind: AuthorizeBind refuses an
		// endpoint nobody holds a credential for.
		dir, err := cfg.Resolver.DataDir()
		if err != nil {
			t.Fatal(err)
		}
		value = mintToken(t, dir, httpbridge.CreateSpec{Name: "agent", Tier: tier.Destructive})
	})

	session := d.initialize(t, value)

	// tools/list eventually shows the downstream's tool. The observation
	// separates the two ways this waits forever: an error answer (the catalog
	// is not servable at all) from a clean list that simply lacks the tool
	// (the downstream never connected, or governance filtered it out).
	waitForDetail(t, "tools/list to expose fake__echo over HTTP", func() (bool, string) {
		res := d.rpc(t, value, session, mcp.MethodToolsList, nil)
		if res.Error != nil {
			return false, fmt.Sprintf("tools/list returned error %+v", res.Error)
		}
		var list mcp.ListToolsResult
		if err := json.Unmarshal(res.Result, &list); err != nil {
			t.Fatalf("decode tools/list: %v", err)
		}
		names := make([]string, 0, len(list.Tools))
		for _, def := range list.Tools {
			if def.Name == "fake__echo" {
				return true, ""
			}
			names = append(names, def.Name)
		}
		return false, fmt.Sprintf("tools/list returned %d tools without fake__echo: %v", len(names), names)
	})

	// tools/call reaches the real (fake) downstream and echoes the marker.
	var call mcp.CallResult
	waitForDetail(t, "tools/call to succeed over HTTP", func() (bool, string) {
		res := d.rpc(t, value, session, mcp.MethodToolsCall, mcp.CallToolParams{
			Name:      "fake__echo",
			Arguments: json.RawMessage(`{"marker":"http-e2e"}`),
		})
		if res.Error != nil {
			return false, fmt.Sprintf("tools/call returned error %+v", res.Error)
		}
		if err := json.Unmarshal(res.Result, &call); err != nil {
			t.Fatalf("decode tools/call result: %v", err)
		}
		return true, ""
	})
	if !strings.Contains(string(mustJSON(t, call)), "http-e2e") {
		t.Fatalf("tools/call result did not echo the marker: %s", mustJSON(t, call))
	}
}

// TestHTTPDataPlaneRejectsBadCredentials: unknown, revoked and expired tokens
// are all one undifferentiated 401, and the endpoint answers nothing without a
// credential at all.
func TestHTTPDataPlaneRejectsBadCredentials(t *testing.T) {
	var good, expired, revoked string
	d := startHTTPDaemon(t, func(cfg *daemon.Config) {
		dir, err := cfg.Resolver.DataDir()
		if err != nil {
			t.Fatal(err)
		}
		good = mintToken(t, dir, httpbridge.CreateSpec{Name: "good", Tier: tier.Destructive})
		expired = mintToken(t, dir, httpbridge.CreateSpec{
			Name: "expired", Tier: tier.Destructive,
			ExpiresAt: time.Now().Add(-time.Hour),
		})
		revoked = mintToken(t, dir, httpbridge.CreateSpec{Name: "revoked", Tier: tier.Destructive})
		revokeToken(t, dir, "revoked")
	})

	// The good token proves the endpoint is live, so a 401 below is a
	// credential verdict and not a broken listener.
	if s := d.initialize(t, good); s == "" {
		t.Fatal("the valid token did not bind a session")
	}

	for _, tc := range []struct{ name, bearer string }{
		{"no credential", ""},
		{"unknown token", "agt_" + strings.Repeat("0", 64)},
		{"expired token", expired},
		{"revoked token", revoked},
		{"admin-shaped garbage", "not-a-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _, body := d.mcpPost(t, tc.bearer, "",
				mcp.NewRequest(mcp.NewIntID(1), mcp.MethodPing, nil))
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", status, body)
			}
			if !strings.Contains(string(body), httpbridge.CodeUnauthorized) {
				t.Fatalf("body = %s, want the frozen %q code", body, httpbridge.CodeUnauthorized)
			}
		})
	}
}

// TestHTTPDataPlaneTokenTierIsEnforced: a read-only token is refused by the
// pipeline's token tier gate on a tool whose server declared no annotations
// (absent annotations ⇒ destructive, fail-closed).
func TestHTTPDataPlaneTokenTierIsEnforced(t *testing.T) {
	var readOnly string
	d := startHTTPDaemon(t, func(cfg *daemon.Config) {
		dir, err := cfg.Resolver.DataDir()
		if err != nil {
			t.Fatal(err)
		}
		readOnly = mintToken(t, dir, httpbridge.CreateSpec{Name: "reader", Tier: tier.Read})
	})

	session := d.initialize(t, readOnly)
	waitForDetail(t, "the token tier gate to reject the HTTP call", func() (bool, string) {
		res := d.rpc(t, readOnly, session, mcp.MethodToolsCall, mcp.CallToolParams{
			Name: "fake__echo", Arguments: json.RawMessage(`{}`),
		})
		if res.Error == nil {
			t.Fatal("a read-only token executed an unannotated (⇒ destructive) tool over HTTP")
		}
		if strings.Contains(res.Error.Message, "E_TOKEN_TIER_DENIED") {
			return true, ""
		}
		// Denied, but by a different gate — usually the catalog is not live
		// yet, so the call fails busy long before it reaches the tier gate.
		return false, fmt.Sprintf("rejected by something other than the tier gate: %+v", res.Error)
	})
}

// TestHTTPDataPlaneServerAllowlistNarrowsScope: a token allowlisting a
// different server is denied by the SCOPE gate — the allowlist joins the
// ordinary four-layer intersection rather than a bespoke HTTP filter.
func TestHTTPDataPlaneServerAllowlistNarrowsScope(t *testing.T) {
	var narrow string
	d := startHTTPDaemon(t, func(cfg *daemon.Config) {
		dir, err := cfg.Resolver.DataDir()
		if err != nil {
			t.Fatal(err)
		}
		narrow = mintToken(t, dir, httpbridge.CreateSpec{
			Name: "narrow", Tier: tier.Destructive, Servers: []string{"other"},
		})
	})

	session := d.initialize(t, narrow)
	waitFor(t, "the scope gate to reject the HTTP call", func() bool {
		res := d.rpc(t, narrow, session, mcp.MethodToolsCall, mcp.CallToolParams{
			Name: "fake__echo", Arguments: json.RawMessage(`{}`),
		})
		if res.Error == nil {
			t.Fatal("a token allowlisting another server reached fake__echo over HTTP")
		}
		return strings.Contains(res.Error.Message, "E_SCOPE_DENIED")
	})

	res := d.rpc(t, narrow, session, mcp.MethodToolsList, nil)
	if res.Error != nil {
		t.Fatalf("tools/list: %v", res.Error)
	}
	var list mcp.ListToolsResult
	if err := json.Unmarshal(res.Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	for _, def := range list.Tools {
		if def.Name == "fake__echo" {
			t.Fatalf("tools/list exposes %q to a token that allowlists another server", def.Name)
		}
	}
}

// TestHTTPDataPlaneOffByDefault: with no HTTPAddr the daemon runs with NO
// listener at all — not a listener on a default port. The proof is twofold:
// the readiness callback never fires, and nothing is accepting on the port a
// previous run of this test's sibling would have used.
func TestHTTPDataPlaneOffByDefault(t *testing.T) {
	// Bind a port ourselves, release it, and prove the daemon does not claim
	// it: a daemon that defaulted to "some port" would have to pick one, and
	// the assertion below is that it picks none.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}

	var mu sync.Mutex
	fired := false
	h := startDaemon(t, func(cfg *daemon.Config) {
		cfg.OnHTTPReady = func([]string) {
			mu.Lock()
			fired = true
			mu.Unlock()
		}
		cfg.Dial = scriptedDial(map[string]*fakemcp.Script{})
	})
	if h == nil {
		t.Fatal("daemon did not start")
	}

	mu.Lock()
	defer mu.Unlock()
	if fired {
		t.Fatal("the data plane bound an address without one being configured")
	}
	conn, derr := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if derr == nil {
		_ = conn.Close()
		t.Fatalf("something is accepting on %s; no listener was configured", addr)
	}
}

// TestHTTPDataPlaneRefusesNonLoopbackWithoutConfirmation: a non-loopback
// address without the explicit confirmation FAILS the daemon. It must not
// silently fall back to loopback — that would leave the running system quietly
// disagreeing with its configuration.
func TestHTTPDataPlaneRefusesNonLoopbackWithoutConfirmation(t *testing.T) {
	dataDir := t.TempDir()
	resolver := testResolver(t, dataDir)
	mintToken(t, dataDir, httpbridge.CreateSpec{Name: "agent", Tier: tier.Read})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := daemon.Run(ctx, daemon.Config{
		Version:       "test-daemon",
		Resolver:      resolver,
		Log:           slog.New(slog.DiscardHandler),
		ShutdownGrace: 200 * time.Millisecond,
		HTTPAddr:      "0.0.0.0:0",
	})
	if err == nil {
		t.Fatal("the daemon started with a non-loopback data plane and no confirmation")
	}
	if !strings.Contains(err.Error(), "not a loopback address") {
		t.Fatalf("error = %v, want the non-loopback refusal", err)
	}
}

// TestHTTPDataPlaneRefusesUnauthorizedBind: an endpoint nobody holds a
// credential for is refused (httpbridge.AuthorizeBind), and the refusal fails
// the daemon rather than degrading it.
func TestHTTPDataPlaneRefusesUnauthorizedBind(t *testing.T) {
	resolver := testResolver(t, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := daemon.Run(ctx, daemon.Config{
		Version:       "test-daemon",
		Resolver:      resolver,
		Log:           slog.New(slog.DiscardHandler),
		ShutdownGrace: 200 * time.Millisecond,
		HTTPAddr:      "127.0.0.1:0",
	})
	if !errors.Is(err, httpbridge.ErrBindUnauthorized) {
		t.Fatalf("error = %v, want httpbridge.ErrBindUnauthorized", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
