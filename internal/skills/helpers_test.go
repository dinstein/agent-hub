package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixedClock keeps every timestamp deterministic so golden output and
// no-op-guard assertions do not depend on wall time.
func fixedClock() func() time.Time {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return base }
}

// testManager returns a Manager rooted in a temp dir with a fake home, so
// user-scope target conventions resolve inside the test sandbox.
func testManager(t *testing.T, mutate ...func(*Options)) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := Options{Now: fixedClock(), HomeDir: home, AgentVersion: "test"}
	for _, f := range mutate {
		f(&opts)
	}
	m, err := Open(filepath.Join(root, "skills"), opts)
	if err != nil {
		t.Fatal(err)
	}
	return m, home
}

// writeTree materializes a source package for import.
func writeTree(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// sampleSkillMD is the canonical fixture manifest.
const sampleSkillMD = `---
name: PDF Tools
description: Extract text from PDFs
version: 1.0.0
---

Use pdftotext.
`

// addSample imports a one-file skill and returns it plus its source dir.
func addSample(t *testing.T, m *Manager) (*Skill, string) {
	t.Helper()
	src := writeTree(t, t.TempDir(), map[string]string{
		SkillFileName:  sampleSkillMD,
		"ref/notes.md": "notes\n",
	})
	sk, err := m.Add(context.Background(), AddRequest{Path: src})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return sk, src
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// installedPath is where a claude-code user-scope install lands.
func installedPath(home, id string) string {
	return filepath.Join(home, ".claude", "skills", id)
}

// cursorFile is where a cursor user-scope install lands.
func cursorFile(home string) string {
	return filepath.Join(home, ".cursor", "rules", "agenthub.mdc")
}
