package httpbridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Storage primitives for the token store. They are a deliberate (small)
// duplicate of internal/registry's ladder rather than an import: the
// registry's helpers are unexported and the token file is NOT a registry
// document — it holds credential digests, has no generation, no Doc[T]
// envelope and no watch semantics. Sharing the code would mean exporting
// registry internals to make a security artefact look like configuration.

// lockPollInterval is how often a blocked acquirer retries the non-blocking
// flock before its deadline.
const lockPollInterval = 5 * time.Millisecond

// fileLock is a held cross-process advisory lock.
type fileLock struct{ f *os.File }

// acquireLock takes an exclusive flock on path, polling until timeout.
func acquireLock(ctx context.Context, path string, timeout time.Duration) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("httpbridge: opening %s: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := flockExclusiveNB(f)
		if err == nil {
			return &fileLock{f: f}, nil
		}
		if !isWouldBlock(err) {
			_ = f.Close()
			return nil, fmt.Errorf("httpbridge: locking %s: %w", path, err)
		}
		if ctx.Err() != nil {
			_ = f.Close()
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("httpbridge: timed out waiting for %s after %s", path, timeout)
		}
		time.Sleep(lockPollInterval)
	}
}

// release drops the lock. The close alone would release it; the explicit
// unlock keeps the intent readable.
func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = flockUnlock(l.f)
	_ = l.f.Close()
	l.f = nil
}

// atomicWrite persists data to path through the full hardening ladder:
// same-directory temp file, chmod 0600, write, fsync, rename, fsync of the
// parent. A reader never observes a partially written token file.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("httpbridge: writing %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("httpbridge: writing %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("httpbridge: writing %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("httpbridge: writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("httpbridge: writing %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("httpbridge: committing %s: %w", path, err)
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so the preceding rename is durable.
// Filesystems that reject fsync on directories are tolerated: the rename is
// atomic there regardless.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("httpbridge: syncing %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return fmt.Errorf("httpbridge: syncing %s: %w", dir, err)
	}
	return nil
}
