package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeServerRows(t *testing.T, out string) map[string]ServerRow {
	t.Helper()
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	var rows []ServerRow
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("data is not a server list: %v\n%s", err, env.Data)
	}
	byID := make(map[string]ServerRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	return byID
}

// The rule is a field on the server entry, so the command that lists servers
// is where it is read. All three states have to be distinguishable, and the
// one with no tools to hang a row on — a server offering NOTHING — is the
// state most worth finding.
func TestServerLsCarriesTheToolRule(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "server", "tool", "allow", "fs", "--only", "read_file")
	mustRun(t, "", "server", "tool", "allow", "git", "--none")
	mustRun(t, "", "server", "add", "idle", "--cmd", "idle-server")

	rows := decodeServerRows(t, mustRun(t, "", "server", "ls", "--json"))
	if got := rows["fs"].ToolRule; got == nil || got.Rule != "only" || len(got.Tools) != 1 || got.Cached != 2 {
		t.Errorf("fs rule = %+v, want only/1 tool/2 cached", got)
	}
	if got := rows["git"].ToolRule; got == nil || got.Rule != "none" || got.Tools == nil || len(got.Tools) != 0 {
		t.Errorf("git rule = %+v, want none with an EMPTY (not null) list", got)
	}
	if got := rows["idle"].ToolRule; got == nil || got.Rule != "all" || got.Tools != nil {
		t.Errorf("idle rule = %+v, want all with a null list", got)
	}

	out := mustRun(t, "", "server", "ls")
	if !strings.Contains(out, "TOOLS") {
		t.Fatalf("the TOOLS column must appear once a server narrows:\n%s", out)
	}
	// The count is read against the catalog total, so "only" is never printed
	// without saying how much it left out.
	if !strings.Contains(out, "only 1/2") || !strings.Contains(out, "none") {
		t.Errorf("the table must show the state per server:\n%s", out)
	}
}

// A column reading "all" on every row for the rest of time teaches readers to
// stop seeing it — the same reason AUTH and TRACE come and go.
func TestServerLsHidesTheToolsColumnWhenNothingNarrows(t *testing.T) {
	seedCatalog(t)
	if out := mustRun(t, "", "server", "ls"); strings.Contains(out, "TOOLS") {
		t.Errorf("no server narrows its tools, so the column has nothing to say:\n%s", out)
	}
	// The field is still in --json: a program asking what the rule is gets an
	// answer whether or not a human table would have shown a column.
	if got := decodeServerRows(t, mustRun(t, "", "server", "ls", "--json"))["fs"].ToolRule; got == nil || got.Rule != "all" {
		t.Errorf("fs rule = %+v, want the all state reported explicitly", got)
	}
}

// A name no cached tool matches lets nothing through, and after the one
// warning at write time nothing else ever mentions it again.
func TestServerViewsMarkAnUnknownRuleName(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "server", "tool", "allow", "fs", "--only", "read_file,reed_file")

	rule := decodeServerRows(t, mustRun(t, "", "server", "ls", "--json"))["fs"].ToolRule
	if rule == nil || len(rule.Unknown) != 1 || rule.Unknown[0] != "reed_file" {
		t.Fatalf("fs rule = %+v, want reed_file reported unknown", rule)
	}
	out := mustRun(t, "", "server", "ls")
	if !strings.Contains(out, "only 2/2 !") || !strings.Contains(out, "server inspect fs") {
		t.Errorf("the listing must mark the row and name the command that explains it:\n%s", out)
	}
	// The names themselves are in the detail view: what has to be re-read to
	// find the typo is the name, and the listing has no room for it.
	if out := mustRun(t, "", "server", "inspect", "fs"); !strings.Contains(out, "!reed_file") {
		t.Errorf("inspect must mark the unknown name against the name:\n%s", out)
	}
}

// inspect states the rule on every report, including "all". The absence of a
// rule is precisely what a missing line cannot express, and it is the question
// the reader came with.
func TestServerInspectSpellsOutTheToolRule(t *testing.T) {
	seedCatalog(t)
	if out := mustRun(t, "", "server", "inspect", "fs"); !strings.Contains(out, "every tool the server offers") {
		t.Errorf("inspect must say a server with no rule offers everything:\n%s", out)
	}
	mustRun(t, "", "server", "tool", "allow", "fs", "--none")
	if out := mustRun(t, "", "server", "inspect", "fs"); !strings.Contains(out, "exposes nothing") {
		t.Errorf("inspect must spell out the block-all state:\n%s", out)
	}
	mustRun(t, "", "server", "tool", "allow", "fs", "--only", "read_file")
	out := mustRun(t, "", "server", "inspect", "fs")
	if !strings.Contains(out, "only (1 of 2)") || !strings.Contains(out, "read_file") {
		t.Errorf("inspect must name the tools the rule allows, against the total:\n%s", out)
	}
}

// A rule the operator has not yet been able to check against anything must not
// borrow a denominator it does not have: "1 of 0" reads as a smaller catalog,
// not as the absence of one.
func TestServerRuleWithoutACachedCatalogClaimsNoTotal(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "server", "add", "cold", "--cmd", "cold-server")
	mustRun(t, "", "server", "tool", "allow", "cold", "--only", "whatever")

	if got := decodeServerRows(t, mustRun(t, "", "server", "ls", "--json"))["cold"].ToolRule; got.Cached != 0 || got.Unknown != nil {
		t.Errorf("cold rule = %+v, want no total and no spelling verdict", got)
	}
	if out := mustRun(t, "", "server", "ls"); !strings.Contains(out, "only 1/?") {
		t.Errorf("the cell must not invent a total:\n%s", out)
	}
	if out := mustRun(t, "", "server", "inspect", "cold"); !strings.Contains(out, "nothing cached") {
		t.Errorf("inspect must say why there is no total:\n%s", out)
	}
}
