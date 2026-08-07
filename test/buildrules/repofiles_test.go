package buildrules

import (
	"io/fs"
	"path/filepath"
	"testing"
)

// skippedDirs are the directories no repository rule reaches into: vendored
// or generated trees, and the fixture directories a rule would misread as
// source.
//
// This list is the load-bearing half of walkRepoFiles, and the reason both
// live in one place. Four collectors used to carry their own copy — behind
// "every doc path resolves", "every citation resolves", "every test names
// what it holds" and the write-ladder rule — and each of them decides which
// files its rule COVERS. A directory added to one copy and not the others
// takes that tree out of one rule's reach while every other rule still claims
// it: a rule quietly becoming a suggestion in exactly one place, and staying a
// rule everywhere a reader looks.
var skippedDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"dist":         true,
	"bin":          true,
	"testdata":     true,
}

// walkRepoFiles lists every file under root that keep accepts, as paths
// relative to root. what names the subject in the failure message, so a walk
// that dies says which rule lost its input.
func walkRepoFiles(t *testing.T, root, what string, keep func(name string) bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !keep(d.Name()) {
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
		t.Fatalf("walking %s for %s: %v", root, what, err)
	}
	// An empty result is a broken walk, never a real answer: all four callers
	// ask the repository root for a file type it certainly contains. Left
	// unchecked, a collector that silently found nothing would make its rule
	// PASS — over no files — which is the one way a repository rule can stop
	// holding without anything going red. One function now feeds four of them,
	// so the assertion belongs here rather than in each.
	if len(out) == 0 {
		t.Fatalf("walking %s for %s found nothing; the rule would pass over an empty set", root, what)
	}
	return out
}
