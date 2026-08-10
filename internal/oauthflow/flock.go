//go:build darwin || linux || windows

package oauthflow

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The refresh lock, delegated to internal/platform: flock(2) on darwin/linux,
// LockFileEx on Windows, one implementation of each behind one name. See
// docs/windows.md for what the Windows half is worth (it cross-compiles and
// has never run).
//
// This package's dependency budget is the standard library plus
// internal/secrets, internal/guard/netguard and internal/platform. The last
// is a zero-business-dependency foundation (canonical.md §2 rule 4), so
// reaching it adds nothing the dependency directions care about — which is
// what the local copy this replaced could not say for itself: it retried no
// EINTR while internal/secrets' identical function did, and a signal
// delivered mid-syscall therefore reported the offline refresh as broken.
//
// TryLockFile, not LockFile: acquireRefreshLock polls so it can honour the
// caller's context and its own timeout.
//
// Failure direction: a refresh lock that cannot be taken means the offline
// refresh path refuses to run. Two processes racing a one-time refresh token
// is worse than a refresh that reports an error.

func flockExclusiveNB(f *os.File) error { return platform.TryLockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }

func isWouldBlock(err error) bool { return platform.IsLockBusy(err) }
