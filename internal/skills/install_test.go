package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOwnedDirRoundTrip: install materializes the package, rebuild removes
// strays, and remove takes the whole directory back out.
func TestOwnedDirRoundTrip(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	sk, _ := addSample(t, m)

	rec, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "claude-code"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	dir := installedPath(home, sk.ID)
	if rec.Path != dir {
		t.Fatalf("installed at %s, want %s", rec.Path, dir)
	}
	if rec.Granularity != GranularityClient {
		t.Errorf("granularity = %q, want %q (docs/subsystems/skills.md)", rec.Granularity, GranularityClient)
	}
	if got := mustRead(t, filepath.Join(dir, SkillFileName)); !strings.Contains(got, "pdftotext") {
		t.Errorf("SKILL.md not materialized: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "ref", "notes.md")); err != nil {
		t.Errorf("attachment not materialized: %v", err)
	}
	mk, err := readMarker(dir)
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if mk.SkillID != sk.ID || mk.ContentHash != sk.ContentHash || mk.Granularity != GranularityClient {
		t.Errorf("marker = %+v", mk)
	}

	// A stray file inside an owned directory is drift, and re-applying with
	// AllowDrift must rebuild the directory rather than merge into it.
	stray := filepath.Join(dir, "stray.txt")
	if err := os.WriteFile(stray, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "claude-code", AllowDrift: true}); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if _, err := os.Stat(stray); !errors.Is(err, os.ErrNotExist) {
		t.Error("rebuild kept a stray file; owned dirs must be rebuildable")
	}

	res, err := m.Remove(ctx, RemoveRequest{ID: sk.ID})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(res.RemovedInstalls) != 1 {
		t.Errorf("removed installs = %v", res.RemovedInstalls)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("installed directory survived remove")
	}
}

