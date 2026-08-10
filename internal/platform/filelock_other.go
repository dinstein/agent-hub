//go:build !windows && !darwin && !linux

package platform

import (
	"fmt"
	"os"
	"runtime"
)

// Stand-ins for the file-lock primitives on the platforms that have neither
// implementation — flock(2) on darwin/linux (filelock_unix.go) and LockFileEx
// on Windows (filelock_windows.go) cover everything agenthub is built for.
// The four names exist here so the answer on such a build is a typed error
// rather than a missing symbol.
//
// Nothing agenthub can run on reaches these: dataDirNamed already returns
// ErrUnsupportedPlatform outside darwin/linux/windows, so a build landing
// here cannot resolve a data directory to lock a file in. They exist for the
// caller who is told, at the call site, that this platform has no locking.

// LockFile is unsupported on this platform.
func LockFile(*os.File) error {
	return fmt.Errorf("platform: file locking on %s: %w", runtime.GOOS, ErrUnsupportedPlatform)
}

// TryLockFile is unsupported on this platform.
func TryLockFile(*os.File) error {
	return fmt.Errorf("platform: file locking on %s: %w", runtime.GOOS, ErrUnsupportedPlatform)
}

// UnlockFile is unsupported on this platform.
func UnlockFile(*os.File) error {
	return fmt.Errorf("platform: file locking on %s: %w", runtime.GOOS, ErrUnsupportedPlatform)
}

// IsLockBusy reports false here: with no lock implementation there is no
// contention to report, and the errors above are permanent failures rather
// than "try again later".
func IsLockBusy(error) bool { return false }
