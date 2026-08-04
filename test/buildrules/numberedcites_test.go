package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// numberedDocs are the documents besides canonical.md whose sections are
// addressed by number from elsewhere in the tree. Both are flat: `## N.` and
// nothing below it is numbered, which is what lets a `§N.M` citation be
// rejected outright. docs/mcp-2026-07-28.md is deliberately absent — it does
// have `### 1.1` subsections, so it needs a different rule than this one.
var numberedDocs = []string{"architecture.md", "flows.md"}

// TestNumberedDocCitationsResolve extends to architecture.md and flows.md the
// check TestCanonicalCitationsResolve has always applied to canonical.md.
//
// The reasoning is that test's, unchanged: a section number is the one
// cross-reference that goes stale without the file it points at changing
// shape. Someone renumbers or deletes a section and every reference now lands
// on a neighbouring rule — worse than landing on nothing, because a reader who
// finds the wrong rule under the right number has no reason to doubt it.
//
// It applied to one of the three numbered documents. The tree carries well
// over a hundred `architecture.md §N` citations, most of them from the gate
// chain's own files, and nothing graded any of them. One had been wrong since
// the initial public release: internal/scope's Merge headed its rule list "the
// whole table of 4.1", and architecture.md has never had a §4.1 — that one is
// also why a `§N.M` citation is rejected here rather than merely resolved to
// its parent. Neither of these documents has a numbered subsection, so the
// only thing a decimal can be is a reference to something else.
//
// Same limitation as next door, stated for the same reason: this checks that
// the section EXISTS, never that it says what the citation claims. Semantic
// drift stays a review question.
func TestNumberedDocCitationsResolve(t *testing.T) {
	root := repoRoot(t)
	files := citableFiles(t, root)
	if len(files) == 0 {
		t.Fatal("found no files to scan; the walk or the root is wrong")
	}

	total := 0
	for _, doc := range numberedDocs {
		sections := flatDocSections(t, root, doc)
		if len(sections) < 5 {
			t.Fatalf("parsed %d sections out of %s; its heading shape must have changed", len(sections), doc)
		}
		cite := numberedDocCite(doc)

		for _, rel := range files {
			// The document's own headings are the definition, and this file's
			// patterns are not citations.
			if rel == filepath.Join("docs", doc) ||
				rel == filepath.Join("docs", "zh-CN", doc) ||
				rel == filepath.Join("test", "buildrules", "numberedcites_test.go") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Fatalf("reading %s: %v", rel, err)
			}
			for i, line := range strings.Split(string(data), "\n") {
				for _, m := range cite.FindAllStringSubmatch(line, -1) {
					total++
					if m[2] != "" {
						t.Errorf("%s:%d cites %s §%s.%s, and %s has no numbered subsections.\n"+
							"A decimal here points at a section that cannot exist; name the "+
							"top-level section, or the document that actually carries the rule.",
							rel, i+1, doc, m[1], m[2], doc)
						continue
					}
					if !sections[m[1]] {
						t.Errorf("%s:%d cites %s §%s, which has no such section.\n"+
							"Point it at the section that now holds the rule — and if the rule "+
							"moved out of %s entirely, cite the doc that owns it instead.",
							rel, i+1, doc, m[1], doc)
					}
				}
			}
		}
	}
	if total < 40 {
		t.Fatalf("found only %d citations across %v; the pattern stopped matching the tree's "+
			"spellings, and a scan that reaches nothing agrees with everything", total, numberedDocs)
	}
	t.Logf("checked %d citations across %v", total, numberedDocs)
}

// numberedDocCite matches `<doc> §N` and `<doc> §N.M`, with or without a
// `docs/` prefix or an anchor between the two — the spellings the tree uses.
// The decimal is captured rather than ignored so the caller can reject it.
func numberedDocCite(doc string) *regexp.Regexp {
	stem := regexp.QuoteMeta(strings.TrimSuffix(doc, ".md"))
	return regexp.MustCompile(`(?i)(?:docs/)?(?:zh-CN/)?` + stem +
		`(?:\.md)?(?:#[a-z0-9-]+)?\s*§\s*([0-9]+)(?:\.([0-9]+))?`)
}

// flatDocSections returns the top-level section numbers of a `## N. Title`
// document. Fenced blocks are skipped so a mermaid diagram or a shell
// transcript cannot contribute a heading.
func flatDocSections(t *testing.T, root, doc string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "docs", doc))
	if err != nil {
		t.Fatalf("reading %s: %v", doc, err)
	}
	out := map[string]bool{}
	fenced := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := flatSection.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// flatSection matches `## 7. Scope: …`, capturing the number.
var flatSection = regexp.MustCompile(`^## ([0-9]+)\.\s`)
