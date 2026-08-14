package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	// rulingCite matches a citation of a ruling from the original design
	// document, in both spellings the tree uses: bare ("ruling #8", "rulings
	// #17") and appendix-qualified ("A.1 #8", "appendix A.6 #2").
	rulingCite = regexp.MustCompile(`(?i)\b(?:appendix )?(A\.[0-9]+) #([0-9]+)|\brulings? #([0-9]+)`)

	// registryRow matches a row of docs/decisions/README.md's table and captures its
	// first cell, which holds every spelling that row covers.
	registryRow = regexp.MustCompile("^\\| (`[^|]+`) \\|")

	// backtickedID pulls one id out of that cell: `#6`, `A.1 #6`.
	backtickedID = regexp.MustCompile("`(A\\.[0-9]+ )?#([0-9]+)`")

	// taskNumber matches a milestone task number, which is not a ruling and
	// is not citable. Bounded on the left so a version or a hostname cannot
	// look like one.
	taskNumber = regexp.MustCompile(`(?i)\b(M[0-9](?:\.[0-9])?-[0-9]+)\b`)
)

// TestHistoricalRulingIdsResolve fails when a comment cites a ruling number
// that docs/decisions/README.md does not register.
//
// The ids come from the original design document, which is not in this
// repository. Sixty-odd comments cite them anyway, and the reason to keep them
// is real: a ruling number does not move when a section is renumbered, so it is
// a better anchor than "§7 item 3" — but only while something can resolve it.
// §8 is that something, and this check is what keeps the table and the tree from
// drifting apart in the direction that matters: a citation nobody can look up.
//
// WHAT THIS CHECKS. Every cited id is registered, in the spelling it was cited
// in — `#6` and `A.1 #6` are one ruling, and §8 lists both in the same row
// precisely so either spelling resolves. It also fails on a milestone task
// number (`M0-7`, `M1-3`), which is not a ruling at all: it names a unit of work
// in a plan that is equally absent, so it dates the sentence around it while
// adding nothing a reader can act on. Eleven of those were removed the round
// this test was written; this is what stops the twelfth.
//
// WHAT IT DOES NOT CHECK. Whether the row still describes the ruling correctly,
// or whether the document it points at still holds the rule. Both are review
// questions — the same limit TestCanonicalCitationsResolve documents, for the
// same reason.
//
// It deliberately does NOT fail on a registered row that nobody cites. Rows
// exist to be looked up by someone reading old material, and a ruling whose
// last citation was just rewritten is exactly the entry that reader needs.
func TestHistoricalRulingIdsResolve(t *testing.T) {
	root := repoRoot(t)
	registered := registeredRulingIDs(t, root)
	if len(registered) < 10 {
		t.Fatalf("parsed %d ids out of docs/decisions/README.md; the table shape must have changed", len(registered))
	}

	cited := 0
	for _, rel := range citableFiles(t, root) {
		if rel == filepath.Join("docs", "decisions", "README.md") {
			continue // the registry is not a citation of itself
		}
		if rel == filepath.Join("test", "buildrules", "rulingids_test.go") {
			continue // the patterns above are not citations
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range rulingCite.FindAllStringSubmatch(line, -1) {
				id := rulingKey(m[1], m[2])
				if m[3] != "" { // bare "ruling #17"
					id = rulingKey("", m[3])
				}
				cited++
				if !registered[id] {
					t.Errorf("%s:%d cites ruling %s, which docs/decisions/README.md does not register.\n"+
						"Add a row saying what it ruled and which document owns that rule now — "+
						"an id nobody can look up is worse than no id, because it reads as authority.",
						rel, i+1, id)
				}
			}
			if m := taskNumber.FindStringSubmatch(line); m != nil {
				t.Errorf("%s:%d cites the milestone task %s, which is not a ruling and is not citable "+
					"(docs/decisions/README.md).\nThe plan it names is not in this repository; cite the module doc "+
					"for the package, or say the thing the task number was standing in for.",
					rel, i+1, m[1])
			}
		}
	}
	if cited < 30 {
		t.Fatalf("found only %d ruling citations; the pattern stopped matching the tree's spellings", cited)
	}
	t.Logf("checked %d citations against %d registered ids", cited, len(registered))
}

// registeredRulingIDs reads the id spellings out of docs/decisions/README.md's table.
func registeredRulingIDs(t *testing.T, root string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "docs", "decisions", "README.md"))
	if err != nil {
		t.Fatalf("reading docs/decisions/README.md: %v", err)
	}
	out := map[string]bool{}
	inSection := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "## ") {
			inSection = strings.HasPrefix(line, "## Historical ruling ids")
			continue
		}
		if !inSection {
			continue
		}
		m := registryRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, id := range backtickedID.FindAllStringSubmatch(m[1], -1) {
			out[rulingKey(id[1], id[2])] = true
		}
	}
	// A.6 #N is decision 000N by the equivalence the registry states, so the
	// decision files register their appendix spelling without a row each. The
	// count comes from the directory itself: a new decision's appendix
	// spelling resolves the moment its file lands.
	for i := 1; i <= countDecisionFiles(t, root); i++ {
		out[rulingKey("A.6", strconv.Itoa(i))] = true
	}
	return out
}

// rulingKey normalizes the two spellings to one lookup key: "#8" when the
// appendix is absent, "A.1 #8" when it is not.
func rulingKey(appendix, number string) string {
	appendix = strings.ToUpper(strings.TrimSpace(appendix))
	if appendix == "" {
		return "#" + number
	}
	return appendix + " #" + number
}

// countDecisionFiles counts the numbered records in docs/decisions/, which is
// what makes the A.6 equivalence self-maintaining.
func countDecisionFiles(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "docs", "decisions"))
	if err != nil {
		t.Fatalf("reading docs/decisions: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && decisionFileName.MatchString(e.Name()) {
			n++
		}
	}
	return n
}

// decisionFileName matches a numbered decision record.
var decisionFileName = regexp.MustCompile(`^[0-9]{4}-.*\.md$`)
