package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNothingSplitsAnExposedNameOnTheSeparator enforces one of AGENTS.md's
// "easiest things to get wrong": `RouteOf` is the only legitimate provenance
// for an exposed name, and splitting on `__` is forbidden.
//
// The reason is in internal/router's package comment. An exposed name is
// sanitize(serverID) + "__" + sanitize(rawTool), and BOTH halves may contain
// "__" themselves — so a split is ambiguous and a wrong answer is not a parse
// error, it is a different server's tool. Provenance decides which route the
// scope gate is asked about, so getting it wrong is a routing question with a
// security answer.
//
// Until now the rule lived in prose: AGENTS.md, router's package comment, and
// three call sites that say they are obeying it. That is the shape this
// project keeps finding on the wrong side of — a rule everyone repeats and
// nothing refuses.
//
// Membership is not parsing. `strings.Contains(name, "__")` answers "is this a
// router-built name at all", which is what discovery.IsBareName is for, and it
// cannot attribute a name to the wrong server. Only the functions that CUT are
// refused.
//
// Test files are excluded, deliberately and for the same reason
// TestNoDocumentTeachesARetiredCommand excludes them: a test proving the
// router's construction is reversible has to take the name apart to say so.
func TestNothingSplitsAnExposedNameOnTheSeparator(t *testing.T) {
	root := repoRoot(t)
	var offences []string
	scanned := 0

	skip := map[string]bool{".git": true, "node_modules": true, "testdata": true, "dist": true, "bin": true}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		if rel == filepath.Join("test", "buildrules", "exposedsplit_test.go") {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if separatorCut.MatchString(line) {
				offences = append(offences, rel+":"+itoa(i+1)+"  "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A walk that reaches nothing agrees with everything; this tree has
	// hundreds of non-test Go files.
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test Go files; the walk is not reaching the tree", scanned)
	}

	for _, o := range offences {
		t.Errorf("this cuts a string on the exposed-name separator:\n  %s\n"+
			"An exposed name is sanitize(serverID)+\"__\"+sanitize(rawTool) and either half may "+
			"contain \"__\", so the split is ambiguous — and a wrong answer is another server's "+
			"tool, not an error. Use router.RouteOf, which carries the provenance instead of "+
			"recovering it.", o)
	}
}

// separatorCut matches the string functions that take a value APART on the
// separator. Contains is absent on purpose: it asks whether a name is
// router-built, never which server it belongs to.
var separatorCut = regexp.MustCompile(
	`strings\.(Split|SplitN|SplitAfter|SplitAfterN|Cut|CutPrefix|CutSuffix|Index|LastIndex)\([^)]*"__"`)

// itoa keeps the failure message free of an fmt import for one number.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
