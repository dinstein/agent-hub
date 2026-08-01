package buildrules

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeSkillsLinkToCanonicalDirectory(t *testing.T) {
	root := repoRoot(t)
	link := filepath.Join(root, ".claude", "skills")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("reading .claude/skills: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal(".claude/skills must be a symlink; copied skills create a second source of truth")
	}

	const want = "../.agents/skills"
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("reading .claude/skills symlink: %v", err)
	}
	if got != want {
		t.Fatalf(".claude/skills points to %q, want the relative canonical path %q", got, want)
	}
}
