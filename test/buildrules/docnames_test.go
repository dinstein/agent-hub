package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// testFuncDecl matches a top-level test, fuzz or benchmark declaration.
	testFuncDecl = regexp.MustCompile(`^func ((?:Test|Fuzz|Benchmark)\w*)\s*\(`)
	// docCommentHead matches a doc comment whose FIRST line opens with a
	// Test/Fuzz/Benchmark name, which is the shape this package's convention
	// and godoc's both use.
	docCommentHead = regexp.MustCompile(`^// ((?:Test|Fuzz|Benchmark)[A-Z]\w+)\b`)
)

// TestDocCommentsNameTheirOwnTest fails when a test's doc comment opens with
// the name of a different test.
//
// Renaming a test is a two-line edit and the second line is easy to forget:
// five comments in this tree led with a name their function no longer had, and
// in each case the stale name was the more descriptive of the two, which is
// exactly what makes it read as correct. The cost is not cosmetic. Comments
// here cite test names as evidence — "pinned by TestX" — and a citation that
// resolves to nothing sends the next reader looking for a guarantee under a
// name that was never there.
//
// WHAT THIS CHECKS. Only the first line of a contiguous comment block sitting
// immediately above a Test/Fuzz/Benchmark declaration. That narrowness is
// deliberate, and it is what keeps the check off three shapes that look
// similar and are all correct:
//
//   - Type, field and method names that begin with Test: ctlapi's TestConn
//     interface, its TestDeps field, the GUI Hub's TestServer method. Their
//     comments sit above a type or a method, not above a test declaration, so
//     they are never examined.
//   - Globs naming a family: oauthflow's fakeAS says "see TestSSRF*". Safe for
//     two independent reasons — it is attached to a type, and it is mid-
//     sentence rather than opening the block. A glob that DID open a test's own
//     doc comment would be reported, and should be: the first line is where
//     godoc expects that test's own name.
//   - Deliberate references to tests that are GONE. daemon/oauth_test.go
//     explains at length that "the test was called TestRefresherSingleflight"
//     and why it was replaced; the whole value of that paragraph is naming
//     something that no longer exists. It survives because it is mid-comment.
//
// A name mentioned anywhere other than that first line is therefore not
// checked. The alternative — matching Test-shaped words anywhere in any
// comment — reports all three of the above and gets itself disabled. Each of
// these three was verified by planting the shape and watching the check stay
// quiet, not by reasoning about the regex.
func TestDocCommentsNameTheirOwnTest(t *testing.T) {
	root := repoRoot(t)
	files := goTestFiles(t, root)
	if len(files) == 0 {
		t.Fatal("found no _test.go files; the walk or the root is wrong")
	}

	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			decl := testFuncDecl.FindStringSubmatch(line)
			if decl == nil {
				continue
			}
			top, ok := docCommentTop(lines, i)
			if !ok {
				continue // no doc comment; nothing to disagree with
			}
			head := docCommentHead.FindStringSubmatch(lines[top])
			if head == nil {
				continue // prose that does not open with a name
			}
			if head[1] != decl[1] {
				t.Errorf("%s:%d: doc comment opens with %q but is attached to %q.\n"+
					"Rename the comment, not the function — the function name is what "+
					"`go test -run` selects and what other files cite.",
					rel, top+1, head[1], decl[1])
			}
		}
	}
}

// docCommentTop returns the index of the first line of the contiguous //
// comment block immediately above declLine, and whether there was one.
func docCommentTop(lines []string, declLine int) (int, bool) {
	i := declLine - 1
	for i >= 0 && strings.HasPrefix(lines[i], "//") {
		i--
	}
	if i == declLine-1 {
		return 0, false
	}
	return i + 1, true
}

// goTestFiles lists every _test.go file in the tree, skipping vendored and
// generated directories.
func goTestFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "dist", "bin", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for test files: %v", root, err)
	}
	return out
}