// TestOwnedDirRefusesForeignDirectory: a directory without our marker is
// somebody else's. Never absorbed, never rebuilt.
func TestOwnedDirRefusesForeignDirectory(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	sk, _ := addSample(t, m)

	dir := installedPath(home, sk.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(dir, "mine.md")
	if err := os.WriteFile(precious, []byte("hand written"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := m.Plan(ctx, InstallRequest{SkillID: sk.ID, ClientID: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateConflict {
		t.Errorf("plan state = %q, want %q", p.State, StateConflict)
	}
	_, err = m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "claude-code"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("install err = %v, want ErrConflict", err)
	}
	if got := mustRead(t, precious); got != "hand written" {
		t.Errorf("user file was touched: %q", got)
	}
}

// TestSentinelInstallPreservesFile is the shared-file contract end to end.
func TestSentinelInstallPreservesFile(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	sk, src := addSample(t, m)

	path := cursorFile(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(userRules), 0o600); err != nil {
		t.Fatal(err)
	}

	rec, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "cursor"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	got := mustRead(t, path)
	if !strings.HasPrefix(got, userRules) {
		t.Fatalf("user rules disturbed:\n%s", got)
	}
	if !strings.Contains(got, "pdftotext") {
		t.Errorf("skill body missing:\n%s", got)
	}
	if !strings.Contains(got, "ref/notes.md") {
		t.Error("attachment note missing: sentinel installs must admit what they cannot materialize")
	}
	if rec.Strategy != StrategySentinelBlock {
		t.Errorf("strategy = %q", rec.Strategy)
	}

	// Update the library, re-sync, then remove: the user's bytes must come
	// back exactly.
	writeTree(t, src, map[string]string{SkillFileName: strings.Replace(sampleSkillMD, "pdftotext", "pdfimages", 1)})
	if _, err := m.Update(ctx, UpdateRequest{ID: sk.ID, Reapply: true}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got = mustRead(t, path)
	if strings.Contains(got, "pdftotext") || !strings.Contains(got, "pdfimages") {
		t.Errorf("reapply did not refresh the block:\n%s", got)
	}
	if _, err := m.Remove(ctx, RemoveRequest{ID: sk.ID}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := mustRead(t, path); got != userRules {
		t.Errorf("user file not restored:\n%q\nwant\n%q", got, userRules)
	}
}

// TestSentinelInstallRefusesDamagedMarkers: a hand-mangled block stops the
// write, and the file is left byte-identical.
func TestSentinelInstallRefusesDamagedMarkers(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	sk, _ := addSample(t, m)

	path := cursorFile(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	damaged := userRules + startMarker(sk.ID) + "\nhalf a block\n"
	if err := os.WriteFile(path, []byte(damaged), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := m.Plan(ctx, InstallRequest{SkillID: sk.ID, ClientID: "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateConflict {
		t.Errorf("plan state = %q, want %q", p.State, StateConflict)
	}
	_, err = m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "cursor"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("install err = %v, want ErrConflict", err)
	}
	if got := mustRead(t, path); got != damaged {
		t.Errorf("file was rewritten despite the refusal:\n%q", got)
	}
}

// TestSentinelCharCapRefuses: a rendered file over the target cap is a
// conflict, because the client would silently truncate it.
func TestSentinelCharCapRefuses(t *testing.T) {
	ctx := context.Background()
	tiny := TargetDef{
		ClientID: "tinycap", Supports: []SkillKind{KindSkillPack},
		Strategy: StrategySentinelBlock, SentinelFile: "rules.md", CharCap: 40,
		UserDir: func(home string, _ SkillKind) (string, error) { return filepath.Join(home, "tiny"), nil },
	}
	m, _ := testManager(t, func(o *Options) { o.ExtraTargets = []TargetDef{tiny} })
	sk, _ := addSample(t, m)

	_, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "tinycap"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "character cap") {
		t.Errorf("err = %v", err)
	}
}

// TestBlockedByShadowFile: a shadowing file makes our write invisible, so
// it is a conflict rather than a receipt that lies.
func TestBlockedByShadowFile(t *testing.T) {
	ctx := context.Background()
	shadowed := TargetDef{
		ClientID: "shadowed", Supports: []SkillKind{KindSkillPack},
		Strategy: StrategyOwnedDir, BlockedIf: []string{"OVERRIDE.md"},
		UserDir: func(home string, _ SkillKind) (string, error) { return filepath.Join(home, "sh"), nil },
	}
	m, home := testManager(t, func(o *Options) { o.ExtraTargets = []TargetDef{shadowed} })
	sk, _ := addSample(t, m)

	container := filepath.Join(home, "sh")
	if err := os.MkdirAll(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "OVERRIDE.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := m.Plan(ctx, InstallRequest{SkillID: sk.ID, ClientID: "shadowed"})
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateConflict || !strings.Contains(p.Detail, "OVERRIDE.md") {
		t.Errorf("plan = %+v, want a shadow conflict", p)
	}
}

// TestGenericTargetNeedsDirectory: the escape-hatch target refuses to guess.
func TestGenericTargetNeedsDirectory(t *testing.T) {
	ctx := context.Background()
	m, _ := testManager(t)
	sk, _ := addSample(t, m)

	if _, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: GenericTargetID}); err == nil {
		t.Fatal("generic target installed without a directory")
	}
	dst := filepath.Join(t.TempDir(), "elsewhere")
	rec, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: GenericTargetID, Dir: dst})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if rec.Path != filepath.Join(dst, sk.ID) {
		t.Errorf("path = %s", rec.Path)
	}
}

// TestUnsupportedKindRefused: a target must never be handed a kind it does
// not read.
func TestUnsupportedKindRefused(t *testing.T) {
	ctx := context.Background()
	m, _ := testManager(t)
	src := writeTree(t, t.TempDir(), map[string]string{"agent.md": "an agent\n"})
	sk, err := m.Add(ctx, AddRequest{Path: src, Name: "agent def", Kind: KindAgentDef})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "claude-code"})
	if !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("err = %v, want ErrUnsupportedKind", err)
	}
}
