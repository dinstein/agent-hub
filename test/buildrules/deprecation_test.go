package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDeprecationMarkersCarryAReadableRemovalDate makes docs/conventions.md#mcp-protocol-scope's
// promise checkable.
//
// The section says no removal is scheduled earlier than 2027-07-28 and that
// one grep over `earliest-removal:` finds every use site. Both halves are
// only true if every marker spells the field the same way, and three of them
// did not: the http+sse sites carried `earliest-removal: deprecated
// 2025-03-26`, which is the date the feature was DEPRECATED sitting in the
// field that says when it may go.
//
// The failure is directional, which is why a check is worth its weight. A
// sweep asking what can be deleted today reads one date already long past —
// and it belongs to the single entry that must not be acted on, because
// ruling #29 keeps HTTP+SSE on the read side for servers that expose nothing
// else. Every marker with a real future date is safe from that mistake by
// arithmetic; the malformed one is not.
//
// Two properties, both mechanical:
//
//   - the whole marker fits on one line, which is what makes "one grep finds
//     them all" true. A marker wrapped across two comment lines is invisible
//     to it, and one was.
//   - `earliest-removal` is an ISO date or the literal `none`. `none` is not
//     a loophole: it is the honest answer for a feature deprecated upstream
//     that this project intends to keep, and the alternative is inventing a
//     date nobody means.
//
// What is deliberately NOT checked is the date's value. Whether 2027-07-28 is
// the right number is a decision recorded in docs/conventions.md's table, and a test
// asserting it would have to be edited every time the decision is revisited —
// which is how a check ends up being updated to match whatever the code says.
func TestDeprecationMarkersCarryAReadableRemovalDate(t *testing.T) {
	root := repoRoot(t)
	markers := findDeprecationMarkers(t, root)
	if len(markers) == 0 {
		t.Fatal("no DEPRECATED-UPSTREAM markers found; if the convention is gone, delete this " +
			"check, but do not leave the markers free to disagree")
	}

	for _, m := range markers {
		removal, ok := earliestRemoval.FindStringSubmatch(m.text), false
		if removal != nil {
			ok = removal[1] == "none" || isoDate.MatchString(removal[1])
		}
		if !ok {
			t.Errorf("%s:%d carries a DEPRECATED-UPSTREAM marker whose earliest-removal is not "+
				"a date or `none`:\n  %s\n"+
				"docs/conventions.md#mcp-protocol-scope: the field says when the feature may GO. A deprecation date "+
				"there reads as a removal date already past, and a sweep deleting what it "+
				"finds would remove something being kept on purpose.", m.file, m.line, m.text)
		}
	}
}

var (
	// The whole marker on one line: the opening tag through its closing
	// paren. A marker split across comment lines does not match, which is
	// the point — the section's promise is that one grep finds them all.
	deprecationMarker = regexp.MustCompile(`DEPRECATED-UPSTREAM\([^)\n]*\)`)
	// The opening tag alone, to catch the split ones the pattern above skips.
	deprecationOpener = regexp.MustCompile(`DEPRECATED-UPSTREAM\(`)
	earliestRemoval   = regexp.MustCompile(`earliest-removal:\s*([^),]+?)\s*[),]`)
	isoDate           = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

type deprecationSite struct {
	file string
	line int
	text string
}

// findDeprecationMarkers walks every Go file for markers, failing on any
// opening tag whose marker does not close on the same line.
func findDeprecationMarkers(t *testing.T, root string) []deprecationSite {
	t.Helper()
	var out []deprecationSite
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(data), "\n") {
			if !deprecationOpener.MatchString(line) {
				continue
			}
			m := deprecationMarker.FindString(line)
			if m == "" {
				t.Errorf("%s:%d opens a DEPRECATED-UPSTREAM marker that does not close on the "+
					"same line:\n  %s\n"+
					"docs/conventions.md#mcp-protocol-scope promises one grep finds every use site; a marker wrapped "+
					"across comment lines is not found by it.", rel, i+1, strings.TrimSpace(line))
				continue
			}
			out = append(out, deprecationSite{file: rel, line: i + 1, text: m})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// deprecatedWireNames maps a feature in the upstream Deprecated registry to
// the literal that constitutes naming it in Go source. Add a row when
// upstream deprecates something this tree touches; the row is what makes the
// marker convention enforceable instead of merely conventional.
//
// The literal is the WIRE name, quoted. Every other use site reaches the
// feature through the constant declared beside that literal, and those sites
// carry their own markers because a reader arrives at them from the grep —
// but the declaration is the one place the feature is named from nothing,
// and it is the one a new use site is copied from.
var deprecatedWireNames = []struct{ Feature, Literal string }{
	{"roots", `"roots/list"`},
	{"sampling", `"sampling/createMessage"`},
	{"logging", `"logging/setLevel"`},
	{"logging", `"notifications/message"`},
}

// TestEveryDeprecatedFeatureNamedCarriesItsMarker closes the gap the format
// check above leaves open: it proves the markers that exist are well-formed,
// not that the ones that should exist do.
//
// docs/status/mcp-2026-07-28.md §1.3 promises "each use site in this tree carries a
// DEPRECATED-UPSTREAM comment, so one grep finds them all". That promise had
// been false for `sampling` since the feature was deprecated: mcp.
// MethodSamplingCreate declared it and internal/mrtr matched on it to refuse
// it, and neither carried a marker — so the grep that is supposed to
// enumerate what has to move before 2027-07-28 skipped both.
//
// A feature with no use sites at all is not a failure. `logging` is that
// case today, and it stays in the table so that adding the first use site
// fails here rather than passing quietly.
func TestEveryDeprecatedFeatureNamedCarriesItsMarker(t *testing.T) {
	root := repoRoot(t)
	for _, want := range deprecatedWireNames {
		files := goFilesContaining(t, root, want.Literal)
		for _, f := range files {
			// This file names every literal in the table by definition.
			if strings.HasPrefix(f.file, filepath.Join("test", "buildrules")) {
				continue
			}
			if strings.Contains(f.body, markerTag+want.Feature) {
				continue
			}
			// The tag is not spelled literally here: the format check above
			// scans every Go file, this one included, and would read the
			// message as a malformed marker.
			t.Errorf("%s names the deprecated %s wire method %s but carries no "+
				"%s marker naming %q.\n"+
				"docs/status/mcp-2026-07-28.md §1.3 promises one grep finds every use site; "+
				"a site the grep misses is one nobody will migrate.",
				f.file, want.Feature, want.Literal, markerTag, want.Feature)
		}
	}
}

// markerTag is the convention's opening tag, assembled rather than written,
// for the reason given at its only use site.
const markerTag = "DEPRECATED-" + "UPSTREAM("

type goFile struct {
	file string
	body string
}

// goFilesContaining returns every Go file whose text contains literal.
func goFilesContaining(t *testing.T, root, literal string) []goFile {
	t.Helper()
	var out []goFile
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if !strings.Contains(string(data), literal) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, goFile{file: rel, body: string(data)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
