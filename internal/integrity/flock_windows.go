//go:build windows

package integrity

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The Windows half of the lock, delegated to internal/platform: LockFileEx
// takes forty lines and the rationale for WHERE the lock byte sits, and seven
// packages in this repository need the same thing, while the Unix side is a
// syscall.Flock one-liner each. See docs/windows.md.
//
// Failure direction as on Unix: a lock that cannot be taken fails the store operation instead of writing unserialized.

func flockExclusiveNB(f *os.File) error { return platform.TryLockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }

func isWouldBlock(err error) bool { return platform.IsLockBusy(err) }
