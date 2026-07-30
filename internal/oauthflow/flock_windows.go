//go:build windows

package oauthflow

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The Windows half of the lock, delegated to internal/platform: LockFileEx
// takes forty lines and the rationale for WHERE the lock byte sits, and seven
// packages in this repository need the same thing, while the Unix side is a
// syscall.Flock one-liner each. See docs/windows.md.
//
// Failure direction as on Unix: the refresh lock failing means the offline refresh path refuses to run. Two processes racing a one-time refresh token is worse than a refresh that reports an error.

func flockExclusiveNB(f *os.File) error { return platform.TryLockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }

func isWouldBlock(err error) bool { return platform.IsLockBusy(err) }
