package e2e_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// The existing profile coverage in this suite is incidental: profilehotreload
// drives `profile use` and `client bind` to prove the hot-reload chain, and
// every other fixture calls `profile create` only to build a stage. The verbs
// that EDIT a profile in place — server add/rm, rename, rm, discovery — had
// no e2e case at all, and three of them can change what a running client is
// allowed to see.
//
// Each case below therefore keeps one live gateway and asserts on its
// exposed surface, not on the file the CLI wrote: a registry edit that
// nothing propagates is precisely the failure worth catching, and only a
// spawned gateway can tell the two apart.

// profileChange is the result envelope of the mutating `profile` verbs.
type profileChange struct {
	Action    string   `json:"action"`
	Name      string   `json:"name"`
	OldName   string   `json:"old_name"`
	Repointed []string `json:"repointed"`
}

// runProfile runs one mutating `profile` verb and decodes its result.
func runProfile(t *testing.T, dataDir string, args ...string) (profileChange, e2eEnvelope) {
	t.Helper()
	out, _ := runAgenthub(t, dataDir, "", append([]string{"profile"}, append(args, "--json")...)...)
	env := lastEnvelope(t, out)
	if !env.OK {
		t.Fatalf("profile %s: %s", strings.Join(args, " "), out)
	}
	var change profileChange
	if err := json.Unmarshal(env.Data, &change); err != nil {
		t.Fatalf("profile %s data: %v\n%s", strings.Join(args, " "), err, env.Data)
	}
	return change, env
}

// clientRow is the slice of cli.ClientRow these tests read back.
type clientRow struct {
	Client   string `json:"client"`
	Binding  string `json:"binding"`
	Profile  string `json:"profile"`
	Dangling bool   `json:"dangling"`
}

// clientRowByID reads `client ls --json` and returns the row for id. A client
// that was bound but never installed still has a row — the binding is enough
// to put it in the listing — so an absent row here is a real failure, not a
// property of the fixture.
func clientRowByID(t *testing.T, dataDir, id string) clientRow {
	t.Helper()
	out, _ := runAgenthub(t, dataDir, "", "client", "ls", "--json")
	env := lastEnvelope(t, out)
	if !env.OK {
		t.Fatalf("client ls --json: %s", out)
	}
	var list struct {
		Clients []clientRow `json:"clients"`
	}
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("client ls data: %v\n%s", err, env.Data)
	}
	for _, r := range list.Clients {
		if r.Client == id {
			return r
		}
	}
	t.Fatalf("client %q is not in `client ls`: %+v", id, list.Clients)
	return clientRow{}
}

// twoServerProfile is the shared stage: two downstreams, one tool each, and
// a client bound to a profile holding only the first. Two servers because a
// membership edit has to be observable in both directions — a surface that
// only ever grows cannot tell "the edit landed" from "the scope was never
// applied at all".
func twoServerProfile(t *testing.T, dataDir, profile, clientID string) {
	t.Helper()
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")
	runAgenthub(t, dataDir, "", "server", "add", "beta", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "beta")
	runAgenthub(t, dataDir, "", "profile", "create", profile, "--servers", "alpha")
	runAgenthub(t, dataDir, "", "client", "bind", clientID, profile)
}

// TestProfileMembershipEditsMoveTheExposedSurface drives `profile server
// add` and `profile server rm` underneath a live gateway.
//
// These are the two verbs an operator reaches for to widen or narrow one
// client's access after the fact, and both directions matter for a different
// reason: an `add` that does not propagate is a support ticket, while an `rm`
// that does not propagate is a tool the operator believes they took away and
// did not.
func TestProfileMembershipEditsMoveTheExposedSurface(t *testing.T) {
	dataDir := t.TempDir()
	twoServerProfile(t, dataDir, "narrow", "memberclient")

	c := startGateway(t, dataDir, "memberclient")
	c.initialize()
	c.waitForTool("alpha__echo", 30*time.Second)
	if names := c.listTools(30 * time.Second); hasTool(names, "beta__echo") {
		c.fatalf("a profile listing only alpha exposed beta: %v", names)
	}

	runProfile(t, dataDir, "server", "add", "narrow", "beta")
	c.waitForTool("beta__echo", 30*time.Second)
	if names := c.listTools(30 * time.Second); !hasTool(names, "alpha__echo") {
		c.fatalf("adding beta dropped alpha: %v", names)
	}

	runProfile(t, dataDir, "server", "rm", "narrow", "alpha")
	waitTools(t, c, 30*time.Second, "alpha__echo withdrawn", func(names []string) bool {
		return !hasTool(names, "alpha__echo")
	})
	if names := c.listTools(30 * time.Second); !hasTool(names, "beta__echo") {
		c.fatalf("removing alpha took beta with it: %v", names)
	}
	c.close()
}

