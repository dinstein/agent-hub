package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// docs/modules/security.md keeps an inventory of the integrity capabilities
// that exist at the storage layer and are NOT reached by the assembled
// product. That list is load-bearing in a way an ordinary doc paragraph is
// not: it is the difference between "agenthub blocks unapproved tools" and
// "agenthub could block unapproved tools", and a security document that
// overstates what is switched on is worse than one that says nothing.
//
// It is also the kind of list nothing forces anyone to revisit. `Block` and
// `DefaultModeFor` were both absent from it while the same paragraph
// described them as in service, and the paragraph had been rewritten since
// they went unwired.
//
// WHAT THIS CHECKS, AND WHAT IT DOES NOT. It verifies the forward direction:
// every symbol the document claims is unreached really has no non-test
// caller. If someone wires one up, this fails and the doc gets corrected with
// the change that earned it.
//
// It does NOT prove the list is complete — that a newly-unwired export was
// added to it. Doing so needs type-aware call-graph analysis, and a
// name-matching approximation would produce exactly the confident-but-wrong
// answers this file exists to prevent. Completeness stays a review question;
// say so rather than implying a guarantee that is not here.
//
// One entry is also skipped outright. `QuarantineStore.Add` shares its method
// name with sync.WaitGroup, time.Time, atomic counters and the fsnotify
// watcher, and five such calls sit in the very files that import
// internal/integrity — so matching on the name alone reports the WaitGroup in
// toolpolicy.go as evidence that the quarantine store is wired. A name that
// cannot be attributed is not checked, and is named here as unchecked; the
// alternative was a test that fails for the wrong reason forever.

// unwiredSentence matches the inventory sentence; backticked pulls the names
// out of it.
//
// The terminator is a period followed by WHITESPACE, not a bare period. One
// of the entries is `QuarantineStore.Add`, and a plain `\.` ends the match
// inside it — which is how the first version of this test silently checked
// three of the six names and passed against a deliberately broken list.
var (
	unwiredSentence = regexp.MustCompile(`(?s)Still without a non-test caller:(.*?)\.\s`)
	backticked      = regexp.MustCompile("`([A-Za-z]+(?:\\.[A-Za-z]+)?)`")
)

// tooGenericToAttribute are inventory entries whose bare method name is
// shared with the standard library, so a textual search cannot tell a call to
// the integrity store from an unrelated one.
var tooGenericToAttribute = map[string]bool{"Add": true}

func TestSecurityDocsUnwiredListIsStillTrue(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "modules", "security.md"))
	if err != nil {
		t.Fatalf("reading security.md: %v", err)
	}
	names := unwiredNames(t, string(doc))

	sources := goSources(t, root)
	for _, name := range names {
		if tooGenericToAttribute[name] {
			t.Logf("skipping %q: too common a method name to attribute without type information", name)
			continue
		}
		if where := callerOf(sources, name); where != "" {
			t.Errorf("security.md lists %s as having no non-test caller, but %s calls it.\n"+
				"If that capability is now wired, the paragraph around the list has to say so — "+
				"it is what tells a reader whether the protection is on.", name, where)
		}
	}
}

// goSources returns every non-test .go file's path and contents.
func goSources(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	skip := []string{".git", "node_modules", "testdata", "frontend", "bin", "dist"}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if slices.Contains(skip, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	return out
}

// callerOf reports the first "file:line" that calls name, or "".
//
// It looks for a SELECTOR call — `x.Name(` — which is how every symbol in the
// inventory would be reached, since they are all methods on an integrity
// store or a package-level function called through the package qualifier. A
// declaration is not a call, and neither is a comment.
func callerOf(sources map[string]string, name string) string {
	call := regexp.MustCompile(`\.` + regexp.QuoteMeta(name) + `\(`)
	decl := regexp.MustCompile(`^func (\([^)]*\) )?` + regexp.QuoteMeta(name) + `\(`)
	for path, body := range sources {
		for i, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if decl.MatchString(trimmed) || !call.MatchString(line) {
				continue
			}
			return path + ":" + itoa(i+1)
		}
	}
	return ""
}

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

// TestTheUnwiredCheckReadsEveryName guards the check against the bug it
// actually had: the sentence terminator ended the match inside
// `QuarantineStore.Add`, so three of the six names were never looked at and
// the test passed against a list that named a wired symbol.
//
// A checker that silently examines a subset is worse than no checker, because
// its green is read as coverage.
func TestTheUnwiredCheckReadsEveryName(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "modules", "security.md"))
	if err != nil {
		t.Fatalf("reading security.md: %v", err)
	}
	names := unwiredNames(t, string(doc))
	// Every backticked name in the sentence must survive the parse, and the
	// method-qualified one is the case that broke it.
	if len(names) < 4 {
		t.Errorf("parsed only %v out of the inventory; the sentence terminator is eating names", names)
	}
	var hasQualified bool
	for _, n := range names {
		if n == "Add" {
			hasQualified = true
		}
	}
	if !hasQualified {
		t.Errorf("parsed %v — `QuarantineStore.Add` did not survive, which is exactly where "+
			"the terminator used to stop", names)
	}
}

// unwiredNames extracts the symbol names from the inventory sentence.
// `QuarantineStore.Add` names a method; the symbol to look for is Add.
func unwiredNames(t *testing.T, doc string) []string {
	t.Helper()
	m := unwiredSentence.FindStringSubmatch(doc)
	if m == nil {
		t.Fatal(`security.md no longer contains a "Still without a non-test caller:" sentence; ` +
			"either the inventory moved and this check needs to follow it, or it was deleted and " +
			"nothing now records which integrity capabilities are switched off")
	}
	var names []string
	for _, b := range backticked.FindAllStringSubmatch(m[1], -1) {
		name := b[1]
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		t.Fatal("the inventory sentence names no symbols; the parse is broken, not the doc")
	}
	return names
}
