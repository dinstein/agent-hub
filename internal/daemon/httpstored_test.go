package daemon_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/daemon"
	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// Where the data plane's opt-in comes from, now that the desktop application
// is what starts a hub: the application types no flags, so the answer has to
// be readable from the registry — and taking half of it from there and half
// from a command line would assemble a listener nobody asked for.

// seedHTTPFace writes the stored data-plane opt-in before the daemon starts.
func seedHTTPFace(t *testing.T, resolver *platform.Resolver, face registry.HTTPFace) {
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
		tx.Governance.V.HTTP = &registry.Doc[registry.HTTPFace]{V: face}
		return nil
	})
	if err != nil {
		t.Fatalf("seeding the stored http face: %v", err)
	}
}

// daemonRun is one running daemon. The outcome is LATCHED rather than left on
// a channel, because both a test and the cleanup hook need to read it: a
// channel that the test has already drained would leave the hook waiting out
// its whole timeout and reporting a hang that had already finished.
type daemonRun struct {
	mu    sync.Mutex
	ended bool
	err   error
}

func (r *daemonRun) finish(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended, r.err = true, err
}

// outcome reports how the run ended, waiting up to within for it to end at
// all. ended=false means it is still running.
func (r *daemonRun) outcome(within time.Duration) (err error, ended bool) {
	deadline := time.Now().Add(within)
	for {
		r.mu.Lock()
		ended, err = r.ended, r.err
		r.mu.Unlock()
		if ended || time.Now().After(deadline) {
			return err, ended
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runDaemonWith runs one daemon on resolver until the test ends. The HTTP
// fields of cfg are left to the caller: what these tests are about is
// precisely which of them are set.
func runDaemonWith(t *testing.T, resolver *platform.Resolver, mutate func(*daemon.Config)) *daemonRun {
	t.Helper()
	cfg := daemon.Config{
		Version:       "test-daemon",
		Resolver:      resolver,
		Log:           slog.New(slog.DiscardHandler),
		Watch:         registry.WatchOptions{Debounce: 20 * time.Millisecond, Poll: 100 * time.Millisecond},
		ShutdownGrace: 200 * time.Millisecond,
		Dial:          scriptedDial(map[string]*fakemcp.Script{}),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &daemonRun{}
	go func() { run.finish(daemon.Run(ctx, cfg)) }()
	t.Cleanup(func() {
		cancel()
		if _, ended := run.outcome(testTimeout); !ended {
			t.Error("daemon did not stop")
		}
	})
	return run
}

func TestStoredHTTPAddressBringsUpTheDataPlane(t *testing.T) {
	resolver := testResolver(t, t.TempDir())
	seedHTTPFace(t, resolver, registry.HTTPFace{Addr: "127.0.0.1:0", InsecureLoopback: true})

	bound := make(chan []string, 1)
	ready := make(chan daemon.Info, 1)
	run := runDaemonWith(t, resolver, func(cfg *daemon.Config) {
		// No HTTP fields at all: exactly what the application passes.
		cfg.OnReady = func(i daemon.Info) { ready <- i }
		cfg.OnHTTPReady = func(a []string) { bound <- a }
	})

	select {
	case <-ready:
	case <-time.After(testTimeout):
		if err, ended := run.outcome(0); ended {
			t.Fatalf("daemon exited before ready: %v", err)
		}
		t.Fatal("daemon never became ready")
	}

	var addrs []string
	select {
	case addrs = <-bound:
	case <-time.After(testTimeout):
		t.Fatal("the stored address brought up no listener; the HTTP face would be unreachable for anyone who does not type flags")
	}
	if len(addrs) == 0 {
		t.Fatal("the data plane reported no bound address")
	}
	conn, err := net.DialTimeout("tcp", addrs[0], 2*time.Second)
	if err != nil {
		t.Fatalf("nothing is accepting on the address the daemon reported (%s): %v", addrs[0], err)
	}
	_ = conn.Close()
}

// The stored source has to reach the AUTHENTICATOR, not only the bind
// decision — the two used to read different fields.
//
// AuthorizeBind was given the resolved face while httpbridge.Authenticator was
// built from the command-line field alone. Both sources agree whenever a flag
// was typed, because any of the three sends resolveHTTPFace down that branch,
// so nothing that passes flags could see it. From the stored source it split:
// `http.insecureLoopback true` authorized a credential-less bind and then
// served it with an authenticator that refuses every unauthenticated caller,
// so the endpoint came up, logged that it was serving, and answered 401 to
// everybody — with no credential configured anywhere to answer differently.
//
// The previous test on this path connects a TCP socket and stops there, which
// is exactly the evidence the divergence survives. This one sends a request.
func TestStoredInsecureLoopbackReachesTheAuthenticator(t *testing.T) {
	d := startHTTPDaemon(t, func(cfg *daemon.Config) {
		// No HTTP fields at all — what the desktop application passes — so
		// every one of the three comes from the store.
		cfg.HTTPAddr = ""
		seedHTTPFace(t, cfg.Resolver, registry.HTTPFace{Addr: "127.0.0.1:0", InsecureLoopback: true})
	})

	// Unauthenticated, from loopback: precisely what the stored escape hatch
	// says to accept. No token is minted anywhere in this test, so a 401 here
	// cannot be answered by configuring one.
	//
	// initialize rather than ping, because it is the one request that needs no
	// session of its own — a 200 here therefore says the call reached the
	// dispatcher, not merely that it got past the authenticator.
	params, err := json.Marshal(mcp.InitializeParams{
		ProtocolVersion: mcp.ProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "stored-face-test", Version: "0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, hdr, body := d.mcpPost(t, "", "",
		mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize, params))
	if status == http.StatusUnauthorized {
		t.Fatalf("an unauthenticated loopback caller got 401 while http.insecureLoopback is stored true "+
			"(body %s): the bind was authorized by the resolved face and then served by an "+
			"authenticator built from the command line", body)
	}
	if status != http.StatusOK {
		t.Fatalf("initialize over the stored insecure-loopback face: HTTP %d (%s)", status, body)
	}
	if hdr.Get(httpbridge.SessionHeader) == "" {
		t.Fatalf("initialize bound no session over the stored face (body %s)", body)
	}
}

// The command line replaces the stored set rather than merging with it. An
// operator who names an address expecting to be refused must not be let
// through by a confirmation somebody stored months ago for a different one.
func TestFlagsReplaceTheStoredHTTPFaceAsASet(t *testing.T) {
	resolver := testResolver(t, t.TempDir())
	seedHTTPFace(t, resolver, registry.HTTPFace{Addr: "127.0.0.1:0", AllowRemote: true, InsecureLoopback: true})

	run := runDaemonWith(t, resolver, func(cfg *daemon.Config) {
		// Only an address, and a non-loopback one. The stored AllowRemote
		// must not reach it.
		cfg.HTTPAddr = "0.0.0.0:0"
	})

	err, ended := run.outcome(testTimeout)
	if !ended {
		t.Fatal("the daemon neither refused the bind nor stopped")
	}
	if err == nil {
		t.Fatal("a non-loopback address was served without a confirmation of its own")
	}
}
