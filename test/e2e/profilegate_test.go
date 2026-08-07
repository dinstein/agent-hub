package e2e_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scopegate_test.go closed the call-side gap for two of the three narrowing
// layers: a profile deciding which SERVERS a client sees, and `server tool
// allow` deciding which tools the machine offers at all. The third — `profile
// tool allow`, one server's tools narrowed for one profile — had no call-side
// case, and neither did any verb that TAKES something away.
//
// Both are the same omission seen from different sides, and the second is the
// one AGENTS.md's model rests on. Every layer intersects and none can widen,
// so a narrowing that does not reach the execute path is not a smaller
// permission — it is no permission at all, worn as one. A test that only
// reads tools/list cannot see the difference.
//
// The profile tool layer also fails the most quietly of the three. A hole in
// `server tool allow` is open to every client at once and shows up the first
// time anyone looks; a hole here is open only to the clients on that profile,
// which is exactly the population an operator narrowed on purpose and
// therefore watches least.

// TestAProfileToolAllowListRefusesTheCallItWithholds narrows a profile's view
// of a server whose own allow list is wider, calls the tool the profile
// dropped, then widens the PROFILE and calls it again.
//
// Which layer withholds the tool matters here, and the fixture is built so
// only one can: the machine offers both tools (no `server tool allow` at
// all), so a refusal cannot be the global layer's, and the downstream serves
// both, so it cannot be the server's either. What is left is the profile.
//
// The widen is the first anywhere in the suite for this layer. Until it, the
// three `profile tool allow` fixtures in the tree only ever narrowed, and a
// narrowing that is never undone cannot be told from a fixture whose server
// never had the tool.
func TestAProfileToolAllowListRefusesTheCallItWithholds(t *testing.T) {
	dataDir := t.TempDir()

	script := filepath.Join(t.TempDir(), "two.json")
	writeScript(t, script, "read_thing", "write_thing")
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--args", script, "--json")
	enableServer(t, dataDir, "alpha")
	// No `server tool allow`: the machine offers everything alpha serves, so
	// the global layer is provably not the one narrowing below.
	runAgenthub(t, dataDir, "", "profile", "create", "narrow", "--servers", "alpha")
	runAgenthub(t, dataDir, "", "profile", "tool", "allow", "narrow", "alpha", "--only", "read_thing")
	runAgenthub(t, dataDir, "", "client", "bind", "profiletoolclient", "narrow")

	c := startGateway(t, dataDir, "profiletoolclient")
	c.initialize()
	c.waitForTool("alpha__read_thing", 30*time.Second)
	if names := c.listTools(30 * time.Second); hasTool(names, "alpha__write_thing") {
		c.fatalf("a profile allowing only read_thing exposed write_thing: %v", names)
	}
	// The sibling answers, so alpha is demonstrably serving both names.
	if got := c.textContent(c.callTool("alpha__read_thing", map[string]any{"marker": "allowed"},
		30*time.Second)); !strings.Contains(got, "allowed") {
		c.fatalf("alpha__read_thing did not echo: %q", got)
	}

	err := c.callToolRefused("alpha__write_thing", map[string]any{"marker": "should-not-run"}, 30*time.Second)
	wantScopeDenied(t, c, "alpha__write_thing under a profile allow list that omits it", err)

	// --all drops the profile's rule. The global layer never had one, so the
	// intersection is now everything alpha serves.
	runAgenthub(t, dataDir, "", "profile", "tool", "allow", "narrow", "alpha", "--all")
	c.waitForTool("alpha__write_thing", 30*time.Second)
	if got := c.textContent(c.callTool("alpha__write_thing", map[string]any{"marker": "now-allowed"},
		30*time.Second)); !strings.Contains(got, "now-allowed") {
		c.fatalf("alpha__write_thing did not answer once the profile allowed it: %q", got)
	}
	c.close()
}

// TestRevokingAProfileRefusesTheCallsItWithdrew drives the two verbs that
// TAKE access away, in the order an operator meets them: narrow the profile
// by one server, then delete the profile out from under the client.
//
// profile_test.go covers both against tools/list, and its own comment names
// the reason this file exists: "an `rm` that does not propagate is a tool the
// operator believes they took away and did not". A withdrawal is precisely
// where a listing assertion is weakest — a revocation that reaches the
// projection and not the gate LOOKS like it worked from every angle an
// operator can see, and the tool stays callable for anyone who kept the name.
//
// The second half is the fail-closed rule at the execute path. A client bound
// to a profile that no longer exists resolves to the empty set rather than to
// "every enabled server", and the gate is where that has to hold: falling
// back to allow-all there would turn deleting a narrowing profile into
// granting its client everything the profile was keeping from it.
func TestRevokingAProfileRefusesTheCallsItWithdrew(t *testing.T) {
	dataDir := t.TempDir()
	twoServerProfile(t, dataDir, "wide", "revokeclient")
	// twoServerProfile's profile holds alpha only; this case needs both, so
	// the withdrawal below has something to withdraw.
	runProfile(t, dataDir, "server", "add", "wide", "beta")

	c := startGateway(t, dataDir, "revokeclient")
	c.initialize()
	c.waitForTool("alpha__echo", 30*time.Second)
	c.waitForTool("beta__echo", 30*time.Second)
	// Both callable first: whatever is refused later was taken away, not
	// missing from the start.
	for _, tool := range []string{"alpha__echo", "beta__echo"} {
		if got := c.textContent(c.callTool(tool, map[string]any{"marker": "before-revoke"},
			30*time.Second)); !strings.Contains(got, "before-revoke") {
			c.fatalf("%s did not echo before the revocation: %q", tool, got)
		}
	}

	// Withdraw beta from the profile.
	runProfile(t, dataDir, "server", "rm", "wide", "beta")
	waitTools(t, c, 30*time.Second, "beta__echo withdrawn", func(names []string) bool {
		return !hasTool(names, "beta__echo")
	})
	err := c.callToolRefused("beta__echo", map[string]any{"marker": "should-not-run"}, 30*time.Second)
	wantScopeDenied(t, c, "beta__echo after `profile server rm`", err)
	// alpha is untouched — a revocation that took a bystander with it would
	// be a different bug wearing the same green test.
	if got := c.textContent(c.callTool("alpha__echo", map[string]any{"marker": "still-allowed"},
		30*time.Second)); !strings.Contains(got, "still-allowed") {
		c.fatalf("removing beta took alpha's callability with it: %q", got)
	}

	// Delete the profile the client is bound to. Its binding is now dangling,
	// which resolves to the empty set — at the gate as well as in the listing.
	runProfile(t, dataDir, "rm", "wide")
	waitTools(t, c, 30*time.Second, "the surface fails closed to nothing", func(names []string) bool {
		return !hasTool(names, "alpha__echo")
	})
	err = c.callToolRefused("alpha__echo", map[string]any{"marker": "should-not-run"}, 30*time.Second)
	wantScopeDenied(t, c, "alpha__echo under a deleted profile", err)
	// And the deletion must not have WIDENED anything: beta was withdrawn
	// before the profile went away, and a fallback to "every enabled server"
	// would hand it back here.
	err = c.callToolRefused("beta__echo", map[string]any{"marker": "should-not-run"}, 30*time.Second)
	wantScopeDenied(t, c, "beta__echo under a deleted profile", err)
	c.close()
}
