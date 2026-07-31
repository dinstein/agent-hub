package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAddRefusesAnIDThatIsAPath is the regression for an explicit --id
// stored verbatim and then joined into filesystem paths.
//
// The ID names the store directory a version is copied into and the tree a
// removal deletes, so "../../../x" escaped the skills store in both
// directions. It was reachable only from an operator-typed flag, which made
// it a destructive footgun; it becomes a hole the moment a GUI or ctlapi
// caller builds an AddRequest.
func TestAddRefusesAnIDThatIsAPath(t *testing.T) {
	t.Parallel()
	m, _ := testManager(t)
	ctx := context.Background()
	src := writeTree(t, t.TempDir(), map[string]string{SkillFileName: sampleSkillMD})

	for _, id := range []string{
		"../escape",
		"../../../some-dir",
		"a/b",
		`a\b`,
		"..",
		".",
		"-leading",                      // slugify never mints one
		"trailing-",                     //
		"Upper",                         // shares a directory with "upper" on a case-insensitive fs
		"has space",                     //
		"nul\x00byte",                   //
		strings.Repeat("a", maxIDLen+1), // over the length bound
	} {
		_, err := m.Add(ctx, AddRequest{Path: src, ID: id})
		if !errors.Is(err, ErrInvalidID) {
			t.Errorf("Add(id=%q) = %v; want ErrInvalidID", id, err)
		}
	}
}

// TestValidIDRejectsTheEmptyString is separate because Add cannot reach it:
// an empty req.ID is the documented "mint one for me". The shape check is
// still the one that has to answer, because every other caller of the ID —
// the copy, the prune, the removal — joins it into a path, and an empty
// segment collapses that join onto the store directory itself.
func TestValidIDRejectsTheEmptyString(t *testing.T) {
	t.Parallel()
	if validID("") {
		t.Fatal(`validID("") must be false: the join would collapse onto the store directory`)
	}
}

// TestAddAcceptsTheShapeItMints: the failure direction of a shape check is
// refusing legitimate input, so the IDs this package produces itself must
// pass. "" is not in this list — an empty req.ID means "mint one".
func TestAddAcceptsTheShapeItMints(t *testing.T) {
	t.Parallel()
	m, _ := testManager(t)
	ctx := context.Background()
	for _, id := range []string{"pdf-tools", "skill", "skill-2", "a", "a1", "2fa-helper"} {
		src := writeTree(t, t.TempDir(), map[string]string{SkillFileName: sampleSkillMD})
		if _, err := m.Add(ctx, AddRequest{Path: src, ID: id}); err != nil {
			t.Errorf("Add(id=%q): %v", id, err)
		}
	}
}

// TestRemoveRefusesToDeleteOutsideTheStore covers the direction the door
// check cannot: an ID that reached the index some other way. Removal is
// where believing it costs a tree outside the store, and dropping the
// library entry without its files is the recoverable half of the two.
func TestRemoveRefusesToDeleteOutsideTheStore(t *testing.T) {
	t.Parallel()
	m, _ := testManager(t)
	ctx := context.Background()
	sk, _ := addSample(t, m)

	// A neighbour of the store directory, standing in for anything a
	// traversal could reach.
	root := filepath.Dir(m.dir)
	victim := filepath.Join(root, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}

	// Rewrite the ID in the index the way a tamperer would, then remove.
	if err := m.withState(ctx, func(st *state) error {
		entry := st.skills.Skills[sk.ID]
		delete(st.skills.Skills, sk.ID)
		entry.ID = "../../victim"
		st.skills.Skills[entry.ID] = entry
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Remove(ctx, RemoveRequest{ID: "../../victim"}); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("Remove = %v; want ErrInvalidID", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("removal deleted a directory outside the store: %v", err)
	}
}
