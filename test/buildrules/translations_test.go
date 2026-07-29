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

// translationPairs are the documents kept in two languages. README sits at the
// repo root with a suffixed name; everything under docs/ mirrors its path
// beneath docs/zh-CN/.
//
// The pairing is derived rather than listed, so a NEW English document under
// docs/ is picked up the moment it exists — which is the direction that
// matters. A translation that was never started is exactly the case a
// hand-maintained list would omit.
func translationPairs(t *testing.T, root string) [][2]string {
	t.Helper()
	var out [][2]string
	docs := filepath.Join(root, "docs")
	err := filepath.WalkDir(docs, func(path string, d os.DirEntry, err error) error {
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
		rel, err := filepath.Rel(docs, path)
		if err != nil {
			return err
		}
		out = append(out, [2]string{
			filepath.Join("docs", rel),
			filepath.Join("docs", "zh-CN", rel),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking docs/: %v", err)
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
// canonical.md had exactly this. It carried 20 headings against the
// translation's 19, and the missing one was §3's ruling that `server add` and
// `server enable` stay separate primitives. That is the worst possible file for
// the failure, because canonical.md is where rules that must NOT change live —
// an absent rule there does not read as untranslated, it reads as a rule that
// was never made.
//
// WHAT THIS CHECKS, AND WHAT IT DOES NOT. It compares the SEQUENCE OF HEADING
// LEVELS, nothing else. Heading text is not compared — it is translated, so of
// course it differs — and neither is anything below heading level.
//
// It therefore does NOT catch drift inside a section. The same round that
// added this test also restored a dropped bullet in canonical.md §3, where the
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
				t.Errorf("%s has no translation at %s", en, zh)
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
