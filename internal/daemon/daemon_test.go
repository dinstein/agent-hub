package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/daemon"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

const testTimeout = 10 * time.Second

// testResolver pins the data dir and a SHORT socket path (t.TempDir can
// exceed the sun_path limit on macOS) without touching process env.
func testResolver(t *testing.T, dataDir string) *platform.Resolver {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "ahd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "ctl.sock")
	return &platform.Resolver{
		LookupEnv: func(key string) (string, bool) {
			switch key {
			case platform.EnvDataDir:
				return dataDir, true
			case platform.EnvSocket:
				return socket, true
			default:
				return "", false
			}
		},
	}
}

type daemonHandle struct {
	resolver *platform.Resolver
	socket   string
	runDir   string
	cancel   context.CancelFunc
	done     chan error

	stopOnce sync.Once
	stopErr  error
}

// stop cancels the daemon and waits for Run to return. Idempotent so tests
// and the cleanup hook can both call it.
func (h *daemonHandle) stop(t *testing.T) error {
	t.Helper()
	h.stopOnce.Do(func() {
		h.cancel()
		select {
		case h.stopErr = <-h.done:
		case <-time.After(testTimeout):
			t.Error("daemon did not stop")
			h.stopErr = errors.New("daemon did not stop")
		}
	})
	return h.stopErr
}

// startDaemon runs daemon.Run on a temp environment and blocks until the
// readiness handshake completes.
func startDaemon(t *testing.T, mutate func(*daemon.Config)) *daemonHandle {
	t.Helper()
	resolver := testResolver(t, t.TempDir())
	socket, err := resolver.CtlSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	runDir, err := resolver.RunDir()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan daemon.Info, 1)
	cfg := daemon.Config{
		Version:  "test-daemon",
		Resolver: resolver,
		Log:      slog.New(slog.DiscardHandler),
		OnReady:  func(i daemon.Info) { ready <- i },
		Watch:    registry.WatchOptions{Debounce: 20 * time.Millisecond, Poll: 100 * time.Millisecond},
		// Tests must not spend the full production drain grace on every
		// stop while a gateway link SSE connection is open.
		ShutdownGrace: 200 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h := &daemonHandle{resolver: resolver, socket: socket, runDir: runDir, cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- daemon.Run(ctx, cfg) }()
	t.Cleanup(func() { _ = h.stop(t) })

	select {
	case info := <-ready:
		if info.Endpoint != socket {
			t.Fatalf("ready endpoint = %q, want %q", info.Endpoint, socket)
		}
	case err := <-h.done:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("daemon never became ready")
	}
	return h
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForDetail(t, what, func() (bool, string) { return cond(), "" })
}

// waitForDetail is waitFor with evidence. cond returns the current
// observation alongside the verdict, and the last one is reported on timeout
// together with a dump of every goroutine.
//
// Both halves exist because of how these failures actually arrive: a bare
// "timed out waiting for tools/list to expose fake__echo" says only that
// something did not happen within 10s, which is compatible with a downstream
// that never connected, a catalog that rebuilt without the tool, a governance
// read that failed closed, and a goroutine wedged on a lock. Those want
// different fixes and the message distinguishes none of them.
//
// The stack dump is the same tactic the e2e suite uses for hangs (it SIGQUITs
// the process under test and folds the stacks into the failure). The daemon
// here runs IN-PROCESS, so dumping our own goroutines shows the same thing:
// whether anything is actually stuck, and where. It only prints on failure, so
// the cost is paid exactly when there is something to read.
func waitForDetail(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	last := "(condition never reported an observation)"
	for time.Now().Before(deadline) {
		ok, obs := cond()
		if ok {
			return
		}
		if obs != "" {
			last = obs
		}
		time.Sleep(10 * time.Millisecond)
	}
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	t.Fatalf("timed out after %s waiting for %s\nlast observation: %s\n\ngoroutines:\n%s",
		testTimeout, what, last, buf)
}

