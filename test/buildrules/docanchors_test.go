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
var anchorCite = regexp.MustCompile(`(?:docs/)?((?:\.\./)*(?:zh-CN/|subsystems/|status/|decisions/)?[a-z0-9][a-z0-9._-]*\.md)(?:#([\p{L}\p{N}][\p{L}\p{N}-]*))?`)

// TestDocReferencesResolve fails when the tree cites a document under docs/
// that does not exist, or an anchor in one that no heading answers to.
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
func TestDocReferencesResolve(t *testing.T) {
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
		for i, raw := range strings.Split(string(data), "\n") {
			// `[windows.md](status/windows.md)` spells one citation twice,
			// and the LABEL is prose: it is written without the prefix on
			// purpose, so checking it reports a break the link does not
			// have. Keep the target, drop the label.
			line := mdLinkLabel.ReplaceAllString(raw, "$1")
			for _, m := range anchorCite.FindAllStringSubmatchIndex(line, -1) {
				if !citationBoundary(line, m[0], m[1]) {
					continue
				}
				doc, anchor := group(line, m, 1), group(line, m, 2)
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
				// An anchorless match is only a citation when it names a
				// directory or carries the docs/ prefix — outside docs/ a
				// bare `rules.md` is a test fixture's filename and `tt.md`
				// is a struct field, and neither is a claim about the tree.
				// Inside docs/ a bare name IS the relative-link spelling, so
				// there it is checked. This is why the anchored half of the
				// rule was the only half that ever ran: the pattern is loose
				// on purpose, and an anchor is what makes a match certain.
				if anchor == "" && !inDocs &&
					!strings.HasPrefix(line[m[0]:m[1]], "docs/") && !strings.Contains(doc, "/") {
					checked++
					continue
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
					if set == nil {
						continue // no such document under docs/
					}
					if resolved == "" {
						resolved = c
					}
					if anchor == "" {
						found = true // the document exists, which is the whole claim
						break
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
					t.Errorf("%s:%d cites %s, and no such document exists (tried %v).\n"+
						"A document that moved takes its citations with it.", rel, i+1, doc, candidates)
				default:
					t.Errorf("%s:%d cites docs/%s#%s, which no heading in that document slugs to.\n"+
						"Point it at the section that now holds the rule — a renamed heading takes its "+
						"anchor with it.", rel, i+1, resolved, anchor)
				}
			}
		}
	}
	if checked < 200 {
		t.Fatalf("resolved only %d document citations; the pattern stopped matching the tree's "+
			"spellings, and a scan that reaches nothing agrees with everything", checked)
	}
	t.Logf("checked %d anchor citations across %d documents", checked, len(anchors))
}

// nestedDocsPrefix matches a docs/ path with a second docs/ inside it —
// `docs/subsystems/docs/subsystems/controlplane.md`. The character class
// deliberately excludes the space, so two separate citations on one line
// ("docs/architecture.md, docs/model.md") are not one match.
var nestedDocsPrefix = regexp.MustCompile(`docs/[A-Za-z0-9._/-]*docs/`)

// TestNoNestedDocsPrefix fails on a citation whose path repeats the docs/
// prefix, which a rename can produce and which TestDocReferencesResolve
// cannot see.
//
// It could not see it because the pattern there matches the INNER path, and
// the inner path resolves: `docs/subsystems/docs/subsystems/controlplane.md`
// is read as a citation of `docs/subsystems/controlplane.md`, which exists,
// so the check agrees. Retiring docs/modules/ for docs/subsystems/ left 87
// of these across 58 files and nothing went red — the one failure shape a
// resolver cannot report is the one where a wrong path contains a right one.
func TestNoNestedDocsPrefix(t *testing.T) {
	root := repoRoot(t)
	files := citableFiles(t, root)
	for _, rel := range files {
		if rel == filepath.Join("test", "buildrules", "docanchors_test.go") {
			continue // the pattern above is not a citation
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if m := nestedDocsPrefix.FindString(line); m != "" {
				t.Errorf("%s:%d cites %q, which repeats the docs/ prefix.\n"+
					"Drop the duplicated prefix — the path resolves for a reader only by accident.",
					rel, i+1, m)
			}
		}
	}
}

// mdLinkLabel matches a markdown link so the label can be dropped and the
// target kept — see the call site.
var mdLinkLabel = regexp.MustCompile(`\[[^\]]*\]\(([^)]*)\)`)

// citationBoundary reports whether the match at [start,end) is a whole path
// rather than the tail or the head of a longer word. Two real false positives
// it kills, both of them documents this repository does not own:
// `AGENTS.override.md` (a filename, whose tail reads as `override.md`) and
// `server/tools.mdx` (an upstream spec page, whose head reads as `tools.md`).
func citationBoundary(line string, start, end int) bool {
	if start > 0 {
		switch c := line[start-1]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '/', c == '-', c == '_':
			return false
		}
	}
	if end < len(line) {
		switch c := line[end]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			return false
		}
	}
	return true
}

// group returns submatch n of an index-based match, or "" when it did not
// participate.
func group(line string, m []int, n int) string {
	if m[2*n] < 0 {
		return ""
	}
	return line[m[2*n]:m[2*n+1]]
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

// citableFiles lists the files whose comments and prose may cite a document.
//
// The Makefile, the shell scripts and the frontend's HTML are here because
// leaving them out is how the round that added this line found a Makefile and
// an install.sh still citing docs/canonical.md, five commits after that file
// was split up: a rule that reaches four file types is silent about the fifth,
// and silence reads exactly like agreement.
func citableFiles(t *testing.T, root string) []string {
	t.Helper()
	return walkRepoFiles(t, root, "citable files", func(name string) bool {
		if name == "Makefile" {
			return true
		}
		switch filepath.Ext(name) {
		case ".go", ".md", ".ts", ".yml", ".yaml", ".sh", ".py", ".html":
			return true
		}
		return false
	})
}
