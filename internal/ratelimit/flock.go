//go:build darwin || linux || windows

package ratelimit

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The cross-process lock, delegated to internal/platform: flock(2) on
// darwin/linux, LockFileEx on Windows, one implementation of each behind one
// name. See docs/windows.md for what the Windows half is worth (it
// cross-compiles and has never run).
//
// LockFile, not TryLockFile: the counter's contract is "wait your turn", and
// the wait happens inside the kernel call rather than in a poll loop here.

// crossProcessLockSupported reports that this build holds a real exclusive
// lock across the whole read-decide-write cycle, so counters from several
// gateway processes MERGE instead of overwriting each other. New refuses to
// enforce rules on a build where this is false (flock_stub.go).
const crossProcessLockSupported = true

func flockExclusive(f *os.File) error { return platform.LockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }
