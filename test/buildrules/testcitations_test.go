package buildrules

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// testDecl matches a top-level test, fuzz or benchmark declaration.
	testDecl = regexp.MustCompile(`(?m)^func ((?:Test|Fuzz|Benchmark)[A-Za-z0-9_]*)\s*\(`)
	// liveTestCitation matches a comment naming a test AS THE THING THAT
	// CURRENTLY GUARANTEES SOMETHING: a name followed by a verb in the
	// present tense. See the discrimination note on the test below — this
	// shape, and not a bare mention, is what makes the check clean.
	//
	// The verb may sit up to three words after the name, because prose puts
	// them there: "TestEveryEventKindIsDocumented next door proves ..." cited
	// a test that code generation had replaced, and an adjacency rule read it
	// as prose. The gap is bounded and lazy so a match cannot wander into the
	// next sentence, and PAST tense stays excluded — "the test was called
	// TestX and asserted ..." is a historical record this must keep letting
	// through, and two of them are load-bearing prose in this package.
	//
	// Writing that example is why this file now skips itself below: a
	// sentence describing the shape IS the shape. The gap is bounded and lazy so the
	// match cannot wander into the next sentence, and PAST tense stays
	// excluded — "the test was called TestX and asserted ..." is a historical
	// record this must keep letting through, and two of them are load-bearing
	// prose in this very package.
	liveTestCitation = regexp.MustCompile(
		`\b((?:Test|Fuzz|Benchmark)[A-Z][A-Za-z0-9_]*)(?:\s+\S+){0,3}?\s+` +
			`(?:pins|asserts|proves|covers|checks|fails|walks|drives|documents)\b`)
)

// TestCitedTestsExist fails when a comment credits a guarantee to a test that
// is not in the tree.
//
// docnames_test.go's own prose names the hazard and stops one step short of
// covering it: "comments here cite test names as evidence — 'pinned by TestX'
// — and a citation that resolves to nothing sends the next reader looking for
// a guarantee under a name that was never there." It checks that a test's doc
// comment opens with its OWN name. Nothing checked the names cited elsewhere,
// and renaming a test does not touch the comments that point at it.
//
// One was stale when this was written, and it had gone stale the same day:
// 80b3195 gave the two paste paths one shared transport-spelling table and
// added TestNormalizeStdinAgreesWithTheCatalogOnSpellings to hold them
// together, while normalizeStdin's doc comment went on crediting
// "TestNormalizeStdinTransportSpellings" — a name that never existed — with
// pinning a divergence the same commit had just removed.
//
// WHY THE VERB, and why not every mention of a Test name. A bare mention is
// three other things far more often than it is a citation: a type or field
// whose name begins with Test (ctlapi's TestConn, its TestDeps, the GUI Hub's
// TestServer), a glob naming a family (oauthflow's "see TestSSRF*"), a
// placeholder standing in for any test at all (docnames_test's own "TestX"),
// and a deliberately HISTORICAL reference — internal/daemon/oauth_test.go
// explains at length that "the test was called TestRefresherSingleflight",
// about a test that was deleted on purpose. Matching those would take an
// allowlist that grows every time someone writes a sentence about the past.
//
// Requiring a present-tense verb keeps all five out by construction: the
// pattern reads "X pins", "X asserts", "X covers". Measured over the tree it
// matches 281 citations with no exemptions at all.
//
// The cost is recall, and it is worth naming: a citation phrased some other
// way ("pinned by X", "see X") is not checked, because the passive forms in
// this tree are three occurrences of which one is the TestX placeholder — an
// exemption list longer than the thing it guards. This test catches the shape
// that carries the claim, not every shape that carries the name.
func TestCitedTestsExist(t *testing.T) {
	root := repoRoot(t)
	sources := allGoSources(t, root)

	declared := map[string]bool{}
	for _, src := range sources {
		for _, m := range testDecl.FindAllStringSubmatch(src, -1) {
			declared[m[1]] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no test declarations; the walk is wrong, not the tree")
	}

	for rel, src := range sources {
		if strings.HasSuffix(rel, filepath.Join("test", "buildrules", "testcitations_test.go")) {
			continue // the examples above are descriptions of citations, not citations
		}
		for _, block := range commentBlocks(src) {
			for _, m := range liveTestCitation.FindAllStringSubmatch(block.text, -1) {
				if declared[m[1]] {
					continue
				}
				t.Errorf("%s:%d credits %q with a guarantee, and no such test exists.\n"+
					"A citation that resolves to nothing is worse than none: it sends the next "+
					"reader looking for a promise under a name that was never there. Point it at "+
					"the test that actually holds the line, or drop the sentence.",
					rel, block.line, m[1])
			}
		}
	}
}

// commentBlock is a run of consecutive comment lines, flattened to one string.
type commentBlock struct {
	line int // 1-indexed line the run starts on
	text string
}

// commentBlocks joins each run of consecutive comment lines into one string.
//
// Scanning line by line looked equivalent and is not: a doc comment wraps at
// 80 columns, so the name and the verb that makes it a citation land on
// different lines often enough that the first version of this check passed
// its own probe — the very sentence it was written for reads
// "...and TestNormalize... / // drives the table's whole input set...".
func commentBlocks(src string) []commentBlock {
	var out []commentBlock
	var cur []string
	start := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, commentBlock{line: start, text: strings.Join(cur, " ")})
			cur = nil
		}
	}
	for i, line := range strings.Split(src, "\n") {
		c := commentOf(line)
		if c == "" {
			flush()
			continue
		}
		if len(cur) == 0 {
			start = i + 1
		}
		cur = append(cur, strings.TrimSpace(c))
	}
	flush()
	return out
}
