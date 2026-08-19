package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// headingLine matches an ATX heading and captures its level.
var headingLine = regexp.MustCompile(`^(#{1,6})\s+\S`)

// contributorOnlyDocs are the documents deliberately kept in English only,
// each with the reason it stays that way. A trailing slash covers a subtree.
//
// The line is drawn by WHAT A DOCUMENT TRACKS, not by who reads it. Everything
// listed here moves when the code moves, so a translation of it is a second
// file that every behaviour change has to remember — and the copy that gets
// forgotten is indistinguishable from a copy that is current. docs/conventions.md is
// the sharpest case: it is where rules that must NOT change live, so a rule
// updated on one side only does not read as a stale translation, it reads as a
// rule that was never made. The mirror of everything below came to some 5,800
// lines, and the check in this file can only ever prove that their headings
// agree.
//
// What stays translated is the surface that describes the product rather than
// the tree: the root README and docs/guide.md (how to use it), plus
// docs/architecture.md (how the system is carved up), which moves far more
// slowly than the packages beneath it.
var contributorOnlyDocs = []struct{ Path, Why string }{
	{"docs/README.md", "the contributor doc index, which itself lists what is English-only"},
	{"docs/conventions.md", "the conventions themselves; a rule edited on one side only does not read as a stale translation, it reads as a rule that was never made"},
	{"docs/decisions/", "one settled question per file; a half-translated decision record is one nobody can cite"},
	{"docs/flows.md", "runtime sequences and failure branches, restated whenever a flow changes"},
	{"docs/subsystems/", "per-seam invariants and the gaps recorded beside them; each moves with its packages"},
	{"docs/status/", "snapshots of one platform, one protocol revision or one provider surface; each is rewritten whenever that state moves"},
}

// translatedRoots are the directories walked for English documents. Each
// mirrors its translations at <root>/zh-CN/<the same relative path>.
var translatedRoots = []string{"docs"}

// englishOnly reports whether rel is declared contributor-only.
func englishOnly(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, d := range contributorOnlyDocs {
		if strings.HasSuffix(d.Path, "/") {
			if strings.HasPrefix(rel, d.Path) {
				return true
			}
			continue
		}
		if rel == d.Path {
			return true
		}
	}
	return false
}

