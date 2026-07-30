//go:build windows

package audit

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The Windows half of the lock, delegated to internal/platform: LockFileEx
// takes forty lines and the rationale for WHERE the lock byte sits, and seven
// packages in this repository need the same thing, while the Unix side is a
// syscall.Flock one-liner each. See docs/windows.md.
//
// This one replaces a stub that returned nil — "no lock, carry on" — so the
// change here is not "Windows now locks" but "Windows now locks at all".
// Cross-process dedup was best-effort on this platform, in the fail-open
// direction (duplicate security events, never a suppressed one); it is now the
// same single-writer guarantee the other two platforms have.
//
// LockFile, not TryLockFile: this caller's contract is "wait your turn", the
// blocking syscall.Flock(LOCK_EX) that flock_unix.go retries around EINTR.
// Windows has no EINTR to retry — the wait is inside the kernel call.

func flockExclusive(f *os.File) error { return platform.LockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }
