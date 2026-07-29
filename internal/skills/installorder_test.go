package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveStateOrdersInstallsOnEveryKey pins all four keys of the receipt
// ordering in saveState.
//
// Why it matters beyond tidiness: saveIfChanged compares the fresh encoding
// against the bytes that were read, and writes only on a difference. That
// no-op guard is only as good as the ordering — if two receipts can swap
// places between loads, installs.json is rewritten on every save, and the
// file's mtime stops meaning "something was installed".
//
// The four keys exist because receipt identity is (skill, client, scope,
// container): the same skill installed into two projects is two receipts
// (see InstallState.Container), so real states routinely contain rows that
// agree on the first key and differ only on a later one. This test builds
// exactly that — every row shares a ClientID — and feeds it in reversed
// order, so any key that stops participating shows up as a byte difference.
//
// Established by mutation: reducing the comparator to ClientID alone leaves
// the whole package green, which is why this test is here rather than a
// comment saying the order is important.
func TestSaveStateOrdersInstallsOnEveryKey(t *testing.T) {
	rows := []InstallState{
		{ClientID: "claude", Scope: "project", Container: "/a/.claude/skills", SkillID: "alpha"},
		{ClientID: "claude", Scope: "project", Container: "/a/.claude/skills", SkillID: "beta"},
		{ClientID: "claude", Scope: "project", Container: "/b/.claude/skills", SkillID: "alpha"},
		{ClientID: "claude", Scope: "user", Container: "/a/.claude/skills", SkillID: "alpha"},
	}

	write := func(t *testing.T, in []InstallState) []byte {
		t.Helper()
		m, _ := testManager(t)
		st, err := m.loadState()
		if err != nil {
			t.Fatalf("loadState: %v", err)
		}
		st.installs.Installs = append([]InstallState(nil), in...)
		if err := m.saveState(st); err != nil {
			t.Fatalf("saveState: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(m.dir, installsFileName))
		if err != nil {
			t.Fatalf("reading %s: %v", installsFileName, err)
		}
		return data
	}

	forward := write(t, rows)

	reversed := make([]InstallState, len(rows))
	for i, r := range rows {
		reversed[len(rows)-1-i] = r
	}
	backward := write(t, reversed)

	if string(forward) != string(backward) {
		t.Fatalf("installs.json depends on the order receipts were appended in;\n"+
			"a key stopped participating in the sort\n--- appended forward ---\n%s\n--- appended reversed ---\n%s",
			forward, backward)
	}
}
