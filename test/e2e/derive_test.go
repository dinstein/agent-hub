package e2e_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Derived instances decide WHICH PROCESS an allowed call runs on. They are a
// connection-plane feature and nothing else: a derived instance shares its
// base server's id, so exposed names, scope keys and audit records are
// identical, and deriving never adds, hides or renames a tool.
//
// That is exactly why the feature had no end-to-end coverage — there is
// nothing to see. Every observation a client can make is designed to be the
// same whether or not deriving happened, so a test written from the outside
// has no signal at all unless it counts PROCESSES. These cases do.
//
// `derive` has no CLI flag, so each fixture writes servers.json directly,
// which is also the path an operator takes for it today.
//
// The counting is what makes each case falsifiable, and it is worth being
// precise about the arithmetic. The base connection is dialled at startup by
// connectAll, so a server is always started once before any call. A derived
// instance is dialled on FIRST USE, so it appears on the first tools/call and
// not before. One spawn after a call therefore means the call ran on the base;
// two means it ran somewhere of its own.

// deriveFixture registers one server with the given derive policy, using a
// command that counts its own starts. Returns (dataDir, counterPath).
func deriveFixture(t *testing.T, policy string) (string, string) {
	t.Helper()
	dataDir := t.TempDir()
	cmd, counter := spawnCounter(t)
	script := filepath.Join(t.TempDir(), "one.json")
	writeScript(t, script, "echo")

	entry := map[string]any{
		"enabled": true, "source": "manual", "transport": "stdio",
		"command": cmd, "args": []string{script},
	}
	if policy != "" {
		entry["derive"] = policy
	}
	writeServersJSON(t, dataDir, map[string]any{"servers": map[string]any{"x": entry}})
	runAgenthub(t, dataDir, "", "config", "set", "discovery", "full")
	return dataDir, counter
}

// TestASharedServerRunsEveryCallOnTheBaseInstance is the control every other
// case here is read against. Without it a spawn count of one proves nothing —
// it is also what a broken fixture that never derives anything produces.
func TestASharedServerRunsEveryCallOnTheBaseInstance(t *testing.T) {
	dataDir, counter := deriveFixture(t, "") // absent: what every pre-derive entry means

	c := startGateway(t, dataDir, "shared")
	c.initialize()
	c.waitForTool("x__echo", 30*time.Second)
	waitSpawns(t, c, counter, 1, 15*time.Second)

	if got := c.textContent(c.callTool("x__echo",
		map[string]any{"marker": "shared"}, 30*time.Second)); !strings.Contains(got, "shared") {
		c.fatalf("the call did not reach the downstream: %q", got)
	}
	// Still one process: a call on a non-deriving server runs on the base.
	waitSpawns(t, c, counter, 1, 5*time.Second)
	c.close()
}

// TestASessionDerivedServerGetsAnInstanceOfItsOwn drives `derive: session`,
// where the gateway process is the session.
//
// The second process is the whole observation. Everything else a client can
// see — the tool's name, its answer, the scope it is subject to — is
// deliberately identical to the shared case, and is asserted to be identical,
// because a derivation that changed any of them would have crossed out of the
// connection plane.
func TestASessionDerivedServerGetsAnInstanceOfItsOwn(t *testing.T) {
	dataDir, counter := deriveFixture(t, "session")

	c := startGateway(t, dataDir, "derived")
	c.initialize()
	c.waitForTool("x__echo", 30*time.Second)
	// Before any call: the base only. The derived instance is dialled on
	// first use, so seeing two here would mean something dialled eagerly.
	waitSpawns(t, c, counter, 1, 15*time.Second)

	if got := c.textContent(c.callTool("x__echo",
		map[string]any{"marker": "derived"}, 30*time.Second)); !strings.Contains(got, "derived") {
		c.fatalf("the call did not reach the downstream: %q", got)
	}
	waitSpawns(t, c, counter, 2, 30*time.Second)

	// The exposed name is unchanged — deriving is invisible to routing.
	if names := c.listTools(30 * time.Second); !hasTool(names, "x__echo") {
		c.fatalf("deriving changed the exposed surface: %v", names)
	}
	// And a second call reuses the derived instance rather than dialling
	// again: the pool holds it, or every call would cost a process.
	c.callTool("x__echo", map[string]any{"marker": "again"}, 30*time.Second)
	waitSpawns(t, c, counter, 2, 5*time.Second)
	c.close()
}

