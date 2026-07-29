//go:build darwin || linux

package registry

import (
	"errors"
	"os"
	"syscall"
)

// flock via syscall so the package stays standard-library-only (M0 pulls in
// no third-party dependencies; golang.org/x/sys is deliberately not used).
// flock(2) exists on both darwin and linux; Windows support arrives in M2
// with a separate implementation.

func flockExclusiveNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func isWouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