// translationPairs are the documents kept in two languages. README sits at the
// repo root with a suffixed name; everything under a translatedRoots directory
// mirrors its path beneath that directory's zh-CN/.
//
// The pairing is derived rather than listed, so a NEW English document under
// one of those roots is picked up the moment it exists — which is the
// direction that matters. A translation that was never started is exactly the
// case a hand-maintained list would omit. What IS listed is the exemption
// (contributorOnlyDocs), because declining to translate something is a
// decision that should have to be written down.
func translationPairs(t *testing.T, root string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, top := range translatedRoots {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "zh-CN" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			rel, err := filepath.Rel(base, path)
			if err != nil {
				return err
			}
			if englishOnly(filepath.Join(top, rel)) {
				return nil
			}
			out = append(out, [2]string{
				filepath.Join(top, rel),
				filepath.Join(top, "zh-CN", rel),
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s/: %v", top, err)
		}
	}
	out = append(out, [2]string{"README.md", "README.zh-CN.md"})
	return out
}

// TestTranslationsHaveTheSameSectionStructure fails when an English document
// and its zh-CN counterpart do not agree on their heading skeleton.
//
// The two languages are maintained in lockstep here, and the failure this
// catches is a section that only ever got written on one side. It is quiet in
// a way a mistranslation is not: nothing looks wrong, the document simply says
// less, and only a reader who has both open notices.
//
// docs/conventions.md had exactly this while it was still translated. It carried 20
// headings against the translation's 19, and the missing one was §3's ruling
// that `server add` and `server enable` stay separate primitives — an absent
// rule in that file does not read as untranslated, it reads as a rule that was
// never made. That file is now English-only for the same reason it was the
// worst offender; see contributorOnlyDocs above.
//
// WHAT THIS CHECKS, AND WHAT IT DOES NOT. It compares the SEQUENCE OF HEADING
// LEVELS, nothing else. Heading text is not compared — it is translated, so of
// course it differs — and neither is anything below heading level.
//
// It therefore does NOT catch drift inside a section. The same round that
// added this test also restored a dropped bullet in docs/conventions.md#command-naming, where the
// translation had folded a whole bullet into a parenthetical on another one and
// lost the retired `scope` group's command mapping. Nine bullets against eight,
// and this check was blind to it. Comparing bullet counts was considered and
// rejected: line wrapping and ordinary translation style move them around, so
// it would fail constantly for no defect and be deleted. Within-section
// completeness stays a review question — say so rather than implying a
// guarantee that is not here.
func TestTranslationsHaveTheSameSectionStructure(t *testing.T) {
	root := repoRoot(t)
	pairs := translationPairs(t, root)
	if len(pairs) < 2 {
		t.Fatalf("found %d translation pairs; the walk or the root is wrong", len(pairs))
	}

	for _, p := range pairs {
		en, zh := p[0], p[1]
		enLevels, err := headingLevels(filepath.Join(root, en))
		if err != nil {
			t.Fatalf("reading %s: %v", en, err)
		}
		zhLevels, err := headingLevels(filepath.Join(root, zh))
		if err != nil {
			if os.IsNotExist(err) {
				t.Errorf("%s has no translation at %s.\n"+
					"Write the translation, or — if this document tracks the code closely enough "+
					"that a mirror would rot — declare it in contributorOnlyDocs with the reason.",
					en, zh)
				continue
			}
			t.Fatalf("reading %s: %v", zh, err)
		}
		if len(enLevels) != len(zhLevels) {
			t.Errorf("%s has %d headings, %s has %d — a section exists on only one side",
				en, len(enLevels), zh, len(zhLevels))
			continue
		}
		for i := range enLevels {
			if enLevels[i] != zhLevels[i] {
				t.Errorf("%s and %s disagree at heading %d: level %d vs %d "+
					"(the nesting was changed on one side)",
					en, zh, i+1, enLevels[i], zhLevels[i])
				break
			}
		}
	}
}

// TestContributorOnlyDocsMatchTheTree keeps the exemption list honest in both
// directions.
//
// An entry naming a document that no longer exists is the dangerous half: the
// list goes on excusing a name nobody writes any more, and the day a NEW
// document lands at that path it is born untranslated with the exemption
// already in place — silently, because the exemption predates the file.
//
// A lingering zh-CN copy of an exempted document is the other half. It reads
// exactly like a maintained translation, while nothing at all checks it: the
// skeleton test skips the pair, so the file is free to describe a version of
// the rules that stopped being true whenever it was last touched. Deleting a
// translation has to mean deleting the file.
func TestContributorOnlyDocsMatchTheTree(t *testing.T) {
	root := repoRoot(t)

	for _, d := range contributorOnlyDocs {
		if d.Why == "" {
			t.Errorf("%s is exempted from translation with no reason given", d.Path)
		}
		if strings.HasSuffix(d.Path, "/") {
			entries, err := os.ReadDir(filepath.Join(root, d.Path))
			if err != nil {
				t.Errorf("contributorOnlyDocs names the subtree %q, which does not exist: %v", d.Path, err)
				continue
			}
			found := false
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("contributorOnlyDocs names the subtree %q, which holds no markdown — "+
					"drop the entry rather than leaving it to exempt whatever lands there next", d.Path)
			}
			continue
		}
		if !exists(root, d.Path) {
			t.Errorf("contributorOnlyDocs exempts %q from translation, but that document does not exist. "+
				"Drop the entry: left in place, it exempts the next document to land at that path.", d.Path)
		}
	}

	for _, top := range translatedRoots {
		zhRoot := filepath.Join(root, top, "zh-CN")
		if _, err := os.Stat(zhRoot); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(zhRoot, func(path string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				return nil
			}
			rel, err := filepath.Rel(zhRoot, path)
			if err != nil {
				return err
			}
			if englishOnly(filepath.Join(top, rel)) {
				t.Errorf("%s/zh-CN/%s translates a document declared English-only. "+
					"Nothing checks that file, so delete it — an unchecked translation of the rules "+
					"is worse than none.", top, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s/zh-CN: %v", top, err)
		}
	}
}

// headingLevels returns the heading levels of a markdown file in order,
// ignoring anything inside a fenced code block — a shell transcript full of
// `# comment` lines is not a section.
func headingLevels(path string) ([]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []int
	fenced := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := headingLine.FindStringSubmatch(line); m != nil {
			out = append(out, len(m[1]))
		}
	}
	return out, nil
}

// listItem and tableRow are the two structures whose COUNT carries meaning in
// these documents: a bullet is one rule and a table row is one entry, so a
// translation with fewer of either has dropped something a reader on that side
// will never learn is missing.
var (
	listItem = regexp.MustCompile(`^\s*(?:[-*+]|[0-9]+\.)\s+\S`)
	tableRow = regexp.MustCompile(`^\s*\|`)
)

// TestTranslationsKeepEachSectionsStructure compares the two sides SECTION BY
// SECTION, not just heading by heading.
//
// The skeleton test above proves the headings agree and stops there, which is
// exactly the gap the nightly-tidy skill names: "docs/zh-CN/ is checked for its
// heading skeleton and nothing below it, so a dropped bullet survives". A
// dropped bullet is a dropped RULE, and it is invisible from either side alone
// — the translated page reads as complete, because a missing sentence leaves no
// hole.
//
// Counts, not content: nothing here can tell whether a bullet says the same
// thing, and prose length is not comparable across languages. What it can say
// is that both sides carry the same number of rules in the same section, which
// is the property a hand-edited mirror actually loses.
//
// A translator who deliberately merges two bullets into one has to say so by
// merging both sides. That is the intended cost: the alternative is a check
// that agrees with any edit, which is the state this replaces.
func TestTranslationsKeepEachSectionsStructure(t *testing.T) {
	root := repoRoot(t)
	pairs := translationPairs(t, root)
	if len(pairs) < 2 {
		t.Fatalf("found %d translation pairs; the walk or the root is wrong", len(pairs))
	}
	compared := 0
	for _, p := range pairs {
		enH, enB, err := sectionBodies(filepath.Join(root, p[0]))
		if err != nil {
			t.Fatalf("reading %s: %v", p[0], err)
		}
		zhH, zhB, err := sectionBodies(filepath.Join(root, p[1]))
		if err != nil {
			if os.IsNotExist(err) {
				continue // the skeleton test above reports a missing translation
			}
			t.Fatalf("reading %s: %v", p[1], err)
		}
		if len(enH) != len(zhH) {
			continue // likewise: a section exists on only one side
		}
		for i := range enB {
			compared++
			enL, enR := countStructures(enB[i])
			zhL, zhR := countStructures(zhB[i])
			switch {
			case enL != zhL:
				t.Errorf("%s and %s disagree under %q: %d list items vs %d.\n"+
					"A bullet is a rule; the side with fewer has dropped one, and a translated page "+
					"missing a sentence reads exactly like a complete one.",
					p[0], p[1], enH[i], enL, zhL)
			case enR != zhR:
				t.Errorf("%s and %s disagree under %q: %d table rows vs %d.\n"+
					"A row is an entry; one side is describing a smaller table than the other.",
					p[0], p[1], enH[i], enR, zhR)
			}
		}
	}
	if compared == 0 {
		t.Fatal("compared no sections; the split is wrong and the rule would pass over nothing")
	}
	t.Logf("compared %d sections across %d translation pairs", compared, len(pairs))
}

// sectionBodies splits a document into headings and the lines beneath each.
// The text before the first heading is one section named "(preamble)", so a
// rule stated up front is compared too.
func sectionBodies(path string) ([]string, [][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	heads := []string{"(preamble)"}
	bodies := [][]string{{}}
	fenced := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if !fenced && headingLine.MatchString(line) {
			heads = append(heads, strings.TrimSpace(strings.TrimLeft(line, "# ")))
			bodies = append(bodies, []string{})
			continue
		}
		if fenced {
			continue // a shell transcript's flags are not the document's bullets
		}
		bodies[len(bodies)-1] = append(bodies[len(bodies)-1], line)
	}
	return heads, bodies, nil
}

// countStructures returns a section's list items and table rows.
func countStructures(body []string) (items, rows int) {
	for _, line := range body {
		switch {
		case listItem.MatchString(line):
			items++
		case tableRow.MatchString(line):
			rows++
		}
	}
	return items, rows
}
