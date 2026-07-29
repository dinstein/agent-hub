//go:build darwin || linux

package httpbridge

import (
	"errors"
	"os"
	"syscall"
)

// flock(2) via syscall keeps this package on the standard library. Windows
// (LockFileEx) arrives with the M2 port; the stub below keeps the call sites
// compiling until then.

func flockExclusiveNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func isWouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
