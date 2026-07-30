//go:build !windows

package platform

import (
	"fmt"
	"os"
	"runtime"
)

// Non-Windows stand-ins for the file-lock primitives, so the three names
// exist on every platform and the honest answer is a typed error rather than
// a missing symbol.
//
// Nothing reaches these in practice: the packages that lock files select
// syscall.Flock directly on darwin/linux (each package's own flock_unix.go)
// and reach into this package only from a flock_windows.go. They exist for
// the caller who one day writes cross-platform locking code and needs to be
// told, at the call site, that this platform has none.

// LockFile is unsupported outside Windows; use syscall.Flock.
func LockFile(*os.File) error {
	return fmt.Errorf("platform: file locking on %s: %w", runtime.GOOS, ErrUnsupportedPlatform)
}

// TryLockFile is unsupported outside Windows; use syscall.Flock with LOCK_NB.
func TryLockFile(*os.File) error {
	return fmt.Errorf("platform: file locking on %s: %w", runtime.GOOS, ErrUnsupportedPlatform)
}

// UnlockFile is unsupported outside Windows; use syscall.Flock with LOCK_UN.
func UnlockFile(*os.File) error {
	return fmt.Errorf("platform: file locking on %s: %w", runtime.GOOS, ErrUnsupportedPlatform)
}

// IsLockBusy reports false here: with no lock implementation there is no
// contention to report, and the errors above are permanent failures rather
// than "try again later".
func IsLockBusy(error) bool { return false }
