package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSkillMD = `---
name: PDF Tools
description: Extract text from PDFs
version: 1.0.0
---

Use pdftotext.
`

// writeSkillPackage materializes a minimal SKILL.md package to import.
func writeSkillPackage(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// skillEnv isolates both the data dir and HOME: user-scope materialization
// resolves against HOME, and a test must never write into the developer's
// real ~/.claude.
func skillEnv(t *testing.T) (dataDir, home string) {
	t.Helper()
	dataDir = setDataDir(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	return dataDir, home
}

func TestSkillLifecycle(t *testing.T) {
	skillEnv(t)
	src := writeSkillPackage(t, testSkillMD)

	var added SkillRow
	decodeInto(t, mustRun(t, "", "skill", "add", src, "--json"), &added)
	if added.ID == "" || added.Name != "PDF Tools" || !added.Enabled {
		t.Fatalf("add = %+v", added)
	}
	if added.Granularity != "client" {
		t.Errorf("granularity = %q, want client (file materialization cannot be per-session)", added.Granularity)
	}
	if added.Library != "ok" {
		t.Errorf("a freshly imported entry must be pinned and ok, got %q", added.Library)
	}
	id := added.ID

	var list SkillList
	decodeInto(t, mustRun(t, "", "skill", "ls", "--json"), &list)
	if len(list.Skills) != 1 || list.Skills[0].ID != id {
		t.Fatalf("ls = %+v", list.Skills)
	}

	var insp SkillRow
	decodeInto(t, mustRun(t, "", "skill", "inspect", id, "--json"), &insp)
	if len(insp.Files) == 0 {
		t.Errorf("inspect must list the package files: %+v", insp)
	}
	if insp.Fingerprint == "" || !strings.HasPrefix(insp.Fingerprint, "v1:") {
		t.Errorf("fingerprint = %q, want a versioned fingerprint", insp.Fingerprint)
	}

	mustRun(t, "", "skill", "disable", id)
	decodeInto(t, mustRun(t, "", "skill", "ls", "--json"), &list)
	if list.Skills[0].Enabled {
		t.Errorf("disable did not stick: %+v", list.Skills[0])
	}
	mustRun(t, "", "skill", "enable", id)

	var verify SkillVerifyReport
	decodeInto(t, mustRun(t, "", "skill", "verify", "--json"), &verify)
	if !verify.OK || len(verify.Skills) != 1 {
		t.Fatalf("verify = %+v", verify)
	}

	var removed SkillAction
	decodeInto(t, mustRun(t, "", "skill", "rm", id, "--json"), &removed)
	if removed.SkillID != id {
		t.Errorf("rm = %+v", removed)
	}
	decodeInto(t, mustRun(t, "", "skill", "ls", "--json"), &list)
	if len(list.Skills) != 0 {
		t.Errorf("entry survived rm: %+v", list.Skills)
	}
	if code, _, _ := runCLI(t, "", "skill", "inspect", id); code != ExitNotFound {
		t.Errorf("inspect after rm exit = %d, want %d", code, ExitNotFound)
	}
}

// TestSkillVerifyDetectsTampering pins the fail direction: fingerprints are
// recomputed FROM THE BYTES, so editing the library copy is caught and the
// command exits non-zero.
func TestSkillVerifyDetectsTampering(t *testing.T) {
	dataDir, _ := skillEnv(t)
	src := writeSkillPackage(t, testSkillMD)

	var added SkillRow
	decodeInto(t, mustRun(t, "", "skill", "add", src, "--json"), &added)

	// Edit the canonical copy behind agenthub's back.
	var edited bool
	root := filepath.Join(dataDir, "skills", "store")
	err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() || filepath.Base(path) != "SKILL.md" {
			return nil //nolint:nilerr // keep walking
		}
		if werr := os.WriteFile(path, []byte(testSkillMD+"\nrm -rf /\n"), 0o644); werr != nil {
			return werr
		}
		edited = true
		return nil
	})
	if err != nil || !edited {
		t.Fatalf("could not tamper with the library copy (err=%v edited=%v)", err, edited)
	}

	code, out, _ := runCLI(t, "", "skill", "verify", "--json")
	if code != ExitGeneral {
		t.Fatalf("exit = %d, want 1 (tampered library)\n%s", code, out)
	}
	var report SkillVerifyReport
	decodeInto(t, out, &report)
	if report.OK {
		t.Fatalf("verify reported ok on a tampered copy: %+v", report)
	}
	if len(report.Skills) != 1 || report.Skills[0].Library != "tampered" {
		t.Errorf("verify = %+v, want library=tampered", report.Skills)
	}
}

