//go:build darwin || linux || windows

package secrets

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The vault lock, delegated to internal/platform: flock(2) on darwin/linux,
// LockFileEx on Windows, one implementation of each behind one name. This is
// the seam ruling A.5 #23 asks for — nothing outside internal/platform
// branches on the platform beyond selecting this file over flock_stub.go.
// See docs/windows.md for what the Windows half is worth (it cross-compiles
// and has never run).
//
// TryLockFile, not LockFile: acquireVaultLock polls so it can honour the
// caller's context, which a blocking lock syscall cannot.

// crossProcessLockSupported reports that this build holds a real exclusive
// lock across a whole vault read-modify-write cycle. See flock_stub.go for
// what the false case means.
const crossProcessLockSupported = true

func flockExclusiveNB(f *os.File) error { return platform.TryLockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }

func isWouldBlock(err error) bool { return platform.IsLockBusy(err) }
