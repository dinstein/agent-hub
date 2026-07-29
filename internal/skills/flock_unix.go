//go:build darwin || linux

package skills

import (
	"errors"
	"os"
	"syscall"
)

// flock via syscall so the package stays inside its dependency budget
// (standard library + internal/platform). This mirrors registry's and
// integrity's implementations by design but is an independent copy — skills
// must not import either of them (see doc.go). flock(2) exists on darwin
// and linux; Windows arrives in M2 with its own file.

func flockExclusiveNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func isWouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
