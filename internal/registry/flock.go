//go:build darwin || linux || windows

package registry

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The cross-process lock, delegated to internal/platform: flock(2) on
// darwin/linux, LockFileEx on Windows, one implementation of each behind one
// name. See docs/windows.md for what the Windows half is worth (it
// cross-compiles and has never run).
//
// TryLockFile, not LockFile: acquireLock polls so it can honour the caller's
// context and its own timeout, which a blocking lock syscall cannot.
//
// Failure direction: a contended lock is reported as contention
// (isWouldBlock) and a broken one fails Open/Update/Reload. Neither runs
// unlocked.

func flockExclusiveNB(f *os.File) error { return platform.TryLockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }

func isWouldBlock(err error) bool { return platform.IsLockBusy(err) }
