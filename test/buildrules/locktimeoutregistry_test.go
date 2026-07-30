package buildrules

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// parityTable is the file whose table has to name every store that can time
// out on a lock. It is a _test.go file, so goSources does not reach it.
const parityTable = "internal/cli/locktimeoutparity_test.go"

// lockTimeoutSentinel matches the declaration of a package's ErrLockTimeout,
// in either the `var X = ...` or the grouped `X = ...` spelling.
var lockTimeoutSentinel = regexp.MustCompile(`(?m)^\s*(?:var\s+)?ErrLockTimeout\s*=`)

// TestEveryLockTimeoutStoreIsInTheParityTable keeps
// internal/cli/locktimeoutparity_test.go from falling behind the tree.
//
// That test pins the one answer an operator gets for lock contention — exit
// 7, with a retry hint — across every store that can produce it, because
// none of them may import another's document model and so nothing but a test
// makes their errors agree. It claimed to fail when a fifth store was added
// and left untyped. It could not: its cases are a hand-written table, and a
// new package with its own ErrLockTimeout changes nothing there.
//
// The direction that hurts is exactly that one. A store whose ladder returns
// a plain fmt.Errorf falls through the CLI's classifier to exit 1 with a raw
// message, while the identical contention on any other store exits 7 with a
// hint — which is the bug the parity test was written for, in a package it
// does not look at. It stays green throughout.
//
// So the check is on the declarations: whatever declares the sentinel has to
// appear in the table. Seven packages take a cross-process flock, but only
// the ones with a TIMEOUT ladder can report contention to a user at all —
// audit and ratelimit block on LOCK_EX and oauthflow tries once — and
// declaring ErrLockTimeout is what tells the two apart.
func TestEveryLockTimeoutStoreIsInTheParityTable(t *testing.T) {
	root := repoRoot(t)

	table, err := os.ReadFile(filepath.Join(root, parityTable))
	if err != nil {
		t.Fatalf("reading %s: %v", parityTable, err)
	}
	cases := string(table)

	var found int
	for rel, src := range goSources(t, root) {
		if !strings.HasPrefix(rel, "internal/") || !lockTimeoutSentinel.MatchString(src) {
			continue
		}
		pkg := path.Base(path.Dir(rel))
		found++
		if !strings.Contains(cases, pkg+".ErrLockTimeout") {
			t.Errorf("%s declares ErrLockTimeout, and %s never mentions %s.\n"+
				"Contention in that store is then classified by nothing: it falls through "+
				"the CLI's default branch to exit 1 with a raw message, while every other "+
				"store exits 7 with a retry hint — and no test goes red.\n"+
				"Add a case to the table naming %s.ErrLockTimeout.",
				rel, parityTable, pkg, pkg)
		}
	}
	if found == 0 {
		t.Fatalf("found no package declaring ErrLockTimeout; the walk is wrong, not the tree")
	}
}

// goSources reads every non-test .go file under root, keyed by repo-relative
// path. It moved here when the unwired-inventory check was retired with
// internal/integrity; this file is the surviving caller.
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
