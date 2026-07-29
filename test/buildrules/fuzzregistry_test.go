package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestEveryFuzzTargetIsInTheMakefile keeps `make fuzz` honest.
//
// The Makefile carries FUZZ_TARGETS, a hand-written list of
// <package>:<FuzzName> pairs, because the target name alone is not enough to
// run one — `go test -fuzz` needs to be pointed at a package, and the seven
// targets do not all live in the same one. Nothing keeps that list in step
// with the tree.
//
// A target missing from it is the failure worth catching. `make ci` still
// runs its seed corpus, because that is just `go test`, so everything looks
// covered — but `make fuzz`, the deep sweep AGENTS.md tells you to run when
// you touch a parser that reads untrusted input, silently never reaches it.
// The guard is written, configured, and not in effect.
//
// The reverse direction is checked too: an entry naming a target that no
// longer exists makes `make fuzz` fail late, in the middle of a long run,
// on a package that cannot answer for it.
func TestEveryFuzzTargetIsInTheMakefile(t *testing.T) {
	root := repoRoot(t)
	declared := parseFuzzTargets(t, filepath.Join(root, "Makefile"))
	actual := findFuzzTargets(t, root)

	for pkgTarget, file := range actual {
		if _, ok := declared[pkgTarget]; !ok {
			t.Errorf("%s defines %s, which is not in the Makefile's FUZZ_TARGETS.\n"+
				"`make fuzz` will never run it, so its seed corpus is the only fuzzing it gets.\n"+
				"Add \"%s\" to FUZZ_TARGETS, and name it in AGENTS.md beside the others.",
				file, pkgTarget, pkgTarget)
		}
	}
	for pkgTarget, line := range declared {
		if _, ok := actual[pkgTarget]; !ok {
			t.Errorf("Makefile line %d declares fuzz target %q, which no longer exists.\n"+
				"`make fuzz` would fail partway through a long run rather than at the top.",
				line, pkgTarget)
		}
	}
}

// TestAgentsMdNamesEveryFuzzTarget. AGENTS.md is where a contributor is told
// which parsers are guarded and to run a round when they touch one. A target
// it does not name is one nobody is told to run.
func TestAgentsMdNamesEveryFuzzTarget(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("reading AGENTS.md: %v", err)
	}
	text := string(doc)
	for pkgTarget := range findFuzzTargets(t, root) {
		name := pkgTarget[strings.LastIndex(pkgTarget, ":")+1:]
		if !strings.Contains(text, name) {
			t.Errorf("AGENTS.md does not mention the fuzz target %s, so nobody is told it exists", name)
		}
	}
}

// repoRoot resolves the repository root from this package's directory
// (test/buildrules → ../..), the same way internal/depguardtest does.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s does not look like the repository root: %v", root, err)
	}
	return root
}

var fuzzTargetLine = regexp.MustCompile(`^\s*(\./\S+):(Fuzz\w+)\s*\\?\s*$`)

// parseFuzzTargets reads the FUZZ_TARGETS assignment, returning each
// "<pkg>:<Name>" pair mapped to the line it was declared on.
func parseFuzzTargets(t *testing.T, path string) map[string]int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	out := map[string]int{}
	inList := false
	for i, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "FUZZ_TARGETS") {
			inList = true
			continue
		}
		if !inList {
			continue
		}
		m := fuzzTargetLine.FindStringSubmatch(line)
		if m == nil {
			break // the assignment ended
		}
		out[m[1]+":"+m[2]] = i + 1
	}
	if len(out) == 0 {
		t.Fatal("no FUZZ_TARGETS were parsed out of the Makefile; the assignment's shape changed " +
			"and this check would silently pass forever")
	}
	return out
}

var fuzzFunc = regexp.MustCompile(`(?m)^func (Fuzz\w+)\(`)

// findFuzzTargets walks the tree for fuzz functions, returning each
// "<pkg>:<Name>" pair mapped to the file that declares it. Package paths are
// spelled "./internal/..." to match the Makefile's own notation.
func findFuzzTargets(t *testing.T, root string) map[string]string {
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
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		for _, m := range fuzzFunc.FindAllStringSubmatch(string(data), -1) {
			out["./"+filepath.ToSlash(rel)+":"+m[1]] = filepath.ToSlash(
				filepath.Join(rel, d.Name()))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no fuzz functions were found anywhere; the walk is broken, not the tree")
	}
	return out
}
