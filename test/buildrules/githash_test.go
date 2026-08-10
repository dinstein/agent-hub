package buildrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitHashRecipeMatchesAcrossBuilds. The Makefile and Taskfile.yml each
// compute GIT_HASH independently — a build run through `make` and one run
// through `task` (the wails3 GUI build) must still land on the same hash, or
// a release built one way carries a version string a release built the
// other way would never produce. The only thing holding the two in step is
// a comment on each side reading "Must match the Makefile's GIT_HASH" (or
// the Taskfile equivalent); nothing stops the shell fragment itself from
// drifting while that comment stays put.
//
// Both recipes wrap the same two git commands in different shell syntax
// ($(...) in the Taskfile's `sh:`, $(shell ...) in the Makefile), so this
// checks the git commands themselves — the part a rewrite of either wrapper
// would leave untouched, and the part that actually decides the hash.
func TestGitHashRecipeMatchesAcrossBuilds(t *testing.T) {
	root := repoRoot(t)
	makefile := readFile(t, filepath.Join(root, "Makefile"))
	taskfile := readFile(t, filepath.Join(root, "Taskfile.yml"))

	for _, snippet := range []string{
		"git rev-parse --short=7 HEAD 2>/dev/null || echo unknown",
		"git diff --quiet 2>/dev/null || echo -dirty",
	} {
		if !strings.Contains(makefile, snippet) {
			t.Errorf("Makefile no longer contains %q; it and Taskfile.yml compute GIT_HASH "+
				"independently and only agreeing text keeps them in step", snippet)
		}
		if !strings.Contains(taskfile, snippet) {
			t.Errorf("Taskfile.yml no longer contains %q; a build run through `task` would then "+
				"mint a different hash than one run through `make` for the same commit", snippet)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
