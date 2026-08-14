//go:build darwin || linux

package platform_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// TestFileLockExcludesASecondHolder is the half of the seam that can actually
// run on the machines that run the tests: flock(2) is real here, while the
// Windows branch of this API has never executed anywhere (docs/status/windows.md).
//
// Two separate opens of one path, because flock is per open file description
// — two descriptors dup'd from one open would BOTH hold the same lock and the
// test would pass while proving nothing.
func TestFileLockExcludesASecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockme")

	first := openForLock(t, path)
	second := openForLock(t, path)

	if err := platform.TryLockFile(first); err != nil {
		t.Fatalf("TryLockFile on an unheld lock: %v", err)
	}

	err := platform.TryLockFile(second)
	if err == nil {
		t.Fatal("TryLockFile succeeded while another descriptor held the lock; " +
			"a lock that admits two writers is indistinguishable from a working one until a file is corrupted")
	}
	if !platform.IsLockBusy(err) {
		t.Fatalf("IsLockBusy(%v) = false, want true: contention read as a hard failure makes a "+
			"caller that polls give up instead of waiting its turn", err)
	}

	if err := platform.UnlockFile(first); err != nil {
		t.Fatalf("UnlockFile: %v", err)
	}
	if err := platform.TryLockFile(second); err != nil {
		t.Fatalf("TryLockFile after the holder released: %v", err)
	}
	if err := platform.UnlockFile(second); err != nil {
		t.Fatalf("UnlockFile on the second holder: %v", err)
	}
}

// TestBlockingLockWaitsForTheHolder pins the difference between LockFile and
// TryLockFile, which is the whole reason both exist: internal/calllog and
// internal/ratelimit have no deadline to honour and want the kernel to queue
// them, while the packages that carry a context poll a non-blocking attempt.
func TestBlockingLockWaitsForTheHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockme")

	holder := openForLock(t, path)
	waiter := openForLock(t, path)

	if err := platform.TryLockFile(holder); err != nil {
		t.Fatalf("TryLockFile: %v", err)
	}

	acquired := make(chan error, 1)
	go func() { acquired <- platform.LockFile(waiter) }()

	select {
	case err := <-acquired:
		t.Fatalf("LockFile returned %v while the lock was held; it must block", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := platform.UnlockFile(holder); err != nil {
		t.Fatalf("UnlockFile: %v", err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("LockFile after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LockFile never returned after the holder released")
	}
	if err := platform.UnlockFile(waiter); err != nil {
		t.Fatalf("UnlockFile on the waiter: %v", err)
	}
}

// TestIsLockBusyRejectsWhatItDoesNotKnow keeps the predicate's failure
// direction: an unrecognised error is NOT contention, so a caller refuses to
// write rather than retrying a broken lock until its deadline and then
// writing unlocked.
func TestIsLockBusyRejectsWhatItDoesNotKnow(t *testing.T) {
	if platform.IsLockBusy(nil) {
		t.Error("IsLockBusy(nil) = true")
	}
	if platform.IsLockBusy(os.ErrPermission) {
		t.Error("IsLockBusy(os.ErrPermission) = true; a permission failure is not contention")
	}
}

func openForLock(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
