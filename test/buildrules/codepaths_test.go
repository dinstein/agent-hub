package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// generatedProbes are filenames that comments name but the tree must NOT
// contain. internal/depguardtest writes them into a throwaway copy of the
// repository — 339f541 moved them there precisely so a failed run could not
// leave a compile error behind — so a comment explaining the probe protocol
// names a file that only ever exists mid-test.
var generatedProbes = regexp.MustCompile(`^zz_depguard_probe_`)

// goFileRef matches a Go file named in prose, capturing the leading
// directories when the comment gives them. A rooted citation is checked as a
// whole path; a bare filename can only be checked by name.
var goFileRef = regexp.MustCompile(`\b((?:internal|cmd|api|test|build|scripts)/[A-Za-z0-9_./-]+/)?([a-z][a-z0-9_]*\.go)\b`)

// TestCodeCommentsCiteFilesThatExist is TestDocsCitePathsThatExist pointed at
// the other half of the prose.
//
// Comments in this tree cross-reference files constantly — "delegating to
// internal/platform (flock.go)", "the rule in doc.go", "see
// daemonproc_stub.go" — because a package's reasoning is spread across files
// and the comment is what stitches it back together. The docs got a guard for
// exactly this and the code did not, though the code has far more of these
// references and they move with every rename.
//
// The failure is quiet in the same way: a filename that does not resolve does
// not read as stale, it reads as a file the reader has failed to find, so the
// honest response is to go looking. One was already here when this test was
// written: internal/skills/errors.go attributed its fail-closed rule to a
// file named after the integrity package, which has never existed in this
// layout — the rule is stated in docs/subsystems/guard.md.
//
// This file's own prose is scanned like any other. Naming a dead filename to
// illustrate a dead filename would have to be exempted, and an exemption on
// the file that defines the check is the one hole nothing else would catch.
//
// Scope, and be clear about the weak half. A citation that gives the
// directory is checked as a whole path, so renaming or moving the file fails
// this test. A BARE filename is only checked against the set of basenames in
// the tree, which is as far as it can be taken: 46 of the tree's ~210 bare
// citations point outside the citing package (test/buildrules discusses other
// packages' files constantly, and the seven packages that lock a file all
// name flock_stub.go), so requiring the same directory would be 46 false
// positives. The consequence is worth stating rather than discovering: rename
// one package's doc.go and this test stays green, because eight other
// packages still have one. A distinctive name is caught; a common one is not.
//
// Not checked at all: that the named file still says what the comment claims
// about it. That is a review question and always will be.
func TestCodeCommentsCiteFilesThatExist(t *testing.T) {
	root := repoRoot(t)

	present := goFileNames(t, root)
	if len(present) == 0 {
		t.Fatal("found no .go files; the walk or the root is wrong")
	}

	for rel, src := range allGoSources(t, root) {
		for i, line := range strings.Split(src, "\n") {
			for _, m := range goFileRef.FindAllStringSubmatch(commentOf(line), -1) {
				dir, name := m[1], m[2]
				if generatedProbes.MatchString(name) {
					continue
				}
				if dir != "" {
					if !exists(root, dir+name) {
						t.Errorf("%s:%d cites %q, which does not exist.\n"+
							"A comment pointing at a file that moved does not read as stale — it "+
							"reads as a file the reader failed to find. Point it at the real one.",
							rel, i+1, dir+name)
					}
					continue
				}
				if !present[name] {
					t.Errorf("%s:%d names %q, and no file by that name is in the tree.\n"+
						"A comment pointing at a file that moved does not read as stale — it reads "+
						"as a file the reader failed to find. Point it at the real one.",
						rel, i+1, name)
				}
			}
		}
	}
}

// commentOf returns the comment part of a Go line, or "". The `:` guard keeps
// a URL's "//" from turning the rest of a string literal into prose.
func commentOf(line string) string {
	for i := 0; i+1 < len(line); i++ {
		if line[i] != '/' || line[i+1] != '/' {
			continue
		}
		if i > 0 && line[i-1] == ':' {
			continue
		}
		return line[i+2:]
	}
	return ""
}

// goFileNames returns the set of .go basenames present in the tree.
func goFileNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for rel := range allGoSources(t, root) {
		out[filepath.Base(rel)] = true
	}
	return out
}

// allGoSources is goSources plus the _test.go files it skips: a test file's
// comments cross-reference just as much as a non-test one's, and the
// stale-citation risk is identical.
func allGoSources(t *testing.T, root string) map[string]string {
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
		if !strings.HasSuffix(d.Name(), ".go") {
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
		t.Fatalf("walking %s for go sources: %v", root, err)
	}
	return out
}
