package clients

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The splice's safety argument, tested where it is actually made.
//
// jsonc_test.go proves the two components in isolation: spliceEntry refuses a
// shape it cannot walk, and verifySplice catches an edit that landed in the
// wrong place. Neither says anything about what reaches the DISK — and "a
// locator bug costs the user a refusal, not their settings" is a claim about
// the file, not about a return value.
//
// spliceWrite is the join, and it is the only place both refusals turn into
// the *ParseError-with-snippet a caller renders. These tests drive it with an
// edit that fails in each of the two ways, and then look at the file.

// spliceEnv is a JSONC document on disk plus the format that owns it.
type spliceEnv struct {
	f       *jsonFormat
	cfg     *jsonConfig
	path    string
	backups string
	orig    string
}

func newSpliceEnv(t *testing.T) spliceEnv {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	backups := filepath.Join(root, "backups")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	tbl := New(Options{GOOS: "darwin", Home: home, BackupDir: backups, NoDelegate: true})
	format, ok := tbl.Lookup("vscode")
	if !ok {
		t.Fatal("vscode is not registered")
	}
	f, ok := format.(*jsonFormat)
	if !ok {
		t.Fatalf("vscode is a %T, not the JSON format this test drives", format)
	}

	// A document with comments, so read() marks it jsonc and the write path
	// is the splice rather than a re-encode.
	orig := "{\n  // my servers\n  \"servers\": {\n    \"linear\": {\"command\": \"npx\"}\n  }\n}\n"
	path := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.read(f.spec.locationFor(f.tbl, "", path))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !cfg.jsonc {
		t.Fatal("setup: the document was not taken as JSONC, so this test would not exercise the splice")
	}
	return spliceEnv{f: f, cfg: cfg, path: path, backups: backups, orig: orig}
}

// untouched asserts the whole point: the file is byte-identical and no backup
// was taken. A backup is itself a write, and one left behind by an edit that
// never happened is a file the user has to reason about for no reason.
func (e spliceEnv) untouched(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(got) != e.orig {
		t.Errorf("the file was modified by an edit that was refused:\n%s", got)
	}
	entries, err := os.ReadDir(e.backups)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the backup directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused edit left %d backup(s) behind", len(entries))
	}
}

// wantRefusal checks the shape a caller acts on. The snippet is the whole
// value of the refusal — "agenthub will not edit this" without "here is what
// to paste" is a dead end — so its absence is a failure, not a detail.
func wantRefusal(t *testing.T, err error) {
	t.Helper()
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *ParseError", err, err)
	}
	if pe.Hint == "" {
		t.Error("the refusal carries no hint")
	}
	if pe.Snippet == "" {
		t.Error("the refusal carries no manual snippet, so the user is told no way forward")
	}
}

// TestSpliceWriteRefusesAnUnspliceableShape: the locator gave up. Nothing is
// written, and the caller gets the same refusal the client used to get before
// agenthub could splice at all.
func TestSpliceWriteRefusesAnUnspliceableShape(t *testing.T) {
	t.Parallel()
	e := newSpliceEnv(t)

	_, err := e.f.spliceWrite(e.cfg, func([]byte) ([]byte, error) {
		return nil, errJSONCUnsupported
	}, []string{entryName})

	wantRefusal(t, err)
	e.untouched(t)
}

// TestSpliceWriteRefusesAnEditThatVerificationRejects is the case the design
// actually leans on. Here the edit SUCCEEDS and returns plausible bytes — it
// is a locator bug, not a locator refusal — and only verifySplice stands
// between it and the user's settings file. This one changes a key that was
// never ours to touch.
func TestSpliceWriteRefusesAnEditThatVerificationRejects(t *testing.T) {
	t.Parallel()
	e := newSpliceEnv(t)

	_, err := e.f.spliceWrite(e.cfg, func([]byte) ([]byte, error) {
		// Rewrites a foreign entry rather than adding our own.
		return []byte("{\n  // my servers\n  \"servers\": {\n" +
			"    \"linear\": {\"command\": \"HIJACKED\"}\n  }\n}\n"), nil
	}, []string{entryName})

	wantRefusal(t, err)
	e.untouched(t)
}

// TestSpliceWriteRefusesAnEditThatDropsAComment. Comments are the reason the
// splice exists — a re-encode would lose them, which is why this document is
// not re-encoded — so an edit that loses them anyway must not be written,
// even though the JSON it produces is otherwise correct.
func TestSpliceWriteRefusesAnEditThatDropsAComment(t *testing.T) {
	t.Parallel()
	e := newSpliceEnv(t)

	_, err := e.f.spliceWrite(e.cfg, func(src []byte) ([]byte, error) {
		good, serr := spliceEntry(src, e.cfg.loc.Section, entryName,
			map[string]any{"command": "agenthub"})
		if serr != nil {
			return nil, serr
		}
		// Correct in every way except the one the splice exists to protect.
		return []byte(dropFirstLine(string(good))), nil
	}, []string{entryName})

	wantRefusal(t, err)
	e.untouched(t)
}

// TestSpliceWriteReportsNoChangeWithoutWriting: an edit that produces exactly
// the bytes that were already there is "already up to date", not a write. It
// must not take a backup either — re-running connect on a settled file would
// otherwise mint a backup every time.
func TestSpliceWriteReportsNoChangeWithoutWriting(t *testing.T) {
	t.Parallel()
	e := newSpliceEnv(t)

	res, err := e.f.spliceWrite(e.cfg, func(src []byte) ([]byte, error) {
		return src, nil
	}, []string{entryName})
	if err != nil {
		t.Fatalf("an identical edit was refused: %v", err)
	}
	if res.Changed {
		t.Error("an edit that changed nothing reported Changed")
	}
	if res.Backup != "" {
		t.Errorf("an edit that changed nothing took backup %q", res.Backup)
	}
	e.untouched(t)
}

// dropFirstLine removes the leading comment line, and nothing else.
func dropFirstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[i+1:]
		}
	}
	return s
}
