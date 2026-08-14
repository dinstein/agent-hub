//go:build darwin || linux || windows

package calllog

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The cross-process lock, delegated to internal/platform: flock(2) on
// darwin/linux, LockFileEx on Windows, one implementation of each behind one
// name. See docs/status/windows.md for what the Windows half is worth (it
// cross-compiles and has never run).
//
// LockFile, not TryLockFile: the ledger's contract is "wait your turn". A
// writer here holds the lock for one append or one prune pass and has no
// deadline to honour, so the kernel queues it rather than this package
// polling.
//
// Failure direction: a lock that cannot be taken fails the write. See
// flock_stub.go for what the platforms with no lock at all do instead.

const crossProcessLockSupported = true

func flockExclusive(f *os.File) error { return platform.LockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }
