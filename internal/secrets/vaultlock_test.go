package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVaultLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireVaultLock(t.Context(), dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// A second acquisition on its own descriptor must not win. flock is
	// per-open-file-description and LockFileEx is per-handle, so this holds
	// within one process on both.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := acquireVaultLock(ctx, dir); err == nil {
		t.Fatal("second acquire won while the first was held")
	}
	first.release()
	second, err := acquireVaultLock(t.Context(), dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	second.release()
}

func TestVaultLockHonoursContext(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireVaultLock(t.Context(), dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.release()

	// The timeout is vaultLockTimeout; a cancelled context must cut the wait
	// far shorter than that, which is the whole reason the lock polls rather
	// than blocking in the syscall.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if _, err := acquireVaultLock(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire with a cancelled context: err = %v, want context.Canceled", err)
	}
	if waited := time.Since(start); waited > vaultLockTimeout/2 {
		t.Fatalf("waited %s for a cancelled context; the wait is not ctx-aware", waited)
	}
}

// The lock file is created inside the secrets directory and, being a lock
// rather than a store, never gains content. Asserting the name pins what a
// second agenthub process looks for: two versions disagreeing on it would
// each take a lock nobody else respects.
func TestVaultLockFileIsDedicatedAndEmpty(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireVaultLock(t.Context(), dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lock.release()

	path := VaultLockPath(dir)
	if got, want := filepath.Base(path), "vault.lock"; got != want {
		t.Fatalf("lock file name = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("lock file holds %d bytes; it must carry the lock and nothing else", info.Size())
	}
	// It must not be the data file: those are replaced by rename, and a lock
	// on a replaced inode guards nothing.
	for _, name := range []string{encFileName, devKeyFileName, keyRegistryFileName} {
		if filepath.Base(path) == name {
			t.Fatalf("the lock file is %s, a file that gets replaced by rename", name)
		}
	}
}

func TestVaultLockReleaseIsSafeOnNil(t *testing.T) {
	var l *vaultLock
	l.release()
	(&vaultLock{}).release()
}
