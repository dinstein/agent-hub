//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// NOT VERIFIED ON REAL HARDWARE. Like the rest of this package's Windows
// branch, everything here cross-compiles and has never executed. See
// docs/status/windows.md.
//
// Cross-process file locking for Windows, used by every package that keeps a
// single-writer file (internal/registry, skills, calllog, ratelimit,
// httpbridge, oauthflow, secrets). Each of those owns a four-line wrapper
// that calls in here; filelock_unix.go answers the same four names with
// flock(2), so a caller writes one call and the build picks the syscall.
//
// syscall.NewLazyDLL rather than golang.org/x/sys/windows, which has both
// calls ready made: internal/platform is a zero-dependency foundation
// (docs/conventions.md#dependency-directions rule 4, depguard-enforced to $gostd only), the same reason
// packageid_windows.go dials kernel32 by hand.

const (
	// lockfileFailImmediately is LOCKFILE_FAIL_IMMEDIATELY: return at once
	// instead of blocking when the range is already held.
	lockfileFailImmediately = 0x00000001
	// lockfileExclusiveLock is LOCKFILE_EXCLUSIVE_LOCK: a write lock. Without
	// it LockFileEx takes a SHARED lock, which would let two writers in and
	// look like it worked.
	lockfileExclusiveLock = 0x00000002

	// errorLockViolation is ERROR_LOCK_VIOLATION, what LOCKFILE_FAIL_IMMEDIATELY
	// reports when someone else holds the range.
	errorLockViolation = syscall.Errno(33)
	// errorSharingViolation is ERROR_SHARING_VIOLATION. Documented for
	// LockFile rather than LockFileEx, and accepted here as the same answer:
	// the cost of reading it as "busy" is one caller retrying, while reading
	// it as an unknown failure would surface a lock conflict as a crash.
	errorSharingViolation = syscall.Errno(32)
)

// lockByteOffset is where the one-byte lock range sits: far beyond any
// plausible file length, and deliberately NOT over file data.
//
// THIS IS THE WHOLE DESIGN OF THIS FILE. Windows file locks are MANDATORY,
// not advisory — a locked range is unreadable to every other handle, and a
// reader that never asked for a lock gets ERROR_LOCK_VIOLATION on a plain
// Read. Unix flock is advisory: the same reader succeeds. Locking the data
// range would therefore not port flock, it would change what an unlocked
// reader sees, and every read path in this repository that deliberately does
// not lock (`calls tail`, `doctor`, the GUI's file views) would start failing
// on Windows alone, while the writer looked correct.
//
// Locking a range past EOF is explicitly legal on Windows and is how SQLite
// and Go's own cmd/go lock files behave. The offset is part of the on-disk
// protocol between agenthub processes: two builds that disagree about it do
// not lock against each other, so it may not move.
const lockByteOffset = 1 << 62

var (
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	procLockFile = kernel32.NewProc("LockFileEx")
	procUnlock   = kernel32.NewProc("UnlockFileEx")
)

// LockFile takes a blocking exclusive lock on f, for the callers whose
// contract is "wait your turn" (internal/calllog, internal/ratelimit).
func LockFile(f *os.File) error { return lockFile(f, lockfileExclusiveLock) }

// TryLockFile takes an exclusive lock on f without blocking. When another
// process holds it the error satisfies IsLockBusy, which is how callers tell
// "someone else has it" from "locking is broken here".
func TryLockFile(f *os.File) error {
	return lockFile(f, lockfileExclusiveLock|lockfileFailImmediately)
}

func lockFile(f *os.File, flags uintptr) error {
	if err := procLockFile.Find(); err != nil {
		return fmt.Errorf("platform: LockFileEx unavailable: %w", err)
	}
	ol := overlappedAtLockByte()
	// One byte at lockByteOffset. Length is split low/high like the offset:
	// the range is (offset, offset+1].
	r1, _, err := procLockFile.Call(f.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r1 == 0 {
		return err
	}
	return nil
}

// UnlockFile releases the lock LockFile or TryLockFile took.
//
// Failure direction: an error here is reported, never swallowed. Windows also
// releases every lock when the handle closes, so a leaked lock cannot outlive
// the process — but it can outlive the operation, and a caller that kept the
// file open while believing it had unlocked would deadlock against itself.
func UnlockFile(f *os.File) error {
	if err := procUnlock.Find(); err != nil {
		return fmt.Errorf("platform: UnlockFileEx unavailable: %w", err)
	}
	ol := overlappedAtLockByte()
	r1, _, err := procUnlock.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r1 == 0 {
		return err
	}
	return nil
}

// IsLockBusy reports whether err means "another process holds the lock",
// which is the answer TryLockFile's callers act on rather than fail on.
//
// Failure direction: an error this does not recognise is NOT busy. A caller
// then treats it as a real failure and refuses to touch the file, which is
// the safe direction — reading an unknown error as "merely contended" would
// turn a broken lock into a retry loop that eventually gives up and writes
// unlocked.
func IsLockBusy(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == errorLockViolation || errno == errorSharingViolation
}

// overlappedAtLockByte returns the OVERLAPPED naming the lock byte. The
// offset is passed as two 32-bit halves; HEvent stays zero, which is what
// makes the call synchronous.
func overlappedAtLockByte() syscall.Overlapped {
	return syscall.Overlapped{
		Offset:     uint32(lockByteOffset & 0xffffffff),
		OffsetHigh: uint32(lockByteOffset >> 32),
	}
}