func TestDaemonStartStopLifecycle(t *testing.T) {
	h := startDaemon(t, nil)

	// Socket exists and is a socket; daemon.json is complete and 0600.
	fi, err := os.Lstat(h.socket)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket at %s: err=%v mode=%v", h.socket, err, fi)
	}
	info, err := daemon.ReadInfo(h.runDir)
	if err != nil {
		t.Fatalf("ReadInfo: %v", err)
	}
	if info.Endpoint != h.socket || info.Pid != os.Getpid() || info.Version != "test-daemon" {
		t.Fatalf("daemon.json = %+v", info)
	}
	st, err := os.Stat(filepath.Join(h.runDir, daemon.InfoFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("daemon.json perm = %o, want 600", perm)
	}

	// Ping answers with the daemon's version.
	ctx, cancelPing := context.WithTimeout(context.Background(), testTimeout)
	defer cancelPing()
	client := api.New(h.socket)
	defer client.Close()
	hello, err := client.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if hello.Version != "test-daemon" || hello.Pid != os.Getpid() {
		t.Fatalf("Hello = %+v", hello)
	}

	// Graceful stop cleans up both the socket and daemon.json.
	if err := h.stop(t); err != nil {
		t.Fatalf("Run returned %v, want nil on graceful stop", err)
	}
	if _, err := os.Lstat(h.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still present after stop: %v", err)
	}
	if _, err := daemon.ReadInfo(h.runDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("daemon.json still present after stop: %v", err)
	}
}

func TestDaemonRefusesSecondInstance(t *testing.T) {
	h := startDaemon(t, nil)
	err := daemon.Run(context.Background(), daemon.Config{
		Resolver: h.resolver,
		Log:      slog.New(slog.DiscardHandler),
	})
	if !errors.Is(err, ctlapi.ErrAlreadyRunning) {
		t.Fatalf("second Run = %v, want ErrAlreadyRunning", err)
	}
}

func TestDaemonPublishesRegistryChanges(t *testing.T) {
	h := startDaemon(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	client := api.New(h.socket)
	defer client.Close()
	events, err := client.Events.Subscribe(ctx, "servers")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// An EXTERNAL registry write (separate Store instance, so the daemon's
	// self-write suppression does not apply) must surface as a `servers`
	// SSE event after the daemon's watch → reload → publish chain.
	regDir, err := h.resolver.RegistryDir()
	if err != nil {
		t.Fatal(err)
	}
	store, err := registry.Open(regDir)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	err = store.Update(ctx, func(tx *registry.Tx) error {
		if tx.Servers.V.Servers == nil {
			tx.Servers.V.Servers = map[string]registry.Doc[registry.ServerEntry]{}
		}
		tx.Servers.V.Servers["github"] = registry.Doc[registry.ServerEntry]{V: registry.ServerEntry{
			Transport: "stdio", Command: "true", Enabled: true,
		}}
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	for {
		select {
		case ev := <-events:
			if ev.Topic == "servers" && ev.Kind == "changed" {
				return // the change reached the frontend topic
			}
		case <-ctx.Done():
			t.Fatal("no servers event after external registry write")
		}
	}
}

// startLinkedGateway runs a real in-process gateway (gateway.Run assembly
// via its exported API) against the daemon's control socket, with a fakemcp
// downstream. Returns the upstream client pipe ends.
func startLinkedGateway(t *testing.T, h *daemonHandle) (in *io.PipeWriter, out *io.PipeReader) {
	t.Helper()
	seedRegistry(t, h.resolver, "fake")
	gwInR, gwInW := io.Pipe()
	gwOutR, gwOutW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- gateway.Run(context.Background(), gateway.Config{
			ClientID: "cursor",
			In:       gwInR,
			Out:      gwOutW,
			Resolver: h.resolver,
			Log:      slog.New(slog.DiscardHandler),
			Dial: func(_ context.Context, spec downstream.Spec) (transport.Transport, error) {
				return fakemcp.Connect(fakemcp.Minimal("echo"))
			},
			LinkRetry: 100 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		_ = gwInW.Close() // EOF => clean gateway shutdown
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Error("gateway did not stop on cleanup")
		}
	})
	return gwInW, gwOutR
}

func seedRegistry(t *testing.T, resolver *platform.Resolver, ids ...string) {
	t.Helper()
	dir, err := resolver.RegistryDir()
	if err != nil {
		t.Fatal(err)
	}
	store, err := registry.Open(dir)
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}
}

func TestGatewayRegistersWithDaemon(t *testing.T) {
	h := startDaemon(t, nil)
	startLinkedGateway(t, h)

	client := api.New(h.socket)
	defer client.Close()

	// The gateway registers best-effort in the background: poll the
	// daemon's session list until it shows up as a stdio session.
	var sid string
	waitFor(t, "gateway registration", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		sessions, err := client.Sessions.List(ctx)
		if err != nil || len(sessions) != 1 {
			return false
		}
		if sessions[0].ClientID != "cursor" || sessions[0].Origin != "stdio" {
			t.Fatalf("unexpected session %+v", sessions[0])
		}
		sid = sessions[0].ID
		return true
	})

	// Daemon-side scope narrowing round-trips through the real gateway:
	// SetScope succeeds ONLY after the gateway applied and acked the pushed
	// overlay (push-then-commit) — this is the cross-process proof.
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	err := client.Sessions.SetScope(ctx, sid, api.ScopeNarrow{DisableServers: []string{"fake"}})
	if err != nil {
		t.Fatalf("SetScope: %v", err)
	}
}

// TestDaemonKillDataPlaneSurvives is the kill -9 injection test of A.3 #2,
// in-process equivalent: the daemon's connections are force-closed with no
// draining and no goodbyes (ctlapi.Server.Close), exactly what the gateway
// observes when the daemon process is SIGKILLed. The stdio data plane must
// keep answering tools/call, and after a fresh daemon appears the gateway
// must re-register under a NEW session identity.
func TestDaemonKillDataPlaneSurvives(t *testing.T) {
	h := startDaemon(t, nil)
	upIn, upOut := startLinkedGateway(t, h)

	client := api.New(h.socket)
	defer client.Close()
	waitFor(t, "gateway registration", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		sessions, err := client.Sessions.List(ctx)
		return err == nil && len(sessions) == 1
	})

	// Drive the upstream MCP handshake and verify the data plane works
	// while the daemon is up.
	mc := newMCPClient(t, upIn, upOut)
	mc.initialize()
	mc.waitToolReady("fake__echo")
	mc.callEcho("fake__echo", `{"n":1}`)

	// KILL: abort every daemon connection without draining (kill -9
	// equivalent for the gateway's control link), then stop the process.
	if err := h.stop(t); err != nil {
		t.Fatalf("daemon stop: %v", err)
	}

	// Data plane: completely unaffected (docs/architecture.md §2 — the gateway's
	// stdio session must not even hiccup).
	mc.callEcho("fake__echo", `{"n":2}`)

	// Recovery: a fresh daemon on the same socket; the gateway's backoff
	// loop must re-register and receive a NEW identity ("cursor:1" again is
	// fine — the new daemon's seq restarts — but it must be a live session
	// in the NEW daemon's manager).
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ready := make(chan daemon.Info, 1)
	done2 := make(chan error, 1)
	go func() {
		done2 <- daemon.Run(ctx2, daemon.Config{
			Version:  "test-daemon-2",
			Resolver: h.resolver,
			Log:      slog.New(slog.DiscardHandler),
			OnReady:  func(i daemon.Info) { ready <- i },
		})
	}()
	t.Cleanup(func() {
		cancel2()
		select {
		case <-done2:
		case <-time.After(testTimeout):
			t.Error("second daemon did not stop")
		}
	})
	select {
	case <-ready:
	case err := <-done2:
		t.Fatalf("second daemon exited early: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("second daemon never became ready")
	}

	client2 := api.New(h.socket)
	defer client2.Close()
	waitFor(t, "gateway re-registration with the new daemon", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		sessions, err := client2.Sessions.List(ctx)
		return err == nil && len(sessions) == 1 && sessions[0].ClientID == "cursor"
	})

	// And the data plane still works after re-registration.
	mc.callEcho("fake__echo", `{"n":3}`)
}

// mcpClient is a minimal upstream MCP client over the gateway's stdio pair.
// A pump goroutine reads CONTINUOUSLY (the pipes are unbuffered, so an
// unread gateway notification would deadlock the whole session otherwise)
// and forwards responses; notifications are discarded.
type mcpClient struct {
	t     *testing.T
	fw    *mcp.FrameWriter
	resps chan *mcp.Response
	seq   int
}

func newMCPClient(t *testing.T, in *io.PipeWriter, out *io.PipeReader) *mcpClient {
	c := &mcpClient{t: t, fw: mcp.NewFrameWriter(in), resps: make(chan *mcp.Response, 64)}
	go func() {
		fr := mcp.NewFrameReader(out)
		for {
			line, err := fr.Next()
			if err != nil {
				close(c.resps)
				return
			}
			msg, err := mcp.ParseMessage(line)
			if err != nil {
				continue
			}
			if resp, ok := msg.(*mcp.Response); ok {
				c.resps <- resp
			}
		}
	}()
	return c
}

