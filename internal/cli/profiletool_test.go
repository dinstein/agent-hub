package cli

import (
	"strings"
	"testing"
)

func profileToolRows(t *testing.T, args ...string) map[string]ToolRow {
	t.Helper()
	rows := decodeToolRowsFromCLI(t, append([]string{"profile", "tool", "ls"}, args...)...)
	byName := make(map[string]ToolRow, len(rows))
	for _, r := range rows {
		key := r.Name
		if key == "" {
			key = r.Server + "/" + r.RawName // a pending row has no exposed name
		}
		byName[key] = r
	}
	return byName
}

// The point of the listing: the two layers INTERSECTED. Either one read alone
// answers a different question, and joining them by hand per tool is what this
// exists to stop.
func TestProfileToolLsIntersectsBothLayers(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "server", "tool", "allow", "fs", "--only", "read_file")
	mustRun(t, "", "profile", "create", "work", "--servers", "fs,git")
	mustRun(t, "", "profile", "tool", "allow", "work", "git", "--none")

	rows := profileToolRows(t, "work", "--json")
	if _, ok := rows["fs__read_file"]; !ok || len(rows) != 1 {
		t.Fatalf("the profile lets exactly fs__read_file through, got %+v", rows)
	}

	// --all says which layer took each of the others, because the repairs
	// differ: one is a server rule, one is this profile's own.
	all := profileToolRows(t, "work", "--all", "--json")
	if got := all["fs__write_file"]; got.State != toolStateBlocked || got.BlockedBy != blockedByGlobal {
		t.Errorf("fs__write_file = %+v, want blocked by the global layer", got)
	}
	if got := all["git__log"]; got.State != toolStateBlocked || got.BlockedBy != blockedByProfileTools {
		t.Errorf("git__log = %+v, want blocked by the profile's own allow list", got)
	}
	out := mustRun(t, "", "profile", "tool", "ls", "work", "--all")
	if !strings.Contains(out, "BY") || !strings.Contains(out, blockedByProfileTools) {
		t.Errorf("the table must name the layer that blocked:\n%s", out)
	}
}

// A profile narrows on two axes and they need different repairs: put the
// server back, or widen the selector. A listing that reported both as "the
// profile" would send the reader to the wrong command.
func TestProfileToolLsSeparatesTheTwoWaysAProfileNarrows(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "profile", "create", "work", "--servers", "fs")

	all := profileToolRows(t, "work", "--all", "--json")
	if got := all["git__log"]; got.State != toolStateBlocked || got.BlockedBy != blockedByProfileServers {
		t.Errorf("git__log = %+v, want blocked because the profile excludes git", got)
	}
	if got := all["fs__read_file"]; got.State != toolStateOn || got.BlockedBy != "" {
		t.Errorf("fs__read_file = %+v, want offered with no layer named", got)
	}
}

// The same misspelling symptom the global layer has, one layer down: a name
// the selector holds and no catalog has lets nothing through, and the write
// warned about it exactly once.
func TestProfileToolLsReportsAMisspelledSelectorEntry(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "profile", "create", "work")
	mustRun(t, "", "profile", "tool", "allow", "work", "fs", "--only", "read_file,reed_file")

	rows := profileToolRows(t, "work", "--json")
	got, ok := rows["fs/reed_file"]
	if !ok || got.State != toolStatePending || got.BlockedBy != blockedByProfileTools {
		t.Fatalf("reed_file = %+v (present=%v), want a pending row naming the profile", got, ok)
	}
	if !strings.Contains(got.Description, "profile") {
		t.Errorf("the row must say which allow list named it, got %q", got.Description)
	}
}

// The narrowing runs the other way too: a profile cannot widen what the
// machine already took away, and the listing must not suggest it can.
func TestProfileToolLsCannotWidenTheGlobalLayer(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "server", "tool", "allow", "fs", "--none")
	mustRun(t, "", "profile", "create", "work")
	mustRun(t, "", "profile", "tool", "allow", "work", "fs", "--all")

	for name, row := range profileToolRows(t, "work", "fs", "--all", "--json") {
		if row.State != toolStateBlocked || row.BlockedBy != blockedByGlobal {
			t.Errorf("%s = %+v, want blocked by the machine-wide rule", name, row)
		}
	}
}

// Both altitudes take the same flags and answer in the same shape — that is
// what makes what is learned about one transfer to the other.
func TestProfileToolLsSharesTheServerListingsFlags(t *testing.T) {
	seedCatalog(t)
	mustRun(t, "", "profile", "create", "work")

	rows := profileToolRows(t, "work", "--search", "commit log", "--json")
	if len(rows) == 0 {
		t.Fatalf("--search must rank the same catalog, got nothing")
	}
	if got := rows["git__log"]; got.Rank != 1 {
		t.Errorf("git__log = %+v, want the best match for its own description", got)
	}
	// The server argument narrows the listing at both altitudes.
	if rows := profileToolRows(t, "work", "fs", "--json"); len(rows) != 2 {
		t.Errorf("the server argument must narrow the listing, got %+v", rows)
	}
}

// A mistyped profile is told so. At runtime the same name fail-closes to an
// empty scope, which is right for a session that must not widen and useless to
// a reader — a correct listing of nothing looks exactly like an empty profile.
func TestProfileToolLsRefusesAnUnknownProfile(t *testing.T) {
	seedCatalog(t)
	code, _, stderr := runCLI(t, "", "profile", "tool", "ls", "nope")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want not-found; stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "profile ls") {
		t.Errorf("the error must point at the listing of real profiles, got %s", stderr)
	}
}
