package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// This file pins the SHAPE of the command tree rather than any single
// command's behavior:
//
//   - docs/modules/controlplane.md is the acceptance checklist — every command it lists
//     must exist, spelled exactly as listed;
//   - canonical.md §3 freezes the naming rules — resource groups are
//     singular with a plural alias, listings are always `ls`, and EVERY
//     command supports --json.
//
// A rule enforced by a walk of the real tree cannot be forgotten when the
// next group is added; a rule enforced by review can.

// newTestRoot builds the command tree the way Main does.
func newTestRoot(t *testing.T) *cobra.Command {
	t.Helper()
	app := &App{version: "test", stdin: strings.NewReader(""), stdout: nil, stderr: nil}
	return app.newRoot()
}

// newReleaseTestRoot builds the tree the way a release build's Main does
// (main.channel == "release").
func newReleaseTestRoot(t *testing.T) *cobra.Command {
	t.Helper()
	app := &App{version: "test", stdin: strings.NewReader(""), reducedHelp: true}
	return app.newRoot()
}

// withheldGroups is what the release help page keeps back, and
// withheldCommands its flattened membership. Both are spelled out here rather
// than read back from the tree, so the test fails if a group's membership
// drifts instead of silently agreeing with whatever it finds.
var withheldGroups = []*cobra.Group{groupDaemon, groupManage}

// `profile` is deliberately NOT withheld: a shipped build that can connect a
// client but cannot say what that client will then see teaches half the
// model. Setup -> profile -> client bind is the whole everyday path.
var withheldCommands = []string{
	"daemon", "session", "events", "token",
	"approval", "grant", "config", "secret", "tool", "audit", "activity",
	"skill", "doctor",
}

// walk visits every command in the tree, root included.
func walk(cmd *cobra.Command, fn func(*cobra.Command)) {
	fn(cmd)
	for _, c := range cmd.Commands() {
		walk(c, fn)
	}
}

// commandPaths returns every command path in the tree, minus cobra's
// built-in help command.
func commandPaths(root *cobra.Command) map[string]*cobra.Command {
	out := map[string]*cobra.Command{}
	walk(root, func(c *cobra.Command) {
		if c.Name() == "help" || c.Name() == "completion" {
			return
		}
		out[c.CommandPath()] = c
	})
	return out
}

