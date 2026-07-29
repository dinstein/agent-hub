//go:build darwin || linux

package integrity

import (
	"errors"
	"os"
	"syscall"
)

// flock via syscall so the package stays within its dependency budget
// (standard library + internal/platform; golang.org/x/sys deliberately not
// used). This mirrors internal/registry's implementation by design but is an
// independent copy — integrity must not import registry. flock(2) exists on
// both darwin and linux; Windows arrives in M2 with a separate file.

func flockExclusiveNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func isWouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
