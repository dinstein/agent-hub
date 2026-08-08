package e2e_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Two rules meet at a registry edit, and they point in opposite directions.
//
// A DEFINITION change must rebuild the connection. specEqual decides that, and
// its comment names the danger in as many words: "a field that is compared
// nowhere is a field whose edit leaves the old connection running under the
// old definition — for the container block that would mean an operator
// changing the image, the mounts or the network and getting the previous
// isolation until the next restart."
//
// A VISIBILITY change must NOT. docs/architecture.md §7 invariant 2: the
// router is never rebuilt for a scope change, because visibility is a
// query-time projection. Bouncing a downstream because one client's tool list
// was narrowed would restart a process every other client is using.
//
// Neither had an end-to-end case, and the suite could not have told them
// apart if it had one: a tool appearing or disappearing looks identical
// whether or not the process behind it was restarted. These cases count
// spawns, which is the only thing that distinguishes "the connection was
// rebuilt" from "the answer changed".

// spawnCounter writes a command that records every start and then becomes the
// fake server, passing its arguments through so the caller still chooses the
// script. Returns (commandPath, counterPath).
func spawnCounter(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "spawns.log")
	cmd := filepath.Join(dir, "counted.sh")
	body := "#!/bin/sh\necho started >> " + counter + "\nexec " + fakemcpBin + " \"$@\"\n"
	if err := os.WriteFile(cmd, []byte(body), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	return cmd, counter
}

// spawnCount is how many times the downstream process has been started.
func spawnCount(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read spawn counter: %v", err)
	}
	return strings.Count(strings.TrimSpace(string(data)), "\n") + 1
}