// TestRootDerivingFallsBackToTheBaseWhenNoRootIsReported covers the fallback
// direction, which the implementation states as a rule: an empty key means
// the base instance whenever the input the mode keys on is missing, and
// falling back is safe because the base spec is exactly what the operator
// configured.
//
// The default e2e client is precisely that client — it declares no roots
// capability, so the gateway never asks. A per-root server reached by a
// client with no root must therefore run on the base rather than failing or
// inventing a key.
func TestRootDerivingFallsBackToTheBaseWhenNoRootIsReported(t *testing.T) {
	dataDir, counter := deriveFixture(t, "root")

	c := startGateway(t, dataDir, "rootless")
	c.initialize() // no roots capability declared
	c.waitForTool("x__echo", 30*time.Second)
	waitSpawns(t, c, counter, 1, 15*time.Second)

	if got := c.textContent(c.callTool("x__echo",
		map[string]any{"marker": "no-root"}, 30*time.Second)); !strings.Contains(got, "no-root") {
		c.fatalf("a rootless client could not call a per-root server: %q", got)
	}
	waitSpawns(t, c, counter, 1, 5*time.Second)
	c.close()
}

// TestARootDerivedServerGetsAnInstancePerReportedRoot is the delivered half:
// a client that DOES report a root gets an instance keyed to it.
//
// The pair with the case above is what makes either meaningful. The same
// entry, the same command, the same call — and the only difference is whether
// the client answered roots/list with anything, which is the input the policy
// keys on.
func TestARootDerivedServerGetsAnInstancePerReportedRoot(t *testing.T) {
	dataDir, counter := deriveFixture(t, "root")

	c := startGateway(t, dataDir, "rooted")
	c.roots = []string{"file:///workspace/project-one"}
	c.initialize()
	c.waitForTool("x__echo", 30*time.Second)
	waitSpawns(t, c, counter, 1, 15*time.Second)

	if got := c.textContent(c.callTool("x__echo",
		map[string]any{"marker": "rooted"}, 30*time.Second)); !strings.Contains(got, "rooted") {
		c.fatalf("the call did not reach the downstream: %q", got)
	}
	waitSpawns(t, c, counter, 2, 30*time.Second)
	c.close()
}

// TestADeniedCallAcquiresNoDerivedInstance is the property the execute path
// spells out where it dials: acquiring can spawn a child, so it happens
// INSIDE the call closure, after both gates — "a call the scope gate is about
// to deny must not cause either".
//
// It is the two features meeting, and the failure is quiet. A denied call
// that still dialled would leave a refusal with a side effect: a process
// started on behalf of a request that was not allowed to run, which is both a
// resource an attacker can spend by making calls they cannot make and a
// contradiction of what the refusal reported.
func TestADeniedCallAcquiresNoDerivedInstance(t *testing.T) {
	dataDir, counter := deriveFixture(t, "session")
	// Narrow the machine's offer to nothing: the tool stays routable — the
	// entry is still enabled and connected — and the scope gate is what
	// refuses the call.
	runAgenthub(t, dataDir, "", "server", "tool", "allow", "x", "--none")

	c := startGateway(t, dataDir, "deniedderive")
	c.initialize()
	waitSpawns(t, c, counter, 1, 30*time.Second)
	waitTools(t, c, 30*time.Second, "nothing offered", func(names []string) bool {
		return !hasTool(names, "x__echo")
	})

	err := c.callToolRefused("x__echo", map[string]any{"marker": "denied"}, 30*time.Second)
	wantScopeDenied(t, c, "x__echo under an empty server allow list", err)

	// Still one process. The base was dialled at startup; nothing was dialled
	// for the call that was refused.
	waitSpawns(t, c, counter, 1, 5*time.Second)
	c.close()
}