// TestProfileRenameRepointsBindingsAndRemovalFailsClosed covers the two
// destructive verbs together, because their contracts are opposites and the
// pair is what makes each one legible:
//
//   - `rename` MOVES the clients bound to it (internal/cli/profile.go:82 —
//     leaving them dangling would fail-close them to an empty scope), so a
//     live client must not lose its tools across a rename;
//   - `rm` deliberately does NOT move them. A client bound to a profile that
//     no longer exists sees nothing at all, "so a deletion never widens
//     access".
//
// The second half is the one worth a real gateway. Falling back to "every
// enabled server" would be the friendlier-looking behavior and a silent
// privilege escalation: delete the profile that was narrowing a client, and
// the client would gain everything the profile was keeping from it.
func TestProfileRenameRepointsBindingsAndRemovalFailsClosed(t *testing.T) {
	dataDir := t.TempDir()
	twoServerProfile(t, dataDir, "before", "renameclient")

	c := startGateway(t, dataDir, "renameclient")
	c.initialize()
	c.waitForTool("alpha__echo", 30*time.Second)

	change, _ := runProfile(t, dataDir, "rename", "before", "after")
	if change.OldName != "before" || change.Name != "after" {
		t.Errorf("rename = %+v, want before -> after", change)
	}
	if !slices.Contains(change.Repointed, "renameclient") {
		t.Errorf("rename did not report repointing the bound client: %+v", change)
	}
	if row := clientRowByID(t, dataDir, "renameclient"); row.Profile != "after" || row.Dangling {
		t.Errorf("binding after rename = %+v, want profile=after and not dangling", row)
	}
	// The binding followed the name, so the live surface must be unchanged.
	// waitTools rather than a bare listTools: a rename is a registry write
	// like any other, and the gateway is entitled to re-resolve its scope
	// before answering — the assertion is that it settles back on alpha, not
	// that no reload happened.
	waitTools(t, c, 30*time.Second, "alpha__echo kept across the rename", func(names []string) bool {
		return hasTool(names, "alpha__echo")
	})

	_, env := runProfile(t, dataDir, "rm", "after")
	// The warning is part of the contract: deleting a profile out from under
	// a bound client is allowed, and the operator has to be told which client
	// they just fail-closed.
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "renameclient") {
		t.Errorf("`profile rm` did not warn about the client it stranded: %v", env.Warnings)
	}
	if row := clientRowByID(t, dataDir, "renameclient"); !row.Dangling {
		t.Errorf("binding after rm = %+v, want dangling", row)
	}

	waitTools(t, c, 30*time.Second, "the scope fails closed to nothing", func(names []string) bool {
		return !hasTool(names, "alpha__echo")
	})
	if names := c.listTools(30 * time.Second); hasTool(names, "beta__echo") {
		c.fatalf("deleting the profile WIDENED the client's access to beta: %v", names)
	}
	c.close()
}

// TestProfileDiscoveryOverridesTheGlobalDefault pins the per-profile
// discovery override against a global default set the other way.
//
// The two settings live in different files (governance.json and
// profiles.json) and only the gateway ever reconciles them, so "the profile
// wins" is a claim no single-file test can make. The `-` form putting the
// profile back under the global default is the same claim in reverse, and is
// the half that rots quietly: a broken clear leaves the last explicit value
// in force, which looks like nothing happened.
func TestProfileDiscoveryOverridesTheGlobalDefault(t *testing.T) {
	dataDir := t.TempDir()
	// enableServer pins the GLOBAL discovery to full, which is what makes
	// this test meaningful: every disagreement below is the profile's doing.
	twoServerProfile(t, dataDir, "disco", "discoclient")

	c := startGateway(t, dataDir, "discoclient")
	c.initialize()
	c.waitForTool("alpha__echo", 30*time.Second)

	runProfile(t, dataDir, "discovery", "disco", "lazy")
	waitTools(t, c, 30*time.Second, "the lazy meta-tool surface", func(names []string) bool {
		if hasTool(names, "alpha__echo") {
			return false
		}
		for _, meta := range lazyMetaTools {
			if !hasTool(names, meta) {
				return false
			}
		}
		return true
	})

	// `-` drops the override; the global full setting must take back over.
	runProfile(t, dataDir, "discovery", "disco", "-")
	waitTools(t, c, 30*time.Second, "the global default back in force", func(names []string) bool {
		return hasTool(names, "alpha__echo")
	})
	c.close()
}
