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

// --rules is the OLD home of the rule reading (it now lives on `server ls` /
// `server inspect`, see serverrule_test.go). For one release it must keep
// answering, byte-for-byte, and say on stderr where the reading moved — on
// stderr because the callers it is kept alive for are parsing stdout.
func TestToolRulesFlagStillAnswersAndSaysWhereItMoved(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "server", "tool", "allow", "fs", "--only", "read_file")
	mustRun(t, "", "server", "tool", "allow", "git", "--none")
	mustRun(t, "", "server", "add", "idle", "--cmd", "idle-server")

	code, stdout, stderr := runCLI(t, "", "server", "tool", "ls", "--rules", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want the deprecated flag to still work; stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "server ls") || !strings.Contains(stderr, "will be removed") {
		t.Errorf("the notice must name the replacement and its own end, got %q", stderr)
	}
	byServer := map[string]ToolRuleRow{}
	for _, r := range decodeToolRules(t, stdout) {
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
	if len(byServer) != 3 {
		t.Errorf("every configured server needs a row, got %d", len(byServer))
	}
	if out := mustRun(t, "", "server", "tool", "ls", "--rules"); !strings.Contains(out, "exposes nothing") {
		t.Errorf("the human table must still spell out the block-all state:\n%s", out)
	}
}

// Hidden, not documented: --help is what a reader consults instead of running
// the command, so a listed flag would read as the way to do this.
func TestToolRulesFlagIsNotAdvertised(t *testing.T) {
	setDataDir(t)
	if out := mustRun(t, "", "server", "tool", "ls", "--help"); strings.Contains(out, "--rules") {
		t.Errorf("the deprecated flag must not appear in help:\n%s", out)
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
	decodeInto(t, mustRun(t, "", "server", "tool", "inspect", "fs__write_file", "--json"), &blockedByProfile)
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
	mustRun(t, "", "server", "tool", "allow", "fs", "--none")
	var blockedByGlobal ToolInspect
	decodeInto(t, mustRun(t, "", "server", "tool", "inspect", "fs__read_file", "--json"), &blockedByGlobal)
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
	mustRun(t, "", "server", "tool", "allow", "fs", "--none")

	var i ToolInspect
	decodeInto(t, mustRun(t, "", "server", "tool", "inspect", "fs__read_file", "--json"), &i)
	if i.RawName != "read_file" || i.Server != "fs" {
		t.Errorf("a blocked tool must still resolve, got %+v", i)
	}

	// The two-argument form takes the server's own name, and is what makes
	// a name containing the join separator addressable at all.
	decodeInto(t, mustRun(t, "", "server", "tool", "inspect", "fs", "read_file", "--json"), &i)
	if i.Name != "fs__read_file" {
		t.Errorf("two-argument form resolved to %+v", i)
	}

	code, _, stderr := runCLI(t, "", "server", "tool", "inspect", "no_such_tool")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want not-found; stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "tool ls") {
		t.Errorf("the error must point at the listing that has the names: %s", stderr)
	}
}