// TestCommandTreeCoversDesign asserts every command of docs/modules/controlplane.md (as
// reconciled by canonical.md §3) exists.
func TestCommandTreeCoversDesign(t *testing.T) {
	want := []string{
		"agenthub connect",
		"agenthub daemon start", "agenthub daemon stop", "agenthub daemon restart",
		"agenthub daemon status", "agenthub daemon logs",
		"agenthub server add", "agenthub server rm", "agenthub server ls",
		"agenthub server enable", "agenthub server disable",
		"agenthub server inspect", "agenthub server test",
		"agenthub profile ls", "agenthub profile create", "agenthub profile rm",
		"agenthub profile rename", "agenthub profile use",
		"agenthub profile server add", "agenthub profile server rm", "agenthub profile tools",
		"agenthub profile discovery",
		"agenthub client detect", "agenthub client connect",
		"agenthub client disconnect", "agenthub client import",
		"agenthub client ls", "agenthub client bind", "agenthub client unbind",
		"agenthub session ls", "agenthub session show",
		"agenthub session scope", "agenthub session kill",
		"agenthub approval watch", "agenthub approval ls",
		"agenthub approval approve", "agenthub approval deny",
		"agenthub grant ls", "agenthub grant approve", "agenthub grant deny",
		"agenthub events",
		"agenthub secret set", "agenthub secret rm", "agenthub secret ls",
		"agenthub auth login", "agenthub auth status",
		"agenthub auth refresh", "agenthub auth logout",
		"agenthub tool ls", "agenthub tool disable", "agenthub tool enable",
		"agenthub tool override", "agenthub tool pin",
		"agenthub tool quarantine ls", "agenthub tool quarantine release",
		"agenthub audit tail", "agenthub audit export",
		"agenthub activity",
		"agenthub skill ls", "agenthub skill inspect", "agenthub skill add",
		"agenthub skill rm", "agenthub skill enable", "agenthub skill disable",
		"agenthub skill install-to", "agenthub skill sync",
		"agenthub skill update", "agenthub skill verify",
		"agenthub config get", "agenthub config set", "agenthub config ls",
		"agenthub doctor",
	}
	have := commandPaths(newTestRoot(t))
	var missing []string
	for _, w := range want {
		if _, ok := have[w]; !ok {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("command tree is missing %d command(s):\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

// TestEveryCommandHasJSON walks the tree and asserts --json reaches every
// runnable command (canonical.md §3: "every command must have --json"). It is a
// persistent flag on the root, so this really asserts that no command
// shadows or detaches it.
func TestEveryCommandHasJSON(t *testing.T) {
	root := newTestRoot(t)
	var offenders []string
	walk(root, func(c *cobra.Command) {
		if c == root || c.Name() == "help" || c.Name() == "completion" {
			return
		}
		if c.Flags().Lookup("json") == nil && c.InheritedFlags().Lookup("json") == nil {
			offenders = append(offenders, c.CommandPath())
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("commands without --json:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestResourceGroupsAreSingularWithPluralAlias pins the naming rule for the
// resource groups. The action/stream groups (daemon, scope, auth, audit,
// activity, events, config, doctor, connect) keep their names unchanged and
// are deliberately NOT in this list.
func TestResourceGroupsAreSingularWithPluralAlias(t *testing.T) {
	root := newTestRoot(t)
	for _, name := range []string{
		"server", "profile", "client", "session", "tool", "skill", "secret", "approval", "grant",
	} {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Errorf("group %q not found (found %v, err %v)", name, cmd.Name(), err)
			continue
		}
		plural := name + "s"
		found := false
		for _, alias := range cmd.Aliases {
			if alias == plural {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("group %q must carry the plural alias %q, aliases=%v", name, plural, cmd.Aliases)
		}
		// The alias must actually resolve, not merely be declared.
		if aliased, _, aerr := root.Find([]string{plural}); aerr != nil || aliased.Name() != name {
			t.Errorf("alias %q does not resolve to %q (got %v, err %v)", plural, name, aliased.Name(), aerr)
		}
	}
}

// TestListingsAreNamedLs pins "listing subcommands are always named ls": no group may ship a
// `list` (or `show-all`, `dump`, ...) spelling of a listing.
func TestListingsAreNamedLs(t *testing.T) {
	root := newTestRoot(t)
	var offenders []string
	walk(root, func(c *cobra.Command) {
		switch c.Name() {
		case "list", "ls-all", "listall", "dump":
			offenders = append(offenders, c.CommandPath())
		}
		for _, alias := range c.Aliases {
			if alias == "list" {
				offenders = append(offenders, c.CommandPath()+" (alias list)")
			}
		}
	})
	if len(offenders) > 0 {
		t.Errorf("listings must be named 'ls':\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestEveryGroupShowsHelpOnBareInvocation: a bare group prints help and
// exits 0, an unknown subcommand is a usage error (exit 2). Both are the
// groupRunE contract, and both must hold for every group, old and new.
func TestEveryGroupShowsHelpOnBareInvocation(t *testing.T) {
	groups := []string{
		"server", "profile", "client", "session", "secret", "token",
		"tool", "skill", "config", "audit", "approval", "grant", "daemon", "auth",
	}
	for _, g := range groups {
		t.Run(g, func(t *testing.T) {
			setDataDir(t)
			code, out, _ := runCLI(t, "", g)
			if code != ExitOK {
				t.Fatalf("bare %q exit = %d", g, code)
			}
			if !strings.Contains(out, "Usage:") {
				t.Errorf("bare %q did not print help:\n%s", g, out)
			}
			code, _, stderr := runCLI(t, "", g, "definitely-not-a-subcommand")
			if code != ExitUsage {
				t.Errorf("unknown %q subcommand exit = %d, want %d (stderr %s)", g, code, ExitUsage, stderr)
			}
		})
	}
}

// TestRootHelpOrderIsTheOnboardingPath pins the root listing: the phase
// order (setup -> wire up -> daemon -> manage, connect held apart)
// is the one thing on the help page that carries meaning, and with
// EnableCommandSorting off it is only as good as the declaration order. A new
// command appended to whichever AddCommand call is nearest is how that
// meaning erodes without anything failing.
func TestRootHelpOrderIsTheOnboardingPath(t *testing.T) {
	want := []struct {
		group string
		cmds  []string
	}{
		// `server` first and `catalog` last: the catalog is a small curated
		// set, so leading with it teaches a path that ends in "not listed"
		// for most servers. `secret` is not in Setup at all — credentials are
		// normally handled by `server add` and `auth login`, and a manual
		// command up front implies a step the everyday path does not have.
		{"setup", []string{"server", "auth", "catalog"}},
		{"wire", []string{"profile", "client"}},
		// Split on one testable question — does this need a running daemon?
		// `token` mints credentials for the daemon's HTTP data plane, so it
		// sits with the daemon rather than with the other governance verbs.
		{"daemon", []string{"daemon", "session", "events", "token"}},
		{"manage", []string{
			"approval", "grant", "config", "secret", "tool", "audit", "activity",
			"skill", "doctor",
		}},
		{"entry", []string{"connect"}},
	}
	root := newTestRoot(t)

	var gotGroups []string
	for _, g := range root.Groups() {
		gotGroups = append(gotGroups, g.ID)
	}
	var wantGroups []string
	for _, w := range want {
		wantGroups = append(wantGroups, w.group)
	}
	if strings.Join(gotGroups, ",") != strings.Join(wantGroups, ",") {
		t.Errorf("group order = %v, want %v", gotGroups, wantGroups)
	}

	byGroup := map[string][]string{}
	var order []string
	for _, c := range root.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if _, seen := byGroup[c.GroupID]; !seen {
			order = append(order, c.GroupID)
		}
		byGroup[c.GroupID] = append(byGroup[c.GroupID], c.Name())
	}
	for _, w := range want {
		if got := strings.Join(byGroup[w.group], ","); got != strings.Join(w.cmds, ",") {
			t.Errorf("group %q = [%s], want [%s]", w.group, got, strings.Join(w.cmds, ","))
		}
	}
	// Commands are registered group by group, so a command added to the wrong
	// call site splits its group across the listing; cobra renders groups in
	// declaration order regardless, hiding the mistake in the source.
	if len(order) != len(want) {
		t.Errorf("commands are not registered contiguously by group: encountered %v", order)
	}
}

// TestListingsComeFirstInTheirGroup: with sorting off, a group that leads
// with a mutation teaches the destructive verb before the one that shows you
// what you would destroy. `ls` first is the house shape (canonical.md §3
// already makes `ls` the only spelling of a listing).
func TestListingsComeFirstInTheirGroup(t *testing.T) {
	root := newTestRoot(t)
	var offenders []string
	walk(root, func(parent *cobra.Command) {
		subs := parent.Commands()
		hasLs, lsIsFirst := false, false
		for i, c := range subs {
			if c.Name() == "ls" {
				hasLs = true
				lsIsFirst = i == 0
			}
		}
		if hasLs && !lsIsFirst {
			offenders = append(offenders, parent.CommandPath())
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("groups whose 'ls' is not listed first:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestEveryTopLevelCommandIsGrouped: cobra does not error on a missing
// GroupID, it quietly files the command under "Additional Commands" — the
// same failure mode as a missing Short, a delivered command nobody finds.
func TestEveryTopLevelCommandIsGrouped(t *testing.T) {
	root := newTestRoot(t)
	declared := map[string]bool{}
	for _, g := range root.Groups() {
		declared[g.ID] = true
	}
	var ungrouped, unknown []string
	for _, c := range root.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		switch {
		case c.GroupID == "":
			ungrouped = append(ungrouped, c.Name())
		case !declared[c.GroupID]:
			unknown = append(unknown, c.Name()+" (group "+c.GroupID+")")
		}
	}
	if len(ungrouped) > 0 {
		sort.Strings(ungrouped)
		t.Errorf("top-level commands with no GroupID (they land in \"Additional Commands\"):\n  %s",
			strings.Join(ungrouped, "\n  "))
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Errorf("top-level commands whose GroupID is not a declared group:\n  %s",
			strings.Join(unknown, "\n  "))
	}
}

// TestEveryCommandHasShortHelp: a command with no Short is invisible in the
// group listing, which is how a delivered command silently becomes
// undiscoverable.
func TestEveryCommandHasShortHelp(t *testing.T) {
	root := newTestRoot(t)
	var offenders []string
	walk(root, func(c *cobra.Command) {
		if c.Name() == "help" || c.Name() == "completion" {
			return
		}
		if strings.TrimSpace(c.Short) == "" {
			offenders = append(offenders, c.CommandPath())
		}
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("commands without a Short description:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestReleaseHidesExactlyTheWithheldCommands: a release build withholds the
// Daemon and Manage groups from the help page and nothing else.
// Asserting the complement matters as much as the groups themselves —
// `Hidden` is one field on a shared tree, and hiding one command too many is
// the same edit as hiding the right thirteen.
func TestReleaseHidesExactlyTheWithheldCommands(t *testing.T) {
	hidden := map[string]bool{}
	for _, name := range withheldCommands {
		hidden[name] = true
	}
	for _, c := range newReleaseTestRoot(t).Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if want := hidden[c.Name()]; c.Hidden != want {
			t.Errorf("release build: %q Hidden = %v, want %v", c.Name(), c.Hidden, want)
		}
	}

	// Hiding every member is not enough on its own: cobra renders a group's
	// title from the group list, so a still-declared group would print a bare
	// heading and advertise exactly what is being withheld. Assert on the
	// rendered page, which is the thing the user actually reads.
	setDataDir(t)
	_, out, _ := runCLIReleaseHelp(t, "", "--help")
	for _, g := range withheldGroups {
		if strings.Contains(out, g.Title) {
			t.Errorf("release --help still shows the %q heading:\n%s", g.ID, out)
		}
	}
	for _, name := range withheldCommands {
		if strings.Contains(out, "  "+name+" ") {
			t.Errorf("release --help still lists %q:\n%s", name, out)
		}
	}
	// The rest of the page must be untouched — this is a help-page edit, not
	// a reshuffle of everything else.
	for _, g := range []*cobra.Group{groupSetup, groupWire, groupEntry} {
		if !strings.Contains(out, g.Title) {
			t.Errorf("release --help dropped the %q heading, which stays visible:\n%s", g.ID, out)
		}
	}
	for _, other := range []string{"catalog", "server", "auth", "profile", "client", "connect"} {
		if !strings.Contains(out, "  "+other+" ") {
			t.Errorf("release --help dropped %q, which is not withheld:\n%s", other, out)
		}
	}
}

// TestDevShowsEveryCommand pins the default: an unstamped `go build` is a
// development build and teaches the whole surface. This is the side that
// would silently pass if ReducedHelp were ever wired to the wrong sense of
// the channel check.
func TestDevShowsEveryCommand(t *testing.T) {
	for _, c := range newTestRoot(t).Commands() {
		if c.Hidden {
			t.Errorf("dev build: %q is hidden, want visible", c.Name())
		}
	}
}

// TestHiddenCommandsStillRun is the load-bearing one: hiding must not become
// disabling. A hidden command that stopped resolving would look exactly like a
// correctly hidden one on the help page, and only show up as a broken
// `agenthub tool ls` in a shipped binary.
func TestHiddenCommandsStillRun(t *testing.T) {
	// Every withheld group still RESOLVES — cobra's Find must reach past
	// Hidden, at the group and at the leaf.
	root := newReleaseTestRoot(t)
	for _, path := range [][]string{
		{"tool", "ls"}, {"token"},
		{"audit", "tail"}, {"config", "ls"}, {"approval", "ls"}, {"grant", "ls"},
		{"daemon", "status"}, {"session", "ls"}, {"events"}, {"activity"}, {"doctor"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Errorf("release build: %v does not resolve (got %v, err %v)", path, cmd.Name(), err)
		}
	}
	// And they still EXECUTE end to end. One offline-capable listing per
	// withheld group: a real exit 0 with real output proves the RunE was
	// reached, not merely that a command object exists.
	for _, args := range [][]string{{"profile", "ls"}, {"config", "ls"}, {"doctor"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			setDataDir(t)
			code, out, stderr := runCLIReleaseHelp(t, "", args...)
			if code != ExitOK {
				t.Fatalf("release build: %v exit = %d, want %d (stderr %s)", args, code, ExitOK, stderr)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("release build: %v printed nothing:\n%s", args, out)
			}
		})
	}
	// A daemon-dependent one must fail on the DAEMON, not on the tree: exit 4
	// says it ran and could not reach a daemon, exit 2 would say cobra never
	// found the command — the failure this whole test exists to catch.
	setDataDir(t)
	if code, _, stderr := runCLIReleaseHelp(t, "", "session", "ls"); code != ExitDaemonDown {
		t.Errorf("release build: `session ls` exit = %d, want %d (stderr %s)", code, ExitDaemonDown, stderr)
	}
}
