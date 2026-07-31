package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeToolRules(t *testing.T, out string) ToolRuleList {
	t.Helper()
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	var rows ToolRuleList
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("data is not a rule list: %v\n%s", err, env.Data)
	}
	return rows
}

// The rule table is the only reader that can express a server configured to
// expose NOTHING: it contributes no tool rows, so a per-tool listing cannot
// show it at all — and that is the state most worth finding.
func TestToolRulesReportsAllThreeStates(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "tool", "allow", "fs", "--only", "read_file")
	mustRun(t, "", "tool", "allow", "git", "--none")
	mustRun(t, "", "server", "add", "idle", "--cmd", "idle-server")

	byServer := map[string]ToolRuleRow{}
	for _, r := range decodeToolRules(t, mustRun(t, "", "tool", "ls", "--rules", "--json")) {
		byServer[r.Server] = r
	}
	if got := byServer["fs"]; got.Rule != "only" || len(got.Tools) != 1 || got.Cached != 2 {
		t.Errorf("fs row = %+v, want only/1 tool/2 cached", got)
	}
	if got := byServer["git"]; got.Rule != "none" || got.Tools == nil || len(got.Tools) != 0 {
		t.Errorf("git row = %+v, want none with an EMPTY (not null) list", got)
	}
	if got := byServer["idle"]; got.Rule != "all" || got.Tools != nil {
		t.Errorf("idle row = %+v, want all with a null list", got)
	}
	// A server with no rule still gets a row: "which of my servers is
	// narrowed" cannot be answered by a list of the narrowed ones alone.
	if len(byServer) != 3 {
		t.Errorf("every configured server needs a row, got %d", len(byServer))
	}

	out := mustRun(t, "", "tool", "ls", "--rules")
	if !strings.Contains(out, "exposes nothing") {
		t.Errorf("the human table must spell out the block-all state:\n%s", out)
	}
}

// A name no cached tool matches is marked in the rule table, because after
// the one warning at write time nothing else ever mentions it again.
func TestToolRulesMarksUnknownNames(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "tool", "allow", "fs", "--only", "read_file,reed_file")

	rows := decodeToolRules(t, mustRun(t, "", "tool", "ls", "--rules", "fs", "--json"))
	if len(rows) != 1 || len(rows[0].Unknown) != 1 || rows[0].Unknown[0] != "reed_file" {
		t.Fatalf("rule row = %+v, want reed_file reported unknown", rows)
	}
	if out := mustRun(t, "", "tool", "ls", "--rules", "fs"); !strings.Contains(out, "!reed_file") {
		t.Errorf("the human table must mark the unknown name:\n%s", out)
	}
}

// The point of inspect: which LAYER took the tool away. Both answers have to
// be distinguishable, because they need different repairs.
func TestToolInspectNamesTheLayerThatBlocked(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "profile", "create", "work")
	mustRun(t, "", "profile", "tool", "allow", "work", "fs", "--only", "read_file")
	mustRun(t, "", "profile", "use", "work")

	var blockedByProfile ToolInspect
	decodeInto(t, mustRun(t, "", "tool", "inspect", "fs__write_file", "--json"), &blockedByProfile)
	if !blockedByProfile.Global.Allowed {
		t.Errorf("no global rule exists, so the global layer must allow it: %+v", blockedByProfile.Global)
	}
	var profileVerdict *ToolVerdict
	for i, p := range blockedByProfile.Profiles {
		if p.Layer == "work" {
			profileVerdict = &blockedByProfile.Profiles[i]
		}
	}
	if profileVerdict == nil || profileVerdict.Allowed {
		t.Fatalf("the profile must be the layer that blocks: %+v", blockedByProfile.Profiles)
	}
	if !strings.Contains(blockedByProfile.Default, "BLOCKS") {
		t.Errorf("the default line must say an unbound client loses it: %q", blockedByProfile.Default)
	}

	// Now the other layer, on a tool the profile would have allowed.
	mustRun(t, "", "tool", "allow", "fs", "--none")
	var blockedByGlobal ToolInspect
	decodeInto(t, mustRun(t, "", "tool", "inspect", "fs__read_file", "--json"), &blockedByGlobal)
	if blockedByGlobal.Global.Allowed {
		t.Errorf("the global layer must now block: %+v", blockedByGlobal.Global)
	}
	if !strings.Contains(blockedByGlobal.Default, "above every profile") {
		t.Errorf("the default line must name the layer that outranks: %q", blockedByGlobal.Default)
	}
}

// A tool nobody can currently see is exactly the one being asked about, so
// the lookup must reach blocked tools too.
func TestToolInspectResolvesBlockedAndAmbiguousNames(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "tool", "allow", "fs", "--none")

	var i ToolInspect
	decodeInto(t, mustRun(t, "", "tool", "inspect", "fs__read_file", "--json"), &i)
	if i.RawName != "read_file" || i.Server != "fs" {
		t.Errorf("a blocked tool must still resolve, got %+v", i)
	}

	// The two-argument form takes the server's own name, and is what makes
	// a name containing the join separator addressable at all.
	decodeInto(t, mustRun(t, "", "tool", "inspect", "fs", "read_file", "--json"), &i)
	if i.Name != "fs__read_file" {
		t.Errorf("two-argument form resolved to %+v", i)
	}

	code, _, stderr := runCLI(t, "", "tool", "inspect", "no_such_tool")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want not-found; stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "tool ls") {
		t.Errorf("the error must point at the listing that has the names: %s", stderr)
	}
}
