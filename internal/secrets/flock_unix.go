//go:build darwin || linux

package secrets

import (
	"errors"
	"os"
	"syscall"
)

// flock via syscall keeps this package's dependency surface where it is
// (standard library plus golang.org/x/crypto and the keyring), matching
// internal/ratelimit, internal/registry and internal/oauthflow. flock(2)
// exists on both darwin and linux; Windows has its own half in
// flock_windows.go, delegating to internal/platform.

// crossProcessLockSupported reports that this build holds a real exclusive
// lock across a whole vault read-modify-write cycle. See flock_stub.go for
// what the false case means.
const crossProcessLockSupported = true

// flockExclusiveNB attempts a non-blocking exclusive lock, retrying on
// EINTR. EWOULDBLOCK is reported to the caller, which polls.
func flockExclusiveNB(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func flockUnlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

func isWouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
