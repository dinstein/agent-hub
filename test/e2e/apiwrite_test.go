package e2e_test

import (
	"context"
	"errors"
	"net/http"
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

// TestAPIWriteRefusesAStaleGeneration is why this route carries a
// precondition at all.
//
// The registry has five writers — N gateways, the daemon, the CLI, the GUI and
// any third party embedding `api` — and the file lock keeps a write from
// TEARING, not from overwriting somebody else's change. A window holding a
// minutes-old read that wrote last-writer-wins would silently discard edits
// made elsewhere, and silently is the whole problem: nothing would report it,
// and the user would find their change undone with nothing saying by what.
//
// The concurrent writer here is the CLI, which is the realistic shape rather
// than a convenient one: it writes the files directly and sends no
// precondition of its own, so it is exactly the writer a window cannot see
// coming.
func TestAPIWriteRefusesAStaleGeneration(t *testing.T) {
	dataDir, socket, env := sandbox(t)
	runAgenthubEnv(t, env, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")

	c := startDaemonForAPI(t, dataDir, socket, env)
	ctx, cancel := apiCtx(t)
	defer cancel()

	stale, err := c.Servers.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("api Servers.Get: %v", err)
	}

	// Somebody else edits. The view the api client holds is now old.
	runAgenthubEnv(t, env, "", "server", "add", "beta", "--cmd", fakemcpBin, "--json")

	_, err = c.Servers.SetEnabled(ctx, "alpha", true, stale.Generation)
	if err == nil {
		t.Fatal("a write carrying a superseded generation was applied")
	}
	if !api.IsConflict(err) {
		t.Fatalf("the refusal is not identifiable as a stale view: %v", err)
	}
	conflict, ok := api.AsConflict(err)
	if !ok {
		t.Fatalf("IsConflict said yes but AsConflict said no: %v", err)
	}
	// The answer has to carry what to re-read at. Without it the intended
	// recovery — re-read, re-apply the user's intent, retry — needs a second
	// round trip that can itself go stale.
	if conflict.CurrentGeneration <= stale.Generation {
		t.Fatalf("the conflict reports generation %d, which is not ahead of the stale %d",
			conflict.CurrentGeneration, stale.Generation)
	}

	// NOTHING was written. This is the assertion the whole mechanism exists
	// for: a refusal that had half-applied would be worse than last-writer-
	// wins, because it reports failure and leaves a change behind.
	if row := serverByID(t, dataDir, "alpha"); row.Enabled {
		t.Fatalf("the refused write took effect anyway: %+v", row)
	}

	// The recovery is to retry at the generation the conflict REPORTED, and
	// that is what the field is for: it comes from the write path, which
	// compares under the registry lock, so it is the authoritative value.
	//
	// Re-reading first is the documented shape and is what a wholesale Update
	// needs, but it is NOT interchangeable here. The daemon serves reads from a
	// snapshot its registry watcher refreshes asynchronously, so for a couple of
	// hundred milliseconds after an outside writer a re-read still answers the
	// OLD generation and a retry at it earns a second conflict. A frontend that
	// re-reads must therefore back off and retry rather than treat one repeat as
	// a failure — recorded in docs/modules/controlplane.md, because it is a
	// property of the route and not of this test.
	if _, err = c.Servers.SetEnabled(ctx, "alpha", true, conflict.CurrentGeneration); err != nil {
		t.Fatalf("the retry at the generation the conflict reported still failed: %v", err)
	}
	if row := serverByID(t, dataDir, "alpha"); !row.Enabled {
		t.Fatalf("the retry reported success without writing: %+v", row)
	}
}

// TestAPIDuplicateNameIsAConflictButNotAStaleView pins the narrowness of the
// stale-view test, which is the half that is easy to get wrong and impossible
// to notice.
//
// The daemon answers HTTP 409 for more than one reason: a superseded
// generation, and a name already taken. Only the first is fixed by re-reading.
// A client that promoted every 409 to "your view was stale" would send a
// frontend into a retry loop that can never succeed — it would re-read, find
// the name still taken, and try again forever — so `asConflict` requires the
// status AND the E_STALE_PRECONDITION code, and this is what proves the second
// half of that condition is load-bearing against a real daemon.
func TestAPIDuplicateNameIsAConflictButNotAStaleView(t *testing.T) {
	dataDir, socket, env := sandbox(t)
	runAgenthubEnv(t, env, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")

	c := startDaemonForAPI(t, dataDir, socket, env)
	ctx, cancel := apiCtx(t)
	defer cancel()

	// Generation 0 spells "do not check", so nothing here is stale by
	// construction: whatever 409 comes back is about the NAME.
	_, err := c.Servers.Create(ctx, api.ServerSpec{
		ID:    "alpha",
		Entry: api.ServerEntry{Command: fakemcpBin},
	}, 0)
	if err == nil {
		t.Fatal("Create silently replaced an existing server")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("the refusal is not a control-plane error: %v", err)
	}
	if apiErr.Status != http.StatusConflict {
		t.Fatalf("a duplicate name answered HTTP %d, want 409: %v", apiErr.Status, err)
	}
	if api.IsConflict(err) {
		t.Fatalf("a duplicate name was reported as a stale view; re-reading fixes nothing: %v", err)
	}

	// And the existing definition is untouched — a refused create must not be
	// a partial overwrite.
	if row := serverByID(t, dataDir, "alpha"); row.Command != fakemcpBin || row.Enabled {
		t.Fatalf("the refused create changed the stored entry: %+v", row)
	}
}
