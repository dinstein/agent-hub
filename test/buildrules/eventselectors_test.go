package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestEveryEventKindIsSelectable is the third of the three checks
// docs/modules/foundation.md promises, and the one that was missing.
//
// Adding a kind means editing the constant, allKinds, and the published
// table, and then writing it somewhere. Its neighbours grade the constant
// against the table (TestEveryEventKindIsDocumented) and against the emit
// sites (TestEveryEventKindHasAWriter). Both parse `KindX Kind = "wire"`
// declaration lines; neither reads allKinds, so the one edit nothing graded
// was the set that decides what a selector may name.
//
// The gap fails in the direction the vocabulary exists to prevent, and worse
// than the case next door. A kind absent from allKinds is still declared,
// still documented and still WRITTEN — records carrying it accumulate in
// events.jsonl — while KnownKind answers false at every scope, so
// `agenthub events --kind <it>` is refused as a kind that does not exist.
// The neighbouring failure offers a selector that answers "no events"; this
// one denies a kind whose records are on disk.
//
// Membership only, not the (scope, kind) pairing: which scopes a kind belongs
// to is a judgement — `started` is meaningful for a gateway and a daemon and
// meaningless for a server — and a check cannot make it. Belonging to at
// least one scope is not a judgement. A kind in none of them is unreachable
// by every reader, whatever the intended pairing was.
func TestEveryEventKindIsSelectable(t *testing.T) {
	path := filepath.Join(repoRoot(t), "internal", "eventlog", "eventlog.go")
	declared := parseEventKindConsts(t, path)
	if len(declared) == 0 {
		t.Fatal("no Kind constants found; this test asserted nothing")
	}
	registered := parseAllKindsMembers(t, path)
	if len(registered) == 0 {
		t.Fatal("the allKinds map literal parsed empty; if its shape changed, this check " +
			"has to change with it rather than pass vacuously")
	}

	for name, wire := range declared {
		if !registered[name] {
			t.Errorf("internal/eventlog declares %s (%q) but no scope in allKinds lists it.\n"+
				"KnownKind answers false at every scope, so `agenthub events --kind %s` is "+
				"refused as an unknown kind while records carrying it are being written.\n"+
				"Add it to the scope or scopes it belongs to.", name, wire, wire)
		}
	}
}

var (
	// The `var allKinds = map[Scope][]Kind{ … }` literal, up to the line that
	// closes it at column zero. Anchored on the declaration rather than on
	// the identifier alone so a mention of allKinds elsewhere in the file
	// cannot start the capture somewhere meaningless.
	allKindsLiteral = regexp.MustCompile(`(?s)var allKinds = map\[Scope\]\[\]Kind\{.*?\n\}`)
	// A bare KindX inside it. The map holds identifiers, not the string
	// spellings the two neighbouring checks match on, which is exactly why
	// their regexes could not see this list.
	allKindsMember = regexp.MustCompile(`\b(Kind\w+)\b`)
)

func parseAllKindsMembers(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block := allKindsLiteral.Find(data)
	if block == nil {
		t.Fatal("internal/eventlog no longer declares `var allKinds = map[Scope][]Kind{`; " +
			"if the closed set moved, this check must follow it")
	}
	out := map[string]bool{}
	for _, m := range allKindsMember.FindAllSubmatch(block, -1) {
		out[string(m[1])] = true
	}
	return out
}
