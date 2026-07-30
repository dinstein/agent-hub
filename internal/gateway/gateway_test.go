package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

const testTimeout = 10 * time.Second

// testResolver pins AGENTHUB_DATA_DIR at dir without touching process env
// (tests stay parallelizable).
func testResolver(dir string) *platform.Resolver {
	return &platform.Resolver{
		LookupEnv: func(key string) (string, bool) {
			if key == platform.EnvDataDir {
				return dir, true
			}
			return "", false
		},
	}
}

// seedRegistry writes one enabled stdio server entry per given id.
func seedRegistry(t *testing.T, resolver *platform.Resolver, ids ...string) {
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
		// Full mode, pinned rather than inherited. These tests name the
		// downstream tool they expect in tools/list, and only full mode puts
		// it there — the default is lazy, whose list is the five meta-tools.
		// A test about routing, hot reload or rate limits must not break
		// because the presentation default moved; the default has its own
		// coverage in discovery and in the lazy e2e chain.
		tx.Governance.V.Discovery = "full"
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

// scriptedDial returns a DialFunc serving one in-process fakemcp script per
// server id.
func scriptedDial(scripts map[string]*fakemcp.Script) downstream.DialFunc {
	return func(_ context.Context, spec downstream.Spec) (transport.Transport, error) {
		sc, ok := scripts[spec.ID]
		if !ok {
			return nil, fmt.Errorf("no script for server %q", spec.ID)
		}
		return fakemcp.Connect(sc)
	}
}

// hangingDial blocks until the gateway shuts down: the downstream never
// comes up.
func hangingDial(ctx context.Context, _ downstream.Spec) (transport.Transport, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// gatedDial wraps dial so no connection completes before release is closed.
func gatedDial(release <-chan struct{}, dial downstream.DialFunc) downstream.DialFunc {
	return func(ctx context.Context, spec downstream.Spec) (transport.Transport, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return dial(ctx, spec)
	}
}

// testClient is a hand-written MCP client over the gateway's stdio pair,
// built directly on the mcp facade (FrameReader/FrameWriter).
type testClient struct {
	t  *testing.T
	fw *mcp.FrameWriter
	wc io.Closer // closing = client disconnect (gateway sees EOF)

	nextID atomic.Int64

	answerRoots bool
	roots       []mcp.Root
	rootsCalls  atomic.Int64

	mu     sync.Mutex
	resps  map[string]*mcp.Response
	notifs []*mcp.Notification

	reverseWG sync.WaitGroup // reverse-RPC answers run off the read loop
	readDone  chan struct{}
}

// startGateway assembles a gateway over io.Pipe pairs, runs it, and returns
// the white-box handle, the client, and the run error channel.
func startGateway(t *testing.T, cfg Config) (*gateway, *testClient, chan error) {
	t.Helper()
	gwInR, gwInW := io.Pipe()   // client → gateway
	gwOutR, gwOutW := io.Pipe() // gateway → client
	cfg.In = gwInR
	cfg.Out = gwOutW
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	g, err := newGateway(cfg)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- g.run(context.Background()) }()

	c := &testClient{
		t:        t,
		fw:       mcp.NewFrameWriter(gwInW),
		wc:       gwInW,
		resps:    map[string]*mcp.Response{},
		readDone: make(chan struct{}),
	}
	go c.readLoop(gwOutR)

	t.Cleanup(func() {
		_ = gwInW.Close() // EOF: gateway run loop exits
		select {
		case <-errCh:
		case <-time.After(testTimeout):
			t.Error("gateway did not exit on EOF")
		}
		g.shutdown()
		_ = gwOutW.Close()
		<-c.readDone
	})
	return g, c, errCh
}

func (c *testClient) readLoop(r io.Reader) {
	defer close(c.readDone)
	defer c.reverseWG.Wait()
	fr := mcp.NewFrameReader(r)
	for {
		line, err := fr.Next()
		if err != nil {
			return
		}
		msg, perr := mcp.ParseMessage(line)
		if perr != nil {
			c.t.Errorf("gateway sent malformed frame: %v", perr)
			return
		}
		switch m := msg.(type) {
		case *mcp.Response:
			c.mu.Lock()
			c.resps[m.ID.Key()] = m
			c.mu.Unlock()
		case *mcp.Notification:
			c.mu.Lock()
			c.notifs = append(c.notifs, m)
			c.mu.Unlock()
		case *mcp.Request:
			// Answer off the read loop: the pipes are unbuffered, so
			// replying inline can deadlock against the gateway's own read
			// loop writing a response in the other direction.
			c.reverseWG.Add(1)
			go func() {
				defer c.reverseWG.Done()
				c.handleReverse(m)
			}()
		}
	}
}

// handleReverse answers gateway-initiated reverse RPCs (roots/list).
func (c *testClient) handleReverse(req *mcp.Request) {
	if req.Method == mcp.MethodRootsList && c.answerRoots {
		c.rootsCalls.Add(1)
		raw, err := json.Marshal(mcp.ListRootsResult{Roots: c.roots})
		if err != nil {
			c.t.Errorf("marshal roots: %v", err)
			return
		}
		_ = c.fw.WriteFrame(mcp.NewResponse(req.ID, raw))
		return
	}
	_ = c.fw.WriteFrame(mcp.NewErrorResponse(req.ID, &mcp.Error{
		Code: mcp.CodeMethodNotFound, Message: "test client: " + req.Method,
	}))
}

// send writes a request and returns its id.
func (c *testClient) send(method string, params any) mcp.ID {
	c.t.Helper()
	raw := marshalParams(c.t, params)
	id := mcp.NewIntID(c.nextID.Add(1))
	if err := c.fw.WriteFrame(mcp.NewRequest(id, method, raw)); err != nil {
		c.t.Fatalf("send %s: %v", method, err)
	}
	return id
}

func (c *testClient) notify(method string, params any) {
	c.t.Helper()
	if err := c.fw.WriteFrame(mcp.NewNotification(method, marshalParams(c.t, params))); err != nil {
		c.t.Fatalf("notify %s: %v", method, err)
	}
}

// call sends a request and waits for its response.
func (c *testClient) call(method string, params any) *mcp.Response {
	c.t.Helper()
	id := c.send(method, params)
	resp := c.waitResponse(id, testTimeout)
	if resp == nil {
		c.t.Fatalf("no response to %s (id %s)", method, id)
	}
	return resp
}

// waitResponse polls for the response to id; nil on timeout.
func (c *testClient) waitResponse(id mcp.ID, timeout time.Duration) *mcp.Response {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		resp := c.resps[id.Key()]
		c.mu.Unlock()
		if resp != nil {
			return resp
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

func (c *testClient) hasResponse(id mcp.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resps[id.Key()] != nil
}

// waitNotification blocks until a notification with method arrived.
func (c *testClient) waitNotification(method string) {
	c.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, n := range c.notifs {
			if n.Method == method {
				c.mu.Unlock()
				return
			}
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("notification %s never arrived", method)
}

// initialize performs the client half of the handshake.
func (c *testClient) initialize(version string, caps mcp.ClientCapabilities) mcp.InitializeResult {
	c.t.Helper()
	resp := c.call(mcp.MethodInitialize, mcp.InitializeParams{
		ProtocolVersion: version,
		Capabilities:    caps,
		ClientInfo:      mcp.Implementation{Name: "gateway-test-client", Version: "0"},
	})
	if resp.Error != nil {
		c.t.Fatalf("initialize error: %v", resp.Error)
	}
	var res mcp.InitializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		c.t.Fatalf("decode initialize result: %v", err)
	}
	c.notify(mcp.NotificationInitialized, nil)
	return res
}

func (c *testClient) listTools() []mcp.ToolDef {
	c.t.Helper()
	resp := c.call(mcp.MethodToolsList, nil)
	if resp.Error != nil {
		c.t.Fatalf("tools/list error: %v", resp.Error)
	}
	var res mcp.ListToolsResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		c.t.Fatalf("decode tools/list: %v", err)
	}
	return res.Tools
}

func marshalParams(t *testing.T, params any) json.RawMessage {
	t.Helper()
	if params == nil {
		return nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return raw
}

func toolNames(tools []mcp.ToolDef) []string {
	names := make([]string, len(tools))
	for i, d := range tools {
		names[i] = d.Name
	}
	return names
}

// TestStartupSequenceAndFullCallPath walks docs/flows.md end to end:
// initialize answers while downstreams are still connecting, tools/call is
// a retryable busy error until the live router exists, tools/list_changed
// announces the first real catalog, and a full tools/call round-trips with
// every pipeline gate counted.
func TestStartupSequenceAndFullCallPath(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	release := make(chan struct{})
	g, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     gatedDial(release, scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")})),
	})

	// initialize answers immediately: the downstream dial is still gated.
	res := c.initialize("2025-06-18", mcp.ClientCapabilities{})
	if res.ProtocolVersion != "2025-06-18" {
		t.Errorf("negotiated version = %q, want the client's supported version echoed", res.ProtocolVersion)
	}
	if res.ServerInfo.Name != serverName {
		t.Errorf("serverInfo.name = %q, want %q", res.ServerInfo.Name, serverName)
	}

	// Unknown protocol version → our default.
	// (Second initialize is tolerated in M0; only the negotiation matters.)
	resp := c.call(mcp.MethodInitialize, mcp.InitializeParams{ProtocolVersion: "1999-01-01"})
	var res2 mcp.InitializeResult
	if err := json.Unmarshal(resp.Result, &res2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res2.ProtocolVersion != mcp.ProtocolVersion {
		t.Errorf("fallback version = %q, want %q", res2.ProtocolVersion, mcp.ProtocolVersion)
	}

	// No cache, downstream not connected: empty catalog, busy calls.
	if tools := c.listTools(); len(tools) != 0 {
		t.Errorf("tools/list before ready = %v, want empty", toolNames(tools))
	}
	busy := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__echo"})
	if busy.Error == nil || busy.Error.Code != codeRetryBusy {
		t.Fatalf("tools/call before ready = %+v, want retryable busy error %d", busy, codeRetryBusy)
	}

	// Unknown method → MethodNotFound; unknown notification ignored.
	if r := c.call("resources/list", nil); r.Error == nil || r.Error.Code != mcp.CodeMethodNotFound {
		t.Errorf("unknown method response = %+v, want MethodNotFound", r)
	}
	c.notify("notifications/unknown/thing", nil)

	// Let the downstream connect: list_changed must reach the client and
	// the aggregated catalog must appear.
	close(release)
	c.waitNotification(mcp.NotificationToolsListChanged)
	tools := c.listTools()
	if len(tools) != 1 || tools[0].Name != "fake__echo" {
		t.Fatalf("tools/list after ready = %v, want [fake__echo]", toolNames(tools))
	}

	// Full call path through pipeline and downstream.
	call := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__echo", Arguments: json.RawMessage(`{"x":1}`)})
	if call.Error != nil {
		t.Fatalf("tools/call error: %v", call.Error)
	}
	var cr mcp.CallResult
	if err := json.Unmarshal(call.Result, &cr); err != nil {
		t.Fatalf("decode call result: %v", err)
	}
	if !strings.Contains(string(cr.Content), `{\"x\":1}`) {
		t.Errorf("echo content = %s, want the arguments echoed", cr.Content)
	}

	// Unknown tool with nothing pending → invalid params, not busy.
	unknown := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "nope"})
	if unknown.Error == nil || unknown.Error.Code != mcp.CodeInvalidParams {
		t.Errorf("unknown tool response = %+v, want InvalidParams", unknown)
	}

	// Every gate and the shaping hook saw the call.
	for stage, n := range g.pipe.Counters() {
		if n == 0 {
			t.Errorf("pipeline counter %q = 0, want > 0 (all: %v)", stage, g.pipe.Counters())
		}
	}
	for _, stage := range []string{
		pipeline.GateScope, pipeline.GateTokenTier, pipeline.GatePrecheck,
		pipeline.StageDefendAndShape,
	} {
		if _, ok := g.pipe.Counters()[stage]; !ok {
			t.Errorf("pipeline counter %q missing", stage)
		}
	}
}

// TestCancelledStopsInFlightCall pins notifications/cancelled: the handler
// context is cancelled, the handler goroutine finishes without ever
// answering, and the gateway keeps serving.
func TestCancelledStopsInFlightCall(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "hang")
	script := fakemcp.Minimal("stuck").With(fakemcp.NeverRespond(mcp.MethodToolsCall))
	g, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"hang": script}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	c.waitNotification(mcp.NotificationToolsListChanged)

	id := c.send(mcp.MethodToolsCall, mcp.CallToolParams{Name: "hang__stuck"})
	waitFor(t, "call in flight", func() bool { return g.inflightLen() == 1 })

	c.notify(mcp.NotificationCancelled, mcp.CancelledParams{RequestID: id, Reason: "user gave up"})
	waitFor(t, "in-flight call cancelled", func() bool { return g.inflightLen() == 0 })

	// A cancelled request gets no response; the gateway itself stays alive.
	if pong := c.call(mcp.MethodPing, nil); pong.Error != nil {
		t.Fatalf("ping after cancel: %v", pong.Error)
	}
	if c.hasResponse(id) {
		t.Error("cancelled tools/call must not receive a response")
	}
}

