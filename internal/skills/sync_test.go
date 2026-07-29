package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// addNamed imports a minimal one-file skill under a chosen name.
func addNamed(t *testing.T, m *Manager, name string) *Skill {
	t.Helper()
	src := writeTree(t, t.TempDir(), map[string]string{
		SkillFileName: "---\nname: " + name + "\nversion: 1.0.0\n---\n\nbody of " + name + "\n",
	})
	sk, err := m.Add(context.Background(), AddRequest{Path: src})
	if err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
	return sk
}

func actions(res *SyncResult) map[string]SyncAction {
	out := map[string]SyncAction{}
	for _, it := range res.Items {
		out[it.SkillID] = it.Action
	}
	return out
}

// TestSyncIdempotent: converging twice must write nothing the second time.
// A sync that keeps reporting changes is a sync nobody can automate.
func TestSyncIdempotent(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	a := addNamed(t, m, "alpha")
	b := addNamed(t, m, "beta")

	first, err := m.Sync(ctx, SyncRequest{ClientID: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed {
		t.Fatal("first sync reported no change")
	}
	if got := actions(first); got[a.ID] != ActionInstalled || got[b.ID] != ActionInstalled {
		t.Fatalf("actions = %v", got)
	}
	if first.Granularity != GranularityClient {
		t.Errorf("granularity = %q, want %q", first.Granularity, GranularityClient)
	}

	stamp := statMod(t, filepath.Join(installedPath(home, a.ID), SkillFileName))
	second, err := m.Sync(ctx, SyncRequest{ClientID: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Errorf("second sync reported a change: %+v", second.Items)
	}
	for _, it := range second.Items {
		if it.Action != ActionUnchanged {
			t.Errorf("item %s action = %q, want unchanged", it.SkillID, it.Action)
		}
	}
	if now := statMod(t, filepath.Join(installedPath(home, a.ID), SkillFileName)); now != stamp {
		t.Error("idempotent sync rewrote a file")
	}
}

// TestSyncSelectorNarrowsAndPrunes: the three-state selector only ever
// narrows, and deselected skills are unmaterialized so the target agrees
// with the scope that governs it.
func TestSyncSelectorNarrowsAndPrunes(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	a := addNamed(t, m, "alpha")
	b := addNamed(t, m, "beta")

	if _, err := m.Sync(ctx, SyncRequest{ClientID: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	res, err := m.Sync(ctx, SyncRequest{ClientID: "claude-code", Selector: &SkillSelector{Allow: []string{a.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(res); got[a.ID] != ActionUnchanged || got[b.ID] != ActionRemoved {
		t.Fatalf("actions = %v", got)
	}
	if _, err := os.Stat(installedPath(home, b.ID)); !os.IsNotExist(err) {
		t.Error("deselected skill was left materialized")
	}
	if _, err := os.Stat(installedPath(home, a.ID)); err != nil {
		t.Errorf("selected skill lost: %v", err)
	}

	// Block-all: an empty (non-nil) allow list removes everything.
	res, err = m.Sync(ctx, SyncRequest{ClientID: "claude-code", Selector: &SkillSelector{Allow: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(res); got[a.ID] != ActionRemoved {
		t.Fatalf("block-all actions = %v", got)
	}
}

// TestSyncSkipsDisabled: Disable is a narrowing that a selector can never
// undo.
func TestSyncSkipsDisabled(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	a := addNamed(t, m, "alpha")
	if _, err := m.Disable(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	res, err := m.Sync(ctx, SyncRequest{ClientID: "claude-code", Selector: &SkillSelector{Allow: []string{a.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("items = %+v, want none", res.Items)
	}
	if _, err := os.Stat(installedPath(home, a.ID)); !os.IsNotExist(err) {
		t.Error("a disabled skill was materialized")
	}
}

// TestSyncContinuesPastConflict: one bad target must not stop the batch.
func TestSyncContinuesPastConflict(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	a := addNamed(t, m, "alpha")
	b := addNamed(t, m, "beta")

	foreign := installedPath(home, a.ID)
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "mine.md"), []byte("hand written"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := m.Sync(ctx, SyncRequest{ClientID: "claude-code"})
	if err != nil {
		t.Fatalf("sync returned a hard error for one conflicted target: %v", err)
	}
	got := actions(res)
	if got[a.ID] != ActionFailed {
		t.Errorf("conflicted skill action = %q", got[a.ID])
	}
	if got[b.ID] != ActionInstalled {
		t.Errorf("healthy skill action = %q, want installed", got[b.ID])
	}
	if mustRead(t, filepath.Join(foreign, "mine.md")) != "hand written" {
		t.Error("user file was overwritten")
	}
}

// TestSyncSkipsDriftWithoutForce: drift is reported, not reverted.
func TestSyncSkipsDriftWithoutForce(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	a := addNamed(t, m, "alpha")
	if _, err := m.Sync(ctx, SyncRequest{ClientID: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(installedPath(home, a.ID), SkillFileName)
	if err := os.WriteFile(edited, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := m.Sync(ctx, SyncRequest{ClientID: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(res); got[a.ID] != ActionSkipped {
		t.Fatalf("actions = %v, want skipped", got)
	}
	if mustRead(t, edited) != "mine\n" {
		t.Error("drift was reverted without AllowDrift")
	}

	res, err = m.Sync(ctx, SyncRequest{ClientID: "claude-code", AllowDrift: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(res); got[a.ID] != ActionUpdated {
		t.Fatalf("forced actions = %v", got)
	}
}

// TestSyncProjectScope: project-scope receipts are keyed by their container,
// so the same skill can be materialized into two projects independently.
func TestSyncProjectScope(t *testing.T) {
	ctx := context.Background()
	m, _ := testManager(t)
	a := addNamed(t, m, "alpha")
	p1, p2 := t.TempDir(), t.TempDir()

	for _, root := range []string{p1, p2} {
		if _, err := m.Sync(ctx, SyncRequest{ClientID: "claude-code", Scope: ScopeProject, ProjectRoot: root}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, ".claude", "skills", a.ID)); err != nil {
			t.Fatalf("not materialized in %s: %v", root, err)
		}
	}
	view, err := m.Inspect(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Installs) != 2 {
		t.Fatalf("installs = %d, want one per project", len(view.Installs))
	}
	for _, in := range view.Installs {
		if in.State != StateApplied || in.Install.Scope != ScopeProject {
			t.Errorf("install = %+v", in)
		}
	}
}

func statMod(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime().UnixNano()
}

// TestSyncPruneIsContainerScoped: converging one project must never
// unmaterialize another. The receipt's container is what keeps them apart.
func TestSyncPruneIsContainerScoped(t *testing.T) {
	ctx := context.Background()
	m, _ := testManager(t)
	a := addNamed(t, m, "alpha")
	p1, p2 := t.TempDir(), t.TempDir()

	for _, root := range []string{p1, p2} {
		if _, err := m.Sync(ctx, SyncRequest{ClientID: "claude-code", Scope: ScopeProject, ProjectRoot: root}); err != nil {
			t.Fatal(err)
		}
	}
	// Deselect everything in p1 only.
	if _, err := m.Sync(ctx, SyncRequest{
		ClientID: "claude-code", Scope: ScopeProject, ProjectRoot: p1,
		Selector: &SkillSelector{Allow: []string{}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p1, ".claude", "skills", a.ID)); !os.IsNotExist(err) {
		t.Error("p1 was not pruned")
	}
	if _, err := os.Stat(filepath.Join(p2, ".claude", "skills", a.ID)); err != nil {
		t.Errorf("p2 was pruned by a sync of p1: %v", err)
	}
}
