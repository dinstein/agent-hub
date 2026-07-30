//go:build windows

package registry

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The Windows half of the lock, delegated to internal/platform: LockFileEx
// takes forty lines and the rationale for WHERE the lock byte sits, and seven
// packages in this repository need the same thing, while the Unix side is a
// syscall.Flock one-liner each. See docs/windows.md.
//
// Failure direction is now flock_unix.go's, not flock_stub.go's: a contended lock is reported as contention (isWouldBlock) and a broken one fails Open/Update/Reload. Neither runs unlocked.

func flockExclusiveNB(f *os.File) error { return platform.TryLockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }

func isWouldBlock(err error) bool { return platform.IsLockBusy(err) }
