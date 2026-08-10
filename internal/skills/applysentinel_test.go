package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// TestApplySentinelVerifiesBeforeWriting is the regression for the reorder half
// of the 2026-08-10 sweep's skills finding. A marker reaching the rendered body
// (here via the skill Version) makes upsertBlock produce a duplicate-marker
// block that findBlock rejects. applySentinel used to atomicWrite the file and
// only THEN run findBlock, committing a corrupt, unremovable block to a shared
// client rule file before erroring. It must verify first and leave the file
// untouched on failure.
func TestApplySentinelVerifiesBeforeWriting(t *testing.T) {
	m, _ := testManager(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "rules.md")
	const original = "# user rules\nkeep me\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("seed target file: %v", err)
	}

	// A skill with no stored SKILL.md (renderSkillBody yields an empty body)
	// whose Version smuggles this skill's own end marker into the heading.
	sk := &Skill{ID: "x", Name: "x", Version: endMarker("x")}

	if _, err := m.applySentinel(sk, TargetDef{ClientID: "test-client"}, path); err == nil {
		t.Fatal("applySentinel accepted a body that does not read back")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read target file: %v", err)
	}
	if string(got) != original {
		t.Fatalf("the shared file was modified by a failed apply:\n%s", got)
	}
}