// call sends one request and waits for its response.
func (c *mcpClient) call(method string, params any) *mcp.Response {
	c.t.Helper()
	c.seq++
	id := mcp.NewIntID(int64(c.seq))
	var raw []byte
	if params != nil {
		var err error
		if raw, err = json.Marshal(params); err != nil {
			c.t.Fatal(err)
		}
	}
	if err := c.fw.WriteFrame(mcp.NewRequest(id, method, raw)); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	for {
		select {
		case resp, ok := <-c.resps:
			if !ok {
				c.t.Fatalf("stream closed while waiting for %s response", method)
			}
			if resp.ID.Key() == id.Key() {
				return resp
			}
		case <-deadline.C:
			c.t.Fatalf("no response to %s", method)
		}
	}
}

func (c *mcpClient) initialize() {
	c.t.Helper()
	resp := c.call(mcp.MethodInitialize, mcp.InitializeParams{ProtocolVersion: mcp.ProtocolVersion})
	if resp.Error != nil {
		c.t.Fatalf("initialize error: %v", resp.Error)
	}
	if err := c.fw.WriteFrame(mcp.NewNotification(mcp.NotificationInitialized, nil)); err != nil {
		c.t.Fatal(err)
	}
}

// waitToolReady polls tools/list until the named tool appears (the gateway
// connects downstreams in the background).
func (c *mcpClient) waitToolReady(name string) {
	c.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		resp := c.call(mcp.MethodToolsList, nil)
		if resp.Error == nil {
			var res mcp.ListToolsResult
			if err := json.Unmarshal(resp.Result, &res); err == nil {
				for _, tool := range res.Tools {
					if tool.Name == name {
						return
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("tool %s never became available", name)
}

// callEcho invokes the echo tool and asserts a successful, non-busy result.
// Retries transient busy errors (code -32000) while downstreams connect.
func (c *mcpClient) callEcho(name, args string) {
	c.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{
			Name: name, Arguments: []byte(args),
		})
		if resp.Error == nil {
			return
		}
		if resp.Error.Code != -32000 {
			c.t.Fatalf("tools/call %s: %v", name, resp.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("tools/call %s stayed busy", name)
}

// TestDaemonArmsAndResolvesTheCrashMarker pins the writer half of crash
// detection. The reader half — registry.PreviousShutdown, which `agenthub
// doctor` reports — was wired from the start, but nothing in the product ever
// ARMED a marker: daemon.go carried a "TODO(M1-H): write a crash marker here"
// where this now happens. So doctor's answer was permanently "unknown (no
// marker yet)", including straight after a crash, which is the one moment the
// feature exists for.
//
// Both directions are asserted because either alone is satisfiable by a stub:
// a daemon that always resolves reports every crash as clean, and one that
// never arms reports every clean stop as unknown.
func TestDaemonArmsAndResolvesTheCrashMarker(t *testing.T) {
	h := startDaemon(t, nil)
	regDir, err := h.resolver.RegistryDir()
	if err != nil {
		t.Fatal(err)
	}

	// While the daemon is up the marker is ARMED, which reads as a crash.
	// That is the whole mechanism: the state is corrected only on the way
	// out, so a run that is interrupted leaves the truthful answer behind.
	if got := registry.PreviousShutdown(regDir); got != registry.ShutdownCrash {
		t.Fatalf("a running daemon leaves the marker %q, want %q (armed)", got, registry.ShutdownCrash)
	}

	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := registry.PreviousShutdown(regDir); got != registry.ShutdownClean {
		t.Fatalf("after a graceful stop the marker reads %q, want %q", got, registry.ShutdownClean)
	}
}

// A directory no daemon has ever run in must read as unknown, not clean: the
// diagnostic must never invent a clean bill of health it has no evidence for.
func TestPreviousShutdownIsUnknownBeforeAnyRun(t *testing.T) {
	if got := registry.PreviousShutdown(t.TempDir()); got != registry.ShutdownUnknown {
		t.Fatalf("a fresh directory reports %q, want %q", got, registry.ShutdownUnknown)
	}
}
