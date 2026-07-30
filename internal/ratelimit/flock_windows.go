//go:build windows

package ratelimit

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The Windows half of the lock, delegated to internal/platform: LockFileEx
// takes forty lines and the rationale for WHERE the lock byte sits, and seven
// packages in this repository need the same thing, while the Unix side is a
// syscall.Flock one-liner each. See docs/windows.md.
//
// crossProcessLockSupported is what this file is really for. While it was
// false, New REFUSED to build a limiter that had rules at all (limiter.go),
// because without the lock the read-modify-write cycle degrades to
// last-writer-wins and the effective quota silently multiplies by the number
// of gateway processes — a configuration that claims a quota must be honoured
// or reported, never quietly degraded. Turning it on is a claim about
// LockFileEx holding, and that claim has never run on a Windows machine:
// docs/windows.md is where that is recorded.
const crossProcessLockSupported = true

// LockFile, not TryLockFile: the counter's contract is "wait your turn", the
// blocking syscall.Flock(LOCK_EX) flock_unix.go uses. Windows has no EINTR to
// retry around — the wait happens inside the kernel call.

func flockExclusive(f *os.File) error { return platform.LockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }
