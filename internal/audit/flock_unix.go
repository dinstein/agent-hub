//go:build darwin || linux

package audit

import (
	"os"
	"syscall"
)

// flock via syscall keeps the package free of third-party dependencies
// (golang.org/x/sys is deliberately not used, matching internal/registry).
// flock(2) exists on both darwin and linux; Windows has its own
// implementation in flock_windows.go, delegating to internal/platform.

// flockExclusive takes a blocking exclusive lock, retrying on EINTR.
func flockExclusive(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
