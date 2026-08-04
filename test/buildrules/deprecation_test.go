package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDeprecationMarkersCarryAReadableRemovalDate makes canonical.md §5b's
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
// the right number is a decision recorded in canonical.md's table, and a test
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
				"canonical.md §5b: the field says when the feature may GO. A deprecation date "+
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
					"canonical.md §5b promises one grep finds every use site; a marker wrapped "+
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