// TestSkillInstallToAndSync covers materialization into a client target and
// the convergence semantics of sync.
func TestSkillInstallToAndSync(t *testing.T) {
	_, home := skillEnv(t)
	src := writeSkillPackage(t, testSkillMD)
	var added SkillRow
	decodeInto(t, mustRun(t, "", "skill", "add", src, "--json"), &added)
	id := added.ID

	var plan SkillSyncResult
	decodeInto(t, mustRun(t, "", "skill", "install-to", id, "--client", "claude-code", "--dry-run", "--json"), &plan)
	if len(plan.Items) != 1 || plan.Items[0].Action != "plan" {
		t.Fatalf("dry-run = %+v", plan)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("--dry-run wrote to disk (err=%v)", err)
	}

	var installed SkillSyncResult
	decodeInto(t, mustRun(t, "", "skill", "install-to", id, "--client", "claude-code", "--json"), &installed)
	if len(installed.Items) != 1 || installed.Items[0].Path == "" {
		t.Fatalf("install-to = %+v", installed)
	}
	if _, err := os.Stat(installed.Items[0].Path); err != nil {
		t.Errorf("materialized path missing: %v", err)
	}

	// Sync is idempotent: a second run converges without writing.
	mustRun(t, "", "skill", "sync", "claude-code")
	var second SkillSyncResult
	decodeInto(t, mustRun(t, "", "skill", "sync", "claude-code", "--json"), &second)
	if second.Changed {
		t.Errorf("a converged sync must not report a change: %+v", second)
	}
	if second.Granularity != "client" {
		t.Errorf("granularity = %q, want client", second.Granularity)
	}

	// Disabling then syncing unmaterializes (converge, not accumulate).
	mustRun(t, "", "skill", "disable", id)
	var pruned SkillSyncResult
	decodeInto(t, mustRun(t, "", "skill", "sync", "claude-code", "--json"), &pruned)
	if !pruned.Changed {
		t.Fatalf("sync after disable did not converge: %+v", pruned)
	}
	if _, err := os.Stat(installed.Items[0].Path); !os.IsNotExist(err) {
		t.Errorf("disabled skill is still materialized (err=%v)", err)
	}
}

func TestSkillUpdateCheck(t *testing.T) {
	skillEnv(t)
	src := writeSkillPackage(t, testSkillMD)
	var added SkillRow
	decodeInto(t, mustRun(t, "", "skill", "add", src, "--json"), &added)

	var noop SkillUpdateResult
	decodeInto(t, mustRun(t, "", "skill", "update", added.ID, "--check", "--json"), &noop)
	if noop.Changed || !noop.Check {
		t.Errorf("unchanged --check = %+v", noop)
	}

	if err := os.WriteFile(filepath.Join(src, "SKILL.md"),
		[]byte(strings.Replace(testSkillMD, "1.0.0", "2.0.0", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	var checked SkillUpdateResult
	decodeInto(t, mustRun(t, "", "skill", "update", added.ID, "--check", "--json"), &checked)
	if !checked.Changed {
		t.Fatalf("--check did not notice the new source: %+v", checked)
	}
	// --check writes nothing.
	var stillOld SkillRow
	decodeInto(t, mustRun(t, "", "skill", "inspect", added.ID, "--json"), &stillOld)
	if stillOld.Version != "1.0.0" {
		t.Errorf("--check mutated the library: %+v", stillOld)
	}

	var applied SkillUpdateResult
	decodeInto(t, mustRun(t, "", "skill", "update", added.ID, "--json"), &applied)
	if !applied.Changed || applied.ToVersion != "2.0.0" {
		t.Errorf("update = %+v", applied)
	}
}

func TestSkillUnknownIDIsExit3(t *testing.T) {
	skillEnv(t)
	for _, args := range [][]string{
		{"skill", "inspect", "ghost"},
		{"skill", "rm", "ghost"},
		{"skill", "enable", "ghost"},
		{"skill", "disable", "ghost"},
		{"skill", "update", "ghost"},
		{"skill", "install-to", "ghost", "--client", "claude-code"},
	} {
		if code, _, _ := runCLI(t, "", args...); code != ExitNotFound {
			t.Errorf("%v exit = %d, want %d", args, code, ExitNotFound)
		}
	}
}
