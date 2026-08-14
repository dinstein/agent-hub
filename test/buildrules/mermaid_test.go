package buildrules

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mermaidDiagramType matches the first word of a mermaid block, which names
// the diagram. The set is what this tree draws, not everything mermaid can.
var mermaidDiagramType = regexp.MustCompile(`^(flowchart|graph|sequenceDiagram|stateDiagram(-v2)?|erDiagram|classDiagram|gantt|pie|journey|timeline)\b`)

// sequenceMessage matches a message line in a sequence diagram — the shape
// `A->>B: text`, in every arrow spelling mermaid accepts.
var sequenceMessage = regexp.MustCompile(`^\s*\S+\s*-[->x)]{1,3}>?\s*\S+\s*:`)

// mermaidMarkup matches HTML in a mermaid label; mermaidAllowed is the one
// exception. `<br/>` and `<br>` are special-cased by every renderer, which is
// exactly what the others are not. Go's regexp has no negative lookahead, so
// the exception is a second pattern rather than a `(?!…)`.
var (
	mermaidMarkup  = regexp.MustCompile(`(?i)<[a-z/][^>]*>|&(lt|gt|amp|quot|#\d+);`)
	mermaidAllowed = regexp.MustCompile(`(?i)^<br\s*/?>$`)
)

// TestMermaidDiagramsAvoidWhatRenderersDisagreeOn keeps the diagrams in docs/
// off the two shapes that broke them, both of which are invisible in review:
// the markdown looks right, and the picture is wrong or missing.
//
//  1. A SEMICOLON IN A SEQUENCE MESSAGE. mermaid reads `;` as a statement
//     separator, so `G->>G: stand alone; scope comes from the files` parses
//     the half after it as a new statement and the WHOLE diagram fails to
//     render — four of this tree's seven sequence diagrams did, and what a
//     reader saw was an error box where the picture should be.
//  2. HTML BEYOND `<br/>`. `<b>`, `<i>` and escaped angle brackets render as
//     markup in one renderer and as literal text in another, because
//     htmlLabels is a per-site setting: GitHub, an IDE preview and mermaid.live
//     do not agree. A label reading `<b>servers</b>` is not a rendering bug
//     anybody reports — it just looks broken.
//
// WHAT THIS CHECKS, AND WHAT IT DOES NOT. It is a lexical check on the block's
// text. It does not parse mermaid and cannot: that needs a JavaScript runtime,
// and a check that quietly skips when node is missing is a check this
// repository does not count (docs/conventions.md#engineering-conventions). So a
// diagram that parses and still says the wrong thing is a review question, and
// the syntax errors this cannot see are caught by opening the file in any
// renderer.
func TestMermaidDiagramsAvoidWhatRenderersDisagreeOn(t *testing.T) {
	root := repoRoot(t)
	files := citableFiles(t, root)

	blocks := 0
	for _, rel := range files {
		if filepath.Ext(rel) != ".md" || rel == filepath.Join("test", "buildrules", "mermaid_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		for _, b := range mermaidBlocks(string(data)) {
			blocks++
			for _, f := range mermaidFindings(rel, b) {
				t.Error(f)
			}
		}
	}
	if blocks == 0 {
		t.Fatal("found no mermaid blocks; the fence scan is wrong")
	}
	t.Logf("checked %d mermaid diagrams", blocks)
}

// mermaidBlock is one fenced diagram, with the file line its fence sits on.
type mermaidBlock struct {
	Line  int
	Lines []string
}

func mermaidBlocks(doc string) []mermaidBlock {
	var out []mermaidBlock
	var cur *mermaidBlock
	for i, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if cur == nil {
			if trimmed == "```mermaid" {
				cur = &mermaidBlock{Line: i + 1}
			}
			continue
		}
		if trimmed == "```" {
			out = append(out, *cur)
			cur = nil
			continue
		}
		cur.Lines = append(cur.Lines, line)
	}
	return out
}

// mermaidFindings reports what is wrong with one block. It returns findings
// rather than failing, so TestMermaidCheckBites can plant each shape and see
// it reported — a check with no failing case is one nobody can trust.
func mermaidFindings(rel string, b mermaidBlock) []string {
	var out []string
	kind := ""
	for _, line := range b.Lines {
		if s := strings.TrimSpace(line); s != "" {
			kind = s
			break
		}
	}
	if !mermaidDiagramType.MatchString(kind) {
		return []string{fmt.Sprintf("%s:%d opens a mermaid block with %q, which names no diagram type.\n"+
			"The first line decides how the rest is parsed; without it nothing renders.", rel, b.Line, kind)}
	}
	isSequence := strings.HasPrefix(kind, "sequenceDiagram")

	for n, line := range b.Lines {
		at := b.Line + n + 1
		if isSequence && sequenceMessage.MatchString(line) && strings.Contains(line, ";") {
			out = append(out, fmt.Sprintf("%s:%d puts a `;` in a sequence-diagram message.\n"+
				"mermaid reads it as a statement separator, so the whole diagram fails to render. "+
				"Use an em dash or a comma:\n  %s", rel, at, strings.TrimSpace(line)))
		}
		if m := firstDisallowedMarkup(line); m != "" {
			out = append(out, fmt.Sprintf("%s:%d uses %q inside a mermaid diagram.\n"+
				"Only <br/> renders the same everywhere; other tags and HTML entities show as "+
				"literal text wherever htmlLabels is off. Write the plain characters:\n  %s",
				rel, at, m, strings.TrimSpace(line)))
		}
	}
	return out
}

// firstDisallowedMarkup returns the first HTML fragment on the line that is
// not the permitted line break, or "" when there is none.
func firstDisallowedMarkup(line string) string {
	for _, m := range mermaidMarkup.FindAllString(line, -1) {
		if !mermaidAllowed.MatchString(m) {
			return m
		}
	}
	return ""
}

// TestMermaidCheckBites plants each shape the check exists for and asserts it
// is reported. Both were live defects, and both looked correct in review.
func TestMermaidCheckBites(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  string
	}{
		{
			name:  "semicolon in a sequence message",
			block: "sequenceDiagram\n    A->>B: stand alone; scope comes from the files",
			want:  "sequence-diagram message",
		},
		{
			name:  "bold tag in a label",
			block: "flowchart TD\n    S[\"<b>servers</b>\"]",
			want:  "inside a mermaid diagram",
		},
		{
			name:  "escaped angle brackets in a label",
			block: "flowchart LR\n    A[\"logs/gateway-&lt;client&gt;.log\"]",
			want:  "inside a mermaid diagram",
		},
		{
			name:  "no diagram type",
			block: "    A --> B",
			want:  "names no diagram type",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := mermaidBlock{Line: 1, Lines: strings.Split(c.block, "\n")}
			found := mermaidFindings("planted.md", b)
			if len(found) == 0 {
				t.Fatalf("planted %q and the check stayed quiet", c.block)
			}
			if !strings.Contains(strings.Join(found, "\n"), c.want) {
				t.Fatalf("reported %q, which does not mention %q", found, c.want)
			}
		})
	}

	// The control: a diagram shaped like the ones in docs/ must pass, or the
	// check above would be satisfied by rejecting everything.
	ok := mermaidBlock{Line: 1, Lines: strings.Split(
		"sequenceDiagram\n    A->>B: stand alone — scope comes from the files\n    Note over A,B: fine", "\n")}
	if found := mermaidFindings("planted.md", ok); len(found) != 0 {
		t.Fatalf("a clean diagram was reported: %q", found)
	}
}
