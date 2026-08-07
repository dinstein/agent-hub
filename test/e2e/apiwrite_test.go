package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
)

// The registry has two write routes and they are not variations of one
// another. The CLI writes the FILES, under the registry's cross-process lock,
// daemon or no daemon; holding no long-lived view it sends no precondition.
// The GUI writes through the DAEMON — api → ctlapi → the same internal/confops
// — and because its window may hold a minutes-old read, that route alone
// carries the optimistic-concurrency precondition (docs/architecture.md §10).
//
// Everything about the second route was tested in pieces and never assembled.
// `api`'s own tests dial a `newTestDaemon(t, http.Handler)` — a fake that
// answers canned JSON — and internal/ctlapi's tests run in process. So the api
// client had never spoken to a real daemon, and the daemon had never answered
// the real client. What sits between them is a Unix socket, an envelope
// encoding and a generation counter that has to mean the same thing on both
// sides, which is the class of disagreement no single-process test can have.
//
// test/e2e is where that meets, and it may import `api` for the same reason
// the GUI may: `api` is the published surface and imports nothing under
// internal/ (AGENTS.md hard constraint 1). The client here is the one a
// third-party embedder would get.

// startDaemonForAPI starts a headless daemon on the sandbox socket and
// returns an api.Client dialling it. `daemon start` returns only after the
// readiness handshake is written, so the socket is up when this does.
func startDaemonForAPI(t *testing.T, dataDir, socket string, env []string) *api.Client {
	t.Helper()
	runAgenthubEnv(t, env, "", "daemon", "start", "--headless")
	h := &daemonEnv{dataDir: dataDir, socket: socket, env: env}
	t.Cleanup(func() { h.killDaemon(t) })
	c := api.New(socket)
	t.Cleanup(c.Close)
	return c
}

// apiCtx bounds one control-plane call. api.Client sets no client-wide
// timeout on purpose — its SSE subscriptions are long-lived — so every call
// site owns its deadline, and a test without one would hang rather than fail.
func apiCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// TestAPIWriteReachesTheSameRegistryALiveGatewayWatches drives the GUI's route
// end to end: a real api.Client over the real control socket, a real daemon
// behind it, and a running client that must see the result.
//
// The gateway is the assertion. A write that returns a bumped generation has
// proven only that the daemon answered; what a desktop user is owed is that
// flipping a switch in a window reaches the agent that is already connected.
// Those are separate mechanisms — the daemon's confops write, then the
// registry watch every gateway holds — and the CLI's own hot-reload coverage
// says nothing about the route that goes through the daemon.
func TestAPIWriteReachesTheSameRegistryALiveGatewayWatches(t *testing.T) {
	dataDir, socket, env := sandbox(t)

	// The operator's route lays the ground: a server that exists and is OFF.
	// Using the CLI here is deliberate — the two routes write one registry,
	// and a test that only ever used one could not tell that apart from two.
	runAgenthubEnv(t, env, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")

	c := startDaemonForAPI(t, dataDir, socket, env)
	ctx, cancel := apiCtx(t)
	defer cancel()

	gw := startGatewayEnv(t, env, "apiclient")
	gw.initialize()
	if hasTool(gw.listTools(30*time.Second), "alpha__echo") {
		t.Fatal("a disabled server was exposed before anything enabled it")
	}

	// Read the entry and the generation it was read at, which is the pair the
	// following write sends back. ServerDetail carries both together for
	// exactly this reason.
	before, err := c.Servers.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("api Servers.Get: %v", err)
	}
	if before.Entry.Enabled {
		t.Fatal("the fixture server is already enabled")
	}
	if before.Generation == 0 {
		t.Fatal("the daemon reported generation 0; there is no precondition to send")
	}

	res, err := c.Servers.SetEnabled(ctx, "alpha", true, before.Generation)
	if err != nil {
		t.Fatalf("api Servers.SetEnabled: %v", err)
	}
	if !res.Changed {
		t.Fatal("the write reports no change while flipping a disabled server on")
	}
	if res.Generation <= before.Generation {
		t.Fatalf("generation did not advance: %d after a write at %d", res.Generation, before.Generation)
	}

	// The claim: the daemon's write and the CLI's write land in one registry,
	// and a session that was already open follows it.
	waitTools(t, gw, 45*time.Second, "alpha__echo exposed after an api write", func(names []string) bool {
		return hasTool(names, "alpha__echo")
	})

	// It is a working tool, not just a name: the entry the daemon wrote has to
	// be one the gateway can actually spawn.
	if got := gw.textContent(gw.callTool("alpha__echo", map[string]any{"marker": "via-the-api"}, 45*time.Second)); got == "" {
		t.Fatal("the tool enabled through the api answered nothing")
	}

	// And the CLI reads it back — from the files, with no daemon in the path.
	// A daemon that answered its own reads consistently while writing
	// somewhere else would satisfy everything above.
	if row := serverByID(t, dataDir, "alpha"); !row.Enabled {
		t.Fatalf("the CLI does not see the daemon's write: %+v", row)
	}
	gw.close()
}
