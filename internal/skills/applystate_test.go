package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bump rewrites the source package so the next Update produces a new
// library version.
func bump(t *testing.T, src, marker string) {
	t.Helper()
	writeTree(t, src, map[string]string{
		SkillFileName: strings.Replace(sampleSkillMD, "pdftotext", marker, 1),
	})
}

// TestApplyStateMatrixOwnedDir walks every ApplyState an owned-dir install
// can reach. Each case perturbs exactly one thing, so a wrong precedence
// rule shows up as a specific mismatch rather than a vague failure.
func TestApplyStateMatrixOwnedDir(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		perturb func(t *testing.T, m *Manager, sk *Skill, src, dir string)
		want    ApplyState
	}{
		{"applied", func(*testing.T, *Manager, *Skill, string, string) {}, StateApplied},
		{"stale", func(t *testing.T, m *Manager, sk *Skill, src, _ string) {
			bump(t, src, "pdfimages")
			if _, err := m.Update(ctx, UpdateRequest{ID: sk.ID}); err != nil {
				t.Fatal(err)
			}
		}, StateStale},
		{"drifted", func(t *testing.T, _ *Manager, _ *Skill, _, dir string) {
			if err := os.WriteFile(filepath.Join(dir, SkillFileName), []byte("hand edited\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, StateDrifted},
		{"drifted by deletion", func(t *testing.T, _ *Manager, _ *Skill, _, dir string) {
			if err := os.Remove(filepath.Join(dir, "ref", "notes.md")); err != nil {
				t.Fatal(err)
			}
		}, StateDrifted},
		{"missing", func(t *testing.T, _ *Manager, _ *Skill, _, dir string) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}, StateMissing},
		{"conflict: marker gone", func(t *testing.T, _ *Manager, _ *Skill, _, dir string) {
			if err := os.Remove(filepath.Join(dir, MarkerFileName)); err != nil {
				t.Fatal(err)
			}
		}, StateConflict},
		{"conflict: marker names another skill", func(t *testing.T, _ *Manager, _ *Skill, _, dir string) {
			body := `{"version":1,"managedBy":"agenthub","skillId":"other"}`
			if err := os.WriteFile(filepath.Join(dir, MarkerFileName), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}, StateConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, home := testManager(t)
			sk, src := addSample(t, m)
			if _, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "claude-code"}); err != nil {
				t.Fatal(err)
			}
			dir := installedPath(home, sk.ID)
			tc.perturb(t, m, sk, src, dir)

			view, err := m.Inspect(ctx, sk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Installs) != 1 {
				t.Fatalf("installs = %d", len(view.Installs))
			}
			if got := view.Installs[0].State; got != tc.want {
				t.Errorf("state = %q, want %q (detail: %s)", got, tc.want, view.Installs[0].Detail)
			}
		})
	}
}

// TestApplyStateMatrixSentinel is the same matrix for the shared-file
// strategy, where "missing" and "conflict" have different shapes.
func TestApplyStateMatrixSentinel(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		perturb func(t *testing.T, m *Manager, sk *Skill, src, path string)
		want    ApplyState
	}{
		{"applied", func(*testing.T, *Manager, *Skill, string, string) {}, StateApplied},
		{"stale", func(t *testing.T, m *Manager, sk *Skill, src, _ string) {
			bump(t, src, "pdfimages")
			if _, err := m.Update(ctx, UpdateRequest{ID: sk.ID}); err != nil {
				t.Fatal(err)
			}
		}, StateStale},
		{"drifted", func(t *testing.T, _ *Manager, sk *Skill, _, path string) {
			body := mustRead(t, path)
			body = strings.Replace(body, "pdftotext", "hand edited", 1)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}, StateDrifted},
		{"missing: block removed", func(t *testing.T, _ *Manager, sk *Skill, _, path string) {
			body, _, err := removeBlockFrom(mustRead(t, path), sk.ID, path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}, StateMissing},
		{"missing: file gone", func(t *testing.T, _ *Manager, _ *Skill, _, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, StateMissing},
		{"conflict: sentinels damaged", func(t *testing.T, _ *Manager, sk *Skill, _, path string) {
			body := strings.Replace(mustRead(t, path), endMarker(sk.ID), "", 1)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}, StateConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, home := testManager(t)
			sk, src := addSample(t, m)
			if _, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "cursor"}); err != nil {
				t.Fatal(err)
			}
			tc.perturb(t, m, sk, src, cursorFile(home))

			view, err := m.Inspect(ctx, sk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := view.Installs[0].State; got != tc.want {
				t.Errorf("state = %q, want %q (detail: %s)", got, tc.want, view.Installs[0].Detail)
			}
		})
	}
}

// TestDriftRefusesOverwrite: a locally edited copy is never silently
// reverted; the caller must say AllowDrift.
func TestDriftRefusesOverwrite(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	sk, _ := addSample(t, m)
	if _, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(installedPath(home, sk.ID), SkillFileName)
	if err := os.WriteFile(target, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "claude-code"}); err == nil {
		t.Fatal("drifted install was overwritten without AllowDrift")
	} else if !errors.Is(err, ErrDrifted) {
		t.Fatalf("err = %v, want ErrDrifted", err)
	}
	if got := mustRead(t, target); got != "mine\n" {
		t.Errorf("local edit was destroyed: %q", got)
	}
	if _, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "claude-code", AllowDrift: true}); err != nil {
		t.Fatalf("forced install: %v", err)
	}
	if got := mustRead(t, target); !strings.Contains(got, "pdftotext") {
		t.Errorf("forced install did not converge: %q", got)
	}
}

// TestVerifyPersistsState: Verify is the command whose job is to leave the
// receipts telling the truth, so the refreshed state must be written back
// (List, by contrast, must not write).
func TestVerifyPersistsState(t *testing.T) {
	ctx := context.Background()
	m, home := testManager(t)
	sk, _ := addSample(t, m)
	if _, err := m.InstallTo(ctx, InstallRequest{SkillID: sk.ID, ClientID: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(installedPath(home, sk.ID)); err != nil {
		t.Fatal(err)
	}

	before := mustRead(t, filepath.Join(m.Dir(), installsFileName))
	if _, err := m.List(ctx, ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if after := mustRead(t, filepath.Join(m.Dir(), installsFileName)); after != before {
		t.Error("List wrote to the receipts file; listing must be read-only")
	}

	rep, err := m.Verify(ctx, VerifyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK {
		t.Error("verify reported OK with a missing install")
	}
	if rep.Granularity != GranularityClient {
		t.Errorf("granularity = %q", rep.Granularity)
	}
	if got := mustRead(t, filepath.Join(m.Dir(), installsFileName)); !strings.Contains(got, string(StateMissing)) {
		t.Errorf("verify did not persist the refreshed state:\n%s", got)
	}
}
