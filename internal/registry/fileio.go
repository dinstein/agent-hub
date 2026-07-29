package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// backupDepth is the number of rolling backup generations kept per
	// document under <dir>/backups/<name>.json.1 .. .<backupDepth>.
	backupDepth = 5

	// backupsDirName is the subdirectory holding rolling backups.
	backupsDirName = "backups"

	// readRetries / readRetryDelay: a parse failure is retried this many
	// times (re-reading the file each time) before the file is declared
	// unreadable. This rides out non-atomic writers caught mid-write.
	readRetries    = 4
	readRetryDelay = 75 * time.Millisecond
)

// atomicWrite persists data to path with the full hardening ladder:
// same-directory temp file, chmod 0600, write, fsync, rename over the target,
// fsync of the parent directory. Never leaves a partially written target.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so a preceding rename is durable. Filesystems
// that do not support fsync on directories (returns EINVAL/ENOTSUP) are
// tolerated — the rename itself is still atomic there.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return err
	}
	return nil
}

// rotateBackups shifts <name>.json.1..4 to .2..5 (dropping .5) and stores
// prev — the content being replaced — as generation .1. Called only when a
// real (non-no-op) write is about to happen, so backups always hold distinct
// generations.
func rotateBackups(dir string, base string, prev []byte) error {
	bdir := filepath.Join(dir, backupsDirName)
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return err
	}
	slot := func(i int) string { return filepath.Join(bdir, fmt.Sprintf("%s.%d", base, i)) }
	for i := backupDepth - 1; i >= 1; i-- {
		if err := os.Rename(slot(i), slot(i+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.WriteFile(slot(1), prev, 0o600)
}

// quarantine renames an unparseable file to <name>.unreadable-<timestamp>
// beside the original, preserving its content for post-mortem instead of
// destroying it. Returns the quarantine path.
func quarantine(path string) (string, error) {
	ts := time.Now().UTC().Format("20060102T150405.000000000Z")
	qpath := fmt.Sprintf("%s.unreadable-%s", path, ts)
	if err := os.Rename(path, qpath); err != nil {
		return "", err
	}
	return qpath, syncDir(filepath.Dir(path))
}

// canonicalize reduces a JSON byte stream to its canonical encoding: numbers
// preserved verbatim (json.Number), objects with sorted keys, no
// insignificant whitespace. Used by the no-op guard to compare parsed values
// rather than raw bytes.
func canonicalize(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// canonicallyEqual reports whether two JSON documents encode the same value.
// Malformed input is treated as unequal (forcing a rewrite, the safe
// direction for a persistence layer).
func canonicallyEqual(a, b []byte) bool {
	ca, err := canonicalize(a)
	if err != nil {
		return false
	}
	cb, err := canonicalize(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

// encodeDoc renders a document for disk: two-space indent, trailing newline.
// Deterministic because Doc.MarshalJSON emits sorted keys at every level.
func encodeDoc(doc any) ([]byte, error) {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
