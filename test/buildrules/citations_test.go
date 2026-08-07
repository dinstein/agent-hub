package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// canonicalCite matches a citation of canonical.md by section number, in every
// spelling the tree uses: "canonical.md §2", "docs/canonical.md §5c",
// "CANONICAL §6", with an optional item suffix written "#4", "item 3" or
// "rule 2" — §2's four dependency constraints are cited in that last spelling.
var canonicalCite = regexp.MustCompile(`(?i)(?:docs/)?canonical(?:\.md)? §([0-9]+[a-z]?)(?:\s*(?:#|item |rule )\s*([0-9]+))?`)

// canonicalSection matches a top-level section heading in canonical.md and
// captures its number, including the lettered ones (5b, 5c, 5d).
var canonicalSection = regexp.MustCompile(`^## ([0-9]+[a-z]?)\.\s`)

// numberedItem matches an ordered-list item at the start of a line, which is
// what "§7 #4" and "§2 rule 3" address inside a section.
var numberedItem = regexp.MustCompile(`^\s*([0-9]+)\. `)

// TestCanonicalCitationsResolve fails when the tree cites a canonical.md
// section, or a numbered item inside one, that does not exist.
//
// canonical.md opens by declaring its section numbers an interface, because
// they are: comments cite them by number in over a hundred places, and a
// citation is the one kind of cross-reference that goes stale WITHOUT the file
// it points at changing shape — someone renumbers a section, or deletes one,
// and every reference now lands on a neighbouring ruling. That is worse than
// landing on nothing: a reader who finds the wrong rule under the right number
// has no reason to doubt it.
//
// WHAT THIS CHECKS, AND WHAT IT DOES NOT. Existence of the section, and of the
// numbered item when one is named. It CANNOT check that the section actually
// says what the citing comment claims — the round that added this test found
// eleven citations pointing at real sections for rules those sections had never
// held (a package's row in a directory tree that no longer exists, quoted
// requirements inherited from a design draft that is not in this repository).
// Every one of those passed a numbering check and had to be read to be found.
// Semantic drift stays a review question; say so rather than implying a
// guarantee that is not here.
//
// Historical ruling ids ("ruling #7", "A.6 #3") are checked separately, by
// TestHistoricalRulingIdsResolve, against the registry in canonical.md §8.
func TestCanonicalCitationsResolve(t *testing.T) {
	root := repoRoot(t)
	sections := canonicalSections(t, root)
	if len(sections) < 5 {
		t.Fatalf("parsed %d sections out of canonical.md; the heading shape must have changed", len(sections))
	}

	files := citableFiles(t, root)
	if len(files) == 0 {
		t.Fatal("found no files to scan; the walk or the root is wrong")
	}

	cited := 0
	for _, rel := range files {
		if rel == filepath.Join("docs", "canonical.md") {
			continue // its own "§2 rule 3" style prose is the definition, not a citation
		}
		if rel == filepath.Join("test", "buildrules", "citations_test.go") {
			continue // the patterns above are not citations
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range canonicalCite.FindAllStringSubmatch(line, -1) {
				cited++
				sec, item := strings.ToLower(m[1]), m[2]
				items, ok := sections[sec]
				if !ok {
					t.Errorf("%s:%d cites canonical.md §%s, which has no such section.\n"+
						"Point it at the section that now holds the rule — and if the rule moved out of "+
						"canonical.md entirely, cite the doc that owns it instead.",
						rel, i+1, sec)
					continue
				}
				if item != "" && !items[item] {
					t.Errorf("%s:%d cites canonical.md §%s #%s, but §%s has no item %s.\n"+
						"Items are cited by number, so an off-by-one lands the reader on a different ruling.",
						rel, i+1, sec, item, sec, item)
				}
			}
		}
	}
	if cited < 50 {
		t.Fatalf("found only %d citations; the pattern stopped matching the tree's spellings", cited)
	}
	t.Logf("checked %d citations against %d sections", cited, len(sections))
}

// canonicalSections maps each section number in canonical.md to the set of
// ordered-list item numbers it contains.
func canonicalSections(t *testing.T, root string) map[string]map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "docs", "canonical.md"))
	if err != nil {
		t.Fatalf("reading canonical.md: %v", err)
	}
	out := map[string]map[string]bool{}
	current := ""
	fenced := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := canonicalSection.FindStringSubmatch(line); m != nil {
			current = strings.ToLower(m[1])
			out[current] = map[string]bool{}
			continue
		}
		if current == "" {
			continue
		}
		if m := numberedItem.FindStringSubmatch(line); m != nil {
			out[current][m[1]] = true
		}
	}
	return out
}

// citableFiles lists the files whose comments and prose may cite canonical.md.
func citableFiles(t *testing.T, root string) []string {
	t.Helper()
	return walkRepoFiles(t, root, "citable files", func(name string) bool {
		switch filepath.Ext(name) {
		case ".go", ".md", ".ts", ".yml", ".yaml":
			return true
		}
		return false
	})
}
