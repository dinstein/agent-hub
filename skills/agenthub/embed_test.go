package agenthub

import (
	"os"
	"testing"
)

// TestTheEmbeddedSkillIsTheFileOnDisk pins the embed to the file the release
// path publishes.
//
// `//go:embed` fails the build when its pattern matches nothing, so the
// failure this catches is the other one: a pattern that still matches, but a
// different file. Renaming SKILL.md and re-pointing the directive, or adding
// a second markdown file here and embedding that, both compile — and the
// binary then hands out a document scripts/tap-sync.sh never publishes, while
// the tap goes on serving one nothing compiles. Comparing against the path
// tap-sync.sh reads is what makes the two channels provably one file.
func TestTheEmbeddedSkillIsTheFileOnDisk(t *testing.T) {
	onDisk, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatalf("reading SKILL.md: %v; scripts/tap-sync.sh reads it at this path on every release", err)
	}
	if len(onDisk) == 0 {
		t.Fatal("SKILL.md is empty; the embed would compile and the binary would print nothing")
	}
	if SkillMD != string(onDisk) {
		t.Errorf("the embedded skill is not SKILL.md: %d bytes embedded, %d on disk.\n"+
			"The binary and the tap must hand out the same document.", len(SkillMD), len(onDisk))
	}
}
