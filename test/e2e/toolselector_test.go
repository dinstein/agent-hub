package e2e_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The tool selector exists at two altitudes — `server tool allow` decides what
// the machine offers at all, `profile tool allow` decides what one profile
// passes on — and the rule joining them is that they INTERSECT and neither can
// widen the other (AGENTS.md, "a tool selector is an allow list, never a deny
// list"; internal/cli/toolallow.go's header says the same from the other end).
//
// Until this file the suite covered the profile layer alone
// (profilehotreload_test.go), which is the half that cannot fail dangerously:
// a profile rule that does not apply exposes tools the operator meant to keep
// from one client. The server layer failing the same way exposes them to
// EVERY client, and a profile silently widening past it is the one direction
// the model promises is impossible.
//
// Only a live gateway can answer this. The two rules live in different
// registry documents — servers.json and profiles.json — and nothing but
// scope.Merge ever puts them together, so a test that read either file back
// would be checking that the CLI wrote what it was told, which is a different
// claim.

// alphaTools filters an exposed tool list down to the fixture's server and
// strips the routing prefix, so an assertion reads in the downstream's own
// names — the names both `allow` verbs take.
func alphaTools(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if bare, ok := strings.CutPrefix(n, "alpha__"); ok {
			out = append(out, bare)
		}
	}
	slices.Sort(out)
	return out
}

// waitAlphaTools polls until the exposed surface of the fixture server is
// exactly want. Exactness is the point: a selector test that only asserted
// presence would pass on a merge that had stopped narrowing at all.
func waitAlphaTools(t *testing.T, c *gatewayClient, budget time.Duration, want ...string) {
	t.Helper()
	slices.Sort(want)
	waitTools(t, c, budget, "alpha tools = "+strings.Join(want, ","), func(names []string) bool {
		return slices.Equal(alphaTools(names), want)
	})
}

// TestServerAndProfileToolSelectorsIntersect walks one live client through the
// three states the two layers can be in together, in the order that makes each
// one's absence legible.
//
//	server {read,write} × profile {read,purge} → read
//	                        write is the profile narrowing the machine's offer;
//	                        purge is the profile FAILING to widen past it.
//	server (no rule)    × profile {read,purge} → read, purge
//	                        purge comes back, which is what proves its earlier
//	                        absence was the server layer rather than a fixture
//	                        that never had the tool.
//	server {}           × profile {read,purge} → nothing
//	                        [] is not nil: "offer none" must not read as "no
//	                        rule", the failure AGENTS.md pins on omitzero.
func TestServerAndProfileToolSelectorsIntersect(t *testing.T) {
	dataDir := t.TempDir()

	script := filepath.Join(t.TempDir(), "three.json")
	writeScript(t, script, "read_thing", "write_thing", "purge_thing")
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--args", script, "--json")
	enableServer(t, dataDir, "alpha")

	// The machine's offer: purge_thing is not on it, for anybody.
	runAgenthub(t, dataDir, "", "server", "tool", "allow", "alpha", "--only", "read_thing,write_thing")
	// The profile's offer names purge_thing anyway. A profile is written by
	// the same operator but applies one layer down, and this is the request
	// the model has to refuse rather than honour.
	runAgenthub(t, dataDir, "", "profile", "create", "narrow", "--servers", "alpha")
	runAgenthub(t, dataDir, "", "profile", "tool", "allow", "narrow", "alpha", "--only", "read_thing,purge_thing")
	runAgenthub(t, dataDir, "", "client", "bind", "narrowclient", "narrow")

	c := startGateway(t, dataDir, "narrowclient")
	c.initialize()

	waitAlphaTools(t, c, 30*time.Second, "read_thing")

	// The tool that did survive is a real tool, not a name on a list: the
	// intersection has to leave something callable behind, or the assertion
	// above would also hold for a scope that resolved to garbage.
	if got := c.textContent(c.callTool("alpha__read_thing", map[string]any{"marker": "intersected"}, 30*time.Second)); !strings.Contains(got, "intersected") {
		t.Fatalf("alpha__read_thing did not echo through the intersection: %q", got)
	}

	// Drop the machine's rule. The profile is untouched, so whatever appears
	// now was being withheld by the server layer.
	runAgenthub(t, dataDir, "", "server", "tool", "allow", "alpha", "--all")
	waitAlphaTools(t, c, 30*time.Second, "purge_thing", "read_thing")

	// And the state an operator reaches for in an incident. `--none` leaves
	// the server registered and offering nothing; the profile still names two
	// of its tools, and must not resurrect either.
	runAgenthub(t, dataDir, "", "server", "tool", "allow", "alpha", "--none")
	waitAlphaTools(t, c, 30*time.Second)

	c.close()
}
