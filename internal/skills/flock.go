//go:build darwin || linux || windows

package skills

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The cross-process lock, delegated to internal/platform: flock(2) on
// darwin/linux, LockFileEx on Windows, one implementation of each behind one
// name. See docs/status/windows.md for what the Windows half is worth (it
// cross-compiles and has never run).
//
// internal/platform is the only package this may reach for it — skills must
// not import internal/registry or any other store (doc.go) — and it is a
// zero-business-dependency foundation, which is why depending on it costs
// nothing the dependency directions care about.
//
// TryLockFile, not LockFile: acquireLock polls so it can honour the caller's
// context and its own timeout.
//
// Failure direction: a lock that cannot be taken fails the store operation
// rather than letting a second writer in.

func flockExclusiveNB(f *os.File) error { return platform.TryLockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }

func isWouldBlock(err error) bool { return platform.IsLockBusy(err) }