// waitSpawns polls until the process has been started want times.
func waitSpawns(t *testing.T, c *gatewayClient, counter string, want int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		got := spawnCount(t, counter)
		if got >= want {
			if got > want {
				c.fatalf("the downstream was started %d times, want %d", got, want)
			}
			return
		}
		if !time.Now().Before(deadline) {
			c.fatalf("the downstream was started %d times within %s, want %d", got, budget, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestRedefiningALiveServerRebuildsItsConnection drives the specEqual half:
// the entry's command line changes under a running gateway, and the
// connection must be torn down and remade rather than kept.
//
// The old tool going away is what makes it a rebuild rather than an addition.
// A gateway that kept the old process and merely re-read the registry would
// serve both names, which is the shape of the bug specEqual exists to prevent.
func TestRedefiningALiveServerRebuildsItsConnection(t *testing.T) {
	dataDir := t.TempDir()
	cmd, counter := spawnCounter(t)
	before := filepath.Join(t.TempDir(), "before.json")
	writeScript(t, before, "tool_before")
	after := filepath.Join(t.TempDir(), "after.json")
	writeScript(t, after, "tool_after")

	runAgenthub(t, dataDir, "", "server", "add", "x", "--cmd", cmd, "--args", before, "--json")
	enableServer(t, dataDir, "x")

	c := startGateway(t, dataDir, "redefineclient")
	c.initialize()
	c.waitForTool("x__tool_before", 30*time.Second)
	waitSpawns(t, c, counter, 1, 15*time.Second)

	// Redefine in place: same id, different arguments.
	writeServersJSON(t, dataDir, map[string]any{
		"servers": map[string]any{
			"x": map[string]any{
				"enabled": true, "source": "manual", "transport": "stdio",
				"command": cmd, "args": []string{after},
			},
		},
	})

	waitTools(t, c, 30*time.Second, "the redefined server's tools", func(names []string) bool {
		return hasTool(names, "x__tool_after") && !hasTool(names, "x__tool_before")
	})
	// A second process: the old one could not have produced the new catalog.
	waitSpawns(t, c, counter, 2, 15*time.Second)
	c.close()
}

// TestAnEnvEditRebuildsTheConnectionToo covers a different specEqual field,
// and the one where getting it wrong is worst.
//
// Env is where a downstream's credential lives. An edit that did not rebuild
// would leave the old process running with the OLD secret — so rotating a key
// would appear to work, the registry would show the new value, and every call
// would keep going out under the old one until something restarted the
// gateway.
func TestAnEnvEditRebuildsTheConnectionToo(t *testing.T) {
	dataDir := t.TempDir()
	dir := t.TempDir()
	dump := filepath.Join(dir, "child-env.txt")
	counter := filepath.Join(dir, "spawns.log")
	cmd := filepath.Join(dir, "counted.sh")
	// Records both the start and the environment it started with; the dump is
	// overwritten each time, so it always describes the CURRENT process.
	body := "#!/bin/sh\necho started >> " + counter + "\nprintenv > " + dump +
		"\nexec " + fakemcpBin + "\n"
	if err := os.WriteFile(cmd, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	runAgenthub(t, dataDir, "", "server", "add", "x", "--cmd", cmd,
		"--env", "ROTATING_KEY=first-value", "--json")
	enableServer(t, dataDir, "x")

	c := startGateway(t, dataDir, "envrefreshclient")
	c.initialize()
	c.waitForTool("x__echo", 30*time.Second)
	if got := childEnv(t, dump, 15*time.Second); !strings.Contains(got, "ROTATING_KEY=first-value") {
		c.fatalf("the first process did not receive the first value")
	}
	waitSpawns(t, c, counter, 1, 15*time.Second)

	writeServersJSON(t, dataDir, map[string]any{
		"servers": map[string]any{
			"x": map[string]any{
				"enabled": true, "source": "manual", "transport": "stdio",
				"command": cmd, "env": map[string]any{"ROTATING_KEY": "second-value"},
			},
		},
	})

	waitSpawns(t, c, counter, 2, 30*time.Second)
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(childEnv(t, dump, 15*time.Second), "ROTATING_KEY=second-value") {
		if !time.Now().Before(deadline) {
			c.fatalf("the rebuilt process is still running with the old value")
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.close()
}

// TestNarrowingToolsDoesNotRestartTheDownstream is the opposite direction,
// and it is the half that a "does the edit take effect" test would happily
// break.
//
// Narrowing what one client may see is a scope change, and scope is applied
// as a projection at query time. Restarting the downstream for it would
// interrupt every other client of that server to change what one of them is
// shown — and the process is shared, so the blast radius is not the client
// that was narrowed.
func TestNarrowingToolsDoesNotRestartTheDownstream(t *testing.T) {
	dataDir := t.TempDir()
	cmd, counter := spawnCounter(t)
	script := filepath.Join(t.TempDir(), "two.json")
	writeScript(t, script, "read_thing", "write_thing")

	runAgenthub(t, dataDir, "", "server", "add", "x", "--cmd", cmd, "--args", script, "--json")
	enableServer(t, dataDir, "x")

	c := startGateway(t, dataDir, "projectionclient")
	c.initialize()
	c.waitForTool("x__read_thing", 30*time.Second)
	c.waitForTool("x__write_thing", 30*time.Second)
	waitSpawns(t, c, counter, 1, 15*time.Second)

	runAgenthub(t, dataDir, "", "server", "tool", "allow", "x", "--only", "read_thing")
	waitTools(t, c, 30*time.Second, "write_thing withdrawn", func(names []string) bool {
		return hasTool(names, "x__read_thing") && !hasTool(names, "x__write_thing")
	})

	// The surface moved and the process did not. Checked after the surface
	// has demonstrably changed, so this is "no restart happened" rather than
	// "nothing has happened yet".
	if got := spawnCount(t, counter); got != 1 {
		c.fatalf("narrowing the tool list restarted the downstream (%d spawns); "+
			"visibility is a query-time projection and must rebuild nothing", got)
	}
	// And the surviving tool still works, so the connection was not merely
	// left alive but left usable.
	if got := c.textContent(c.callTool("x__read_thing",
		map[string]any{"marker": "still-connected"}, 30*time.Second)); !strings.Contains(got, "still-connected") {
		c.fatalf("the untouched connection stopped serving: %q", got)
	}
	c.close()
}

// TestRemovingALiveServerWithdrawsItsTools covers `server rm` against a
// running gateway. serverlifecycle_test.go drives the verb offline and
// serverlive_test.go covers `disable`, which is a different edit: disable
// leaves the entry, rm takes it away entirely, and the loader's two paths
// have no reason to agree unless something checks.
func TestRemovingALiveServerWithdrawsItsTools(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "going", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "going")
	runAgenthub(t, dataDir, "", "server", "add", "staying", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "staying")

	c := startGateway(t, dataDir, "rmclient")
	c.initialize()
	c.waitForTool("going__echo", 30*time.Second)
	c.waitForTool("staying__echo", 30*time.Second)

	runAgenthub(t, dataDir, "", "server", "rm", "going", "--json")
	waitTools(t, c, 30*time.Second, "the removed server's tools are gone", func(names []string) bool {
		return !hasTool(names, "going__echo")
	})
	if names := c.listTools(30 * time.Second); !slices.Contains(names, "staying__echo") {
		c.fatalf("removing one server took its neighbour with it: %v", names)
	}
	// The neighbour is still callable, not merely still listed.
	if got := c.textContent(c.callTool("staying__echo",
		map[string]any{"marker": "unaffected"}, 30*time.Second)); !strings.Contains(got, "unaffected") {
		c.fatalf("the surviving server stopped serving: %q", got)
	}
	c.close()
}
