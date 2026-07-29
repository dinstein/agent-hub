package clients

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// DefaultKeepBackups is how many pre-write copies are retained per client.
// Deep history is not the goal: the backup exists so the LAST write can be
// undone, and an unbounded pile of other applications' configuration
// (which routinely embeds API tokens) is a liability, not a feature.
const DefaultKeepBackups = 10

// backupTimeLayout produces fixed-width, lexicographically sortable names,
// which is what makes rotation a plain sort of the directory listing.
const backupTimeLayout = "20060102T150405.000000"

// backupDir resolves the central backup directory, creating it with 0700.
//
// Central, not a sidecar next to the original: a project-level .mcp.json
// lives in a git worktree, and dropping a .mcp.json.agenthub-backup beside
// it means every connect dirties `git status` (and risks committing another
// application's credentials).
func (t *Table) backupDir() (string, error) {
	dir := t.opts.BackupDir
	if dir == "" {
		data, err := platform.DataDir()
		if err != nil {
			return "", fmt.Errorf("clients: resolve backup directory: %w", err)
		}
		dir = BackupDir(data)
	}
	if err := platform.EnsureDir(dir); err != nil {
		return "", fmt.Errorf("clients: backup directory: %w", err)
	}
	return dir, nil
}

func (t *Table) keepBackups() int {
	if t.opts.KeepBackups > 0 {
		return t.opts.KeepBackups
	}
	return DefaultKeepBackups
}

// backup copies data to <backups>/clients/<client>-<ts>Z.json before the
// caller overwrites source, then rotates older copies away. It returns the
// backup path.
//
// Mode 0600: client configurations frequently carry API tokens in env
// blocks, so a copy of one is as sensitive as the vault.
func (t *Table) backup(clientID, source string, data []byte) (string, error) {
	dir, err := t.backupDir()
	if err != nil {
		return "", err
	}
	prefix := backupPrefix(clientID)
	stamp := time.Now().UTC().Format(backupTimeLayout) + "Z"

	// O_EXCL so two concurrent connects can never write the same file;
	// the suffix loop resolves a same-microsecond collision.
	var path string
	var fh *os.File
	for i := 0; ; i++ {
		name := prefix + stamp + ".json"
		if i > 0 {
			name = fmt.Sprintf("%s%s-%d.json", prefix, stamp, i)
		}
		path = filepath.Join(dir, name)
		fh, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || i >= 100 {
			return "", fmt.Errorf("clients: create backup for %s: %w", source, err)
		}
	}
	if _, err := fh.Write(data); err != nil {
		_ = fh.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("clients: write backup %s: %w", path, err)
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("clients: sync backup %s: %w", path, err)
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("clients: close backup %s: %w", path, err)
	}

	// Rotation is best-effort: losing an old copy must never fail a
	// connect that already has its fresh backup safely on disk.
	t.rotate(dir, prefix)
	return path, nil
}

// rotate deletes all but the newest keepBackups files for one client.
// Names are fixed-width timestamps, so lexicographic order is chronological.
func (t *Table) rotate(dir, prefix string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".json") {
			names = append(names, n)
		}
	}
	keep := t.keepBackups()
	if len(names) <= keep {
		return
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-keep] {
		_ = os.Remove(filepath.Join(dir, n))
	}
}

// backupPrefix is the file-name prefix for one client. The trailing '-'
// is part of the prefix so that client "cursor" never matches a file of a
// hypothetical client "cursor-nightly".
func backupPrefix(clientID string) string { return clientID + "-" }
