package gateway

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/session"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// ctlHarness is a minimal in-process daemon control plane: real
// ctlapi.Server + session manager on a real UDS.
type ctlHarness struct {
	srv *ctlapi.Server
	mgr *session.MemoryManager
	bus *event.Bus
}

// startCtlServer boots a control-plane server on socket. Returns a stop
// function that FORCE-closes every connection (ctlapi.Server.Close) — the
// in-process kill -9 equivalent (A.3 #2).
func startCtlServer(t *testing.T, socket string, mutate ...func(*ctlapi.Options)) (*ctlHarness, func()) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "ctl-registry"))
	if err != nil {
		t.Fatal(err)
	}
	bus := event.NewBus()
	mgr := session.NewMemoryManager(session.Options{Bus: bus})
	opts := ctlapi.Options{
		Version:  "test-ctl",
		Registry: reg,
		Sessions: mgr,
		Bus:      bus,
		Logger:   slog.New(slog.DiscardHandler),
		// No keep-alives: frames only.
		KeepAlive: -1,
	}
	for _, m := range mutate {
		m(&opts)
	}
	srv, err := ctlapi.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ctlapi.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = srv.Close()
	}
	t.Cleanup(stop)
	return &ctlHarness{srv: srv, mgr: mgr, bus: bus}, stop
}

// linkResolver pins the data dir AND the control socket path.
func linkResolver(t *testing.T, dataDir string) (*platform.Resolver, string) {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "ahgw")
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
	}, socket
}

func waitCond(t *testing.T, what string, cond func() bool) {
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

// TestCtlLinkRegisters: the gateway registers best-effort and the daemon
// ends up holding a live session for it.
func TestCtlLinkRegisters(t *testing.T) {
	t.Parallel()
	resolver, socket := linkResolver(t, t.TempDir())
	h, _ := startCtlServer(t, socket)

	seedRegistry(t, resolver, "fake")
	g, c, _ := startGateway(t, Config{
		ClientID: "cursor",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
		// Fast retry so a lost race with server startup cannot stall the test.
		LinkRetry: 50 * time.Millisecond,
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})

	waitCond(t, "gateway registration", func() bool { return g.ctl.Session() != "" })
	sid := session.SessionID(g.ctl.Session())
	if _, ok := h.mgr.Get(sid); !ok {
		t.Fatalf("session %q not present in the daemon manager", sid)
	}

}

// TestCtlLinkDaemonKillInjection is the gateway side of the kill -9
// injection test (A.3 #2, in-process equivalent: force-close the daemon's
// listener and every connection with no draining):
//
//  1. gateway registered;
//  2. daemon killed → stdio data plane keeps answering tools/call;
//  3. the session id is dropped;
//  4. a fresh daemon appears on the same socket → the gateway's backoff
//     loop re-registers under a fresh identity.
func TestCtlLinkDaemonKillInjection(t *testing.T) {
	t.Parallel()
	resolver, socket := linkResolver(t, t.TempDir())
	_, kill := startCtlServer(t, socket)

	seedRegistry(t, resolver, "fake")
	g, c, _ := startGateway(t, Config{
		ClientID:  "cursor",
		Resolver:  resolver,
		Dial:      scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
		LinkRetry: 50 * time.Millisecond,
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	c.waitNotification(mcp.NotificationToolsListChanged)

	waitCond(t, "gateway registration", func() bool { return g.ctl.Session() != "" })
	// Data plane works while the daemon is up.
	callEchoOK(t, c)

	// KILL -9 (in-process equivalent): connections abort mid-stream, the
	// socket file stays behind as stale debris — exactly the crash shape.
	kill()

	// 2. The stdio data plane must not even hiccup.
	callEchoOK(t, c)

	// 3. The daemon-side session died with it: the gateway drops the id.
	waitCond(t, "session dropped after daemon death", func() bool {
		return g.ctl.Session() == ""
	})

	// 4. Fresh daemon on the same socket (Listen removes the stale socket
	// after probing it dead); the gateway re-registers with a NEW identity.
	h2, _ := startCtlServer(t, socket)
	waitCond(t, "re-registration with the new daemon", func() bool {
		sid2 := g.ctl.Session()
		if sid2 == "" {
			return false
		}
		_, ok := h2.mgr.Get(session.SessionID(sid2))
		return ok
	})

	// Still answering after re-registration.
	callEchoOK(t, c)
}

// TestCtlLinkAbsentDaemonIsHarmless: no daemon at all — the gateway serves
// normally and simply keeps retrying in the background (the M0 behavioral
// baseline that must never regress).
func TestCtlLinkAbsentDaemonIsHarmless(t *testing.T) {
	t.Parallel()
	resolver, _ := linkResolver(t, t.TempDir())
	seedRegistry(t, resolver, "fake")
	g, c, _ := startGateway(t, Config{
		ClientID:  "cursor",
		Resolver:  resolver,
		Dial:      scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
		LinkRetry: 20 * time.Millisecond,
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	c.waitNotification(mcp.NotificationToolsListChanged)
	callEchoOK(t, c)
	if g.ctl.Session() != "" {
		t.Fatal("gateway claims a session with no daemon present")
	}
}

// callEchoOK asserts one successful fake__echo tools/call round trip,
// retrying the transient "still connecting" busy error.
func callEchoOK(t *testing.T, c *testClient) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{
			Name: "fake__echo", Arguments: []byte(`{"ok":true}`),
		})
		if resp.Error == nil {
			return
		}
		if resp.Error.Code != codeRetryBusy {
			t.Fatalf("tools/call: %v", resp.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("tools/call stayed busy")
}
