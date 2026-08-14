package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// anchorCite matches a citation of a document's section by anchor —
// `docs/model.md#visibility-and-connection-are-two-planes`, with or without a
// `docs/` prefix. This is the spelling that replaced `§N`: an anchor is a
// slugged heading, so it survives a section being moved or renumbered and
// breaks loudly when the heading it names is renamed.
var anchorCite = regexp.MustCompile(`(?:docs/)?((?:\.\./)*(?:zh-CN/|modules/|subsystems/|status/|decisions/)?[a-z0-9][a-z0-9._-]*\.md)#([\p{L}\p{N}][\p{L}\p{N}-]*)`)

// TestDocAnchorsResolve fails when the tree cites a `docs/*.md#anchor` that no
// document answers to.
//
// It is the successor to the numbered-section checks, and it exists for the
// same reason: a cross-reference into prose goes stale without the file it
// points at changing shape. The difference is which edit breaks it. A number
// broke on any INSERTION — add a section and every later citation silently
// lands on its neighbour — so the number was really the section's position.
// An anchor breaks only when the heading itself is reworded, which is an edit
// the person making it can see.
//
// WHAT THIS CHECKS, AND WHAT IT DOES NOT. That the document exists and that
// some heading in it slugs to the cited anchor. It cannot check that the
// section says what the citing comment claims; semantic drift stays a review
// question.
func TestDocAnchorsResolve(t *testing.T) {
	root := repoRoot(t)
	files := citableFiles(t, root)
	if len(files) == 0 {
		t.Fatal("found no files to scan; the walk or the root is wrong")
	}

	anchors := map[string]map[string]bool{} // doc path relative to docs/ -> slugs
	checked := 0

	for _, rel := range files {
		if rel == filepath.Join("test", "buildrules", "docanchors_test.go") {
			continue // the pattern above is not a citation
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		inDocs := strings.HasPrefix(filepath.ToSlash(rel), "docs/")
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range anchorCite.FindAllStringSubmatch(line, -1) {
				doc, anchor := m[1], m[2]
				// Inside docs/, a bare `guide.md#x` is relative to the citing
				// file; `../model.md#x` is spelled with a prefix the pattern
				// does not capture, so only same-directory links are resolved
				// here. Anything under docs/ that names its own directory is
				// checked; the rest is left to the docs/-prefixed spelling
				// that code comments use.
				// A link inside docs/ is relative to the citing file:
				// `../model.md#x` from docs/subsystems/ is docs/model.md#x. A
				// link LABEL is not — `[architecture.md#x](../architecture.md#x)`
				// spells the same citation twice, once without the prefix — so
				// a path that does not resolve relatively is retried against
				// docs/ before it is reported.
				candidates := []string{filepath.ToSlash(filepath.Clean(doc))}
				if inDocs {
					fromHere := filepath.ToSlash(strings.TrimPrefix(
						filepath.Clean(filepath.Join(filepath.Dir(rel), doc)), "docs/"))
					candidates = append([]string{fromHere}, candidates...)
				}
				resolved, found := "", false
				for _, c := range candidates {
					set, ok := anchors[c]
					if !ok {
						var err error
						set, err = headingSlugs(filepath.Join(root, "docs", c))
						if err != nil {
							if !os.IsNotExist(err) {
								t.Fatalf("reading docs/%s: %v", c, err)
							}
							set = nil
						}
						anchors[c] = set
					}
					if resolved == "" && set != nil {
						resolved = c
					}
					if set[anchor] {
						found = true
						break
					}
				}
				checked++
				switch {
				case found:
				case resolved == "":
					t.Errorf("%s:%d cites %s#%s, and no such document exists (tried %v)",
						rel, i+1, doc, anchor, candidates)
				default:
					t.Errorf("%s:%d cites docs/%s#%s, which no heading in that document slugs to.\n"+
						"Point it at the section that now holds the rule — a renamed heading takes its "+
						"anchor with it.", rel, i+1, resolved, anchor)
				}
			}
		}
	}
	if checked < 100 {
		t.Fatalf("resolved only %d anchor citations; the pattern stopped matching the tree's "+
			"spellings, and a scan that reaches nothing agrees with everything", checked)
	}
	t.Logf("checked %d anchor citations across %d documents", checked, len(anchors))
}

// headingSlugs returns the GitHub anchor of every heading in a markdown file.
// Fenced blocks are skipped so a shell transcript's `# comment` is not a
// section.
func headingSlugs(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
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
		if m := headingLine.FindStringSubmatch(line); m != nil {
			out[slugify(strings.TrimSpace(strings.TrimPrefix(line, m[1])))] = true
		}
	}
	return out, nil
}

// slugify reproduces GitHub's heading anchors closely enough to catch rot:
// lowercase, drop everything that is not a letter, a digit, a space, a hyphen
// or an underscore, then spaces become hyphens. Letters are unicode-wide, so a
// translated heading anchors on its own characters the way GitHub renders it.
func slugify(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}