// TestToolCacheRoundTrip runs the gateway twice against the same data dir:
// the first run persists the connected server's tools; the second run —
// whose downstream never comes up — still answers tools/list from the
// cache while tools/call stays a retryable busy error.
func TestToolCacheRoundTrip(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	resolver := testResolver(dataDir)
	seedRegistry(t, resolver, "fake")

	// Run 1: connect, aggregate, persist.
	run1 := func() {
		_, c, _ := startGateway(t, Config{
			ClientID: "run1",
			Resolver: resolver,
			Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
		})
		c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
		c.waitNotification(mcp.NotificationToolsListChanged)
		waitFor(t, "cache file written", func() bool {
			_, err := os.Stat(filepath.Join(dataDir, "cache", "tools", "fake.json"))
			return err == nil
		})
		_ = c.wc.Close() // disconnect; Cleanup handles the rest
	}
	run1()

	// Run 2: the downstream hangs forever — the cache answers.
	_, c, _ := startGateway(t, Config{
		ClientID: "run2",
		Resolver: resolver,
		Dial:     hangingDial,
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	tools := c.listTools()
	if len(tools) != 1 || tools[0].Name != "fake__echo" {
		t.Fatalf("cached tools/list = %v, want [fake__echo]", toolNames(tools))
	}
	busy := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__echo"})
	if busy.Error == nil || busy.Error.Code != codeRetryBusy {
		t.Fatalf("tools/call with downstream never up = %+v, want busy error", busy)
	}
}

// TestCorruptRegistryStillServesFromCache breaks servers.json on disk and
// proves the gateway still starts and answers tools/list from the cache
// (docs/flows.md: registry load failure does not kill the gateway).
func TestCorruptRegistryStillServesFromCache(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	resolver := testResolver(dataDir)
	seedRegistry(t, resolver, "fake") // creates the registry files

	// Seed the tool cache directly (as a previous healthy run would have).
	cache := newToolCache(filepath.Join(dataDir, "cache", "tools"), slog.New(slog.DiscardHandler))
	err := cache.write("fake", []mcp.ToolDef{{
		Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Corrupt the registry.
	regDir, err := resolver.RegistryDir()
	if err != nil {
		t.Fatalf("RegistryDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(regDir, "servers.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt servers.json: %v", err)
	}

	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     hangingDial,
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	tools := c.listTools()
	if len(tools) != 1 || tools[0].Name != "fake__echo" {
		t.Fatalf("tools/list with corrupt registry = %v, want [fake__echo] from cache", toolNames(tools))
	}
}

// TestDownstreamRootsReverseRPC pins the RootSource chain: a downstream
// roots/list reverse RPC is answered with the upstream client's roots,
// cached until roots/list_changed invalidates the cache.
func TestDownstreamRootsReverseRPC(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	// No downstream servers needed: the peer handler is exercised directly.
	g, c, _ := startGateway(t, Config{ClientID: "test-client", Resolver: resolver})
	c.answerRoots = true
	c.roots = []mcp.Root{{URI: "file:///workspace", Name: "workspace"}}

	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{
		Roots: &mcp.RootsCapability{ListChanged: true},
	})

	h := g.peerHandler("fake")
	askRoots := func() []mcp.Root {
		t.Helper()
		resp, err := h(context.Background(), mcp.NewRequest(mcp.NewIntID(7), mcp.MethodRootsList, nil))
		if err != nil || resp == nil || resp.Error != nil {
			t.Fatalf("peer roots/list = (%+v, %v)", resp, err)
		}
		var res mcp.ListRootsResult
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			t.Fatalf("decode roots result: %v", err)
		}
		return res.Roots
	}

	roots := askRoots()
	if len(roots) != 1 || roots[0].URI != "file:///workspace" {
		t.Fatalf("roots = %+v, want the client's roots", roots)
	}
	calls := c.rootsCalls.Load() // 1, or 2 if the post-initialized prefetch raced the fetch
	if calls == 0 {
		t.Fatal("client never received a roots/list reverse RPC")
	}

	// Cached: asking again does not re-query the client.
	_ = askRoots()
	if got := c.rootsCalls.Load(); got != calls {
		t.Errorf("rootsCalls after cached ask = %d, want %d", got, calls)
	}

	// roots/list_changed invalidates; the ping round-trip fences the
	// notification through the gateway read loop.
	c.notify(mcp.NotificationRootsListChanged, nil)
	if pong := c.call(mcp.MethodPing, nil); pong.Error != nil {
		t.Fatalf("ping: %v", pong.Error)
	}
	_ = askRoots()
	if got := c.rootsCalls.Load(); got != calls+1 {
		t.Errorf("rootsCalls after invalidation = %d, want %d", got, calls+1)
	}
}

// TestNoRootsCapabilityYieldsEmpty: a client that never declared the roots
// capability is never asked.
func TestNoRootsCapabilityYieldsEmpty(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	g, c, _ := startGateway(t, Config{ClientID: "test-client", Resolver: resolver})
	c.answerRoots = true
	c.roots = []mcp.Root{{URI: "file:///secret"}}
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{}) // no roots capability

	roots, err := g.roots.Roots(context.Background())
	if err != nil || len(roots) != 0 {
		t.Fatalf("Roots() = (%+v, %v), want empty without the capability", roots, err)
	}
	if c.rootsCalls.Load() != 0 {
		t.Error("client without roots capability must never be queried")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
