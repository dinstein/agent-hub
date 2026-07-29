package clients_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dinstein/agent-hub/internal/clients"
)

// TestBackupIsCentralAndPrivate: the pre-write copy lands under
// <data>/backups/clients, NOT beside the original. A project .mcp.json
// lives in a git worktree; a sidecar backup would dirty every status and
// risk committing another application's credentials. Mode is 0600 for the
// same reason.
func TestBackupIsCentralAndPrivate(t *testing.T) {
	e := newEnv(t, "darwin")
	path := filepath.Join(e.project, ".mcp.json")
	orig := `{"mcpServers":{"other":{"command":"npx","env":{"API_TOKEN":"s3cret"}}}}`
	write(t, path, orig)

	res, err := e.format(t, "claude-code").Connect(path, entry("claude-code"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if filepath.Dir(res.Backup) != e.backups {
		t.Errorf("backup %q is not in the central directory %q", res.Backup, e.backups)
	}
	if got := read(t, res.Backup); got != orig {
		t.Errorf("backup content = %q", got)
	}
	info, err := os.Stat(res.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %o, want 0600 (backups can hold credentials)", info.Mode().Perm())
	}
	if di, err := os.Stat(e.backups); err != nil {
		t.Fatal(err)
	} else if di.Mode().Perm() != 0o700 {
		t.Errorf("backup directory mode = %o, want 0700", di.Mode().Perm())
	}

	// No sidecar anywhere near the original.
	entries, err := os.ReadDir(e.project)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if ent.Name() != ".mcp.json" {
			t.Errorf("connect left debris in the project directory: %s", ent.Name())
		}
	}
}

// TestBackupRotation: only the newest KeepBackups copies per client
// survive, and rotation is per client — one noisy client never evicts
// another's history.
func TestBackupRotation(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	project := filepath.Join(root, "project")
	backups := filepath.Join(root, "backups")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	tbl := clients.New(clients.Options{
		GOOS: "darwin", Home: home, BackupDir: backups, KeepBackups: 3,
	})
	e := env{tbl: tbl, home: home, backups: backups, project: project}

	cc, _ := tbl.Lookup("claude-code")
	path := filepath.Join(project, ".mcp.json")
	write(t, path, `{"mcpServers":{}}`)

	// Six writes: each connect/disconnect pair changes the file, so each
	// produces exactly one backup.
	for i := 0; i < 3; i++ {
		if _, err := cc.Connect(path, entry("claude-code")); err != nil {
			t.Fatalf("connect %d: %v", i, err)
		}
		if _, err := cc.Disconnect(path); err != nil {
			t.Fatalf("disconnect %d: %v", i, err)
		}
	}
	got := backupsOf(t, e, "claude-code")
	if len(got) != 3 {
		t.Fatalf("kept %d backups, want 3: %v", len(got), got)
	}
	// Names are fixed-width timestamps: sorted order is chronological, and
	// the survivors must be the newest ones — the last backup taken is the
	// state right before the final write, i.e. the connected file.
	sort.Strings(got)
	if newest := read(t, got[len(got)-1]); !contains(newest, `"agenthub"`) {
		t.Errorf("newest surviving backup is not the most recent state:\n%s", newest)
	}

	// A second client keeps its own history.
	cursor, _ := tbl.Lookup("cursor")
	cpath := filepath.Join(project, ".cursor", "mcp.json")
	write(t, cpath, `{"mcpServers":{}}`)
	if _, err := cursor.Connect(cpath, entry("cursor")); err != nil {
		t.Fatal(err)
	}
	if n := len(backupsOf(t, e, "claude-code")); n != 3 {
		t.Errorf("claude-code history = %d after a cursor write, want 3", n)
	}
	if n := len(backupsOf(t, e, "cursor")); n != 1 {
		t.Errorf("cursor history = %d, want 1", n)
	}
}

// TestOversizedConfigRefused: a file above the 64 MiB limit is rejected on
// the stat, before it is read, and is left untouched. The sparse file
// makes this cheap; the point is that agenthub never allocates for it.
func TestOversizedConfigRefused(t *testing.T) {
	e := newEnv(t, "darwin")
	path := filepath.Join(e.project, ".mcp.json")
	write(t, path, `{"mcpServers":{}}`)
	if err := os.Truncate(path, clients.MaxConfigSize+1); err != nil {
		t.Skipf("cannot create a sparse %d byte file here: %v", clients.MaxConfigSize+1, err)
	}

	f := e.format(t, "claude-code")
	var te *clients.TooLargeError
	if _, err := f.Connect(path, entry("claude-code")); !errors.As(err, &te) {
		t.Fatalf("connect err = %v, want *TooLargeError", err)
	}
	if te.Limit != clients.MaxConfigSize || te.Size <= clients.MaxConfigSize {
		t.Errorf("TooLargeError = %+v", te)
	}
	if _, err := f.Disconnect(path); !errors.As(err, &te) {
		t.Fatalf("disconnect err = %v, want *TooLargeError", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != clients.MaxConfigSize+1 {
		t.Errorf("oversized file was modified: %+v (err=%v)", info, err)
	}
	if n := len(backupsOf(t, e, "claude-code")); n != 0 {
		t.Errorf("refusal wrote %d backups", n)
	}

	// Exactly at the limit is still refused only above it; a file one byte
	// under the cap must parse normally.
	small := filepath.Join(e.project, "small", ".mcp.json")
	write(t, small, `{"mcpServers":{}}`)
	if _, err := f.Connect(small, entry("claude-code")); err != nil {
		t.Errorf("normal file refused: %v", err)
	}
}

// TestWriteFailsClosedWithoutBackup: if the backup cannot be taken, the
// target file is left exactly as it was. Modifying a user's configuration
// without a recoverable copy is worse than not connecting at all.
func TestWriteFailsClosedWithoutBackup(t *testing.T) {
	requireUnprivileged(t)
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.MkdirAll(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	project := filepath.Join(root, "project")
	tbl := clients.New(clients.Options{
		GOOS: "darwin", Home: filepath.Join(root, "home"),
		BackupDir: filepath.Join(blocked, "backups"),
	})
	f, _ := tbl.Lookup("claude-code")
	path := filepath.Join(project, ".mcp.json")
	orig := `{"mcpServers":{"other":{"command":"npx"}}}`
	write(t, path, orig)

	if _, err := f.Connect(path, entry("claude-code")); err == nil {
		t.Fatal("connect succeeded with an unwritable backup directory")
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want a permission failure", err)
	}
	if got := read(t, path); got != orig {
		t.Errorf("file modified although the backup failed:\n%s", got)
	}
}
