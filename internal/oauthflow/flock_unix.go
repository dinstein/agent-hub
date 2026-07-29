//go:build darwin || linux

package oauthflow

import (
	"errors"
	"os"
	"syscall"
)

// flock(2) via syscall so this package stays inside its dependency budget
// (stdlib + internal/secrets + internal/guard/netguard; golang.org/x/sys is
// deliberately not used). internal/registry and internal/integrity carry
// structurally identical copies — that duplication is intentional: those
// packages must not depend on each other, and three ten-line files are a
// smaller cost than a shared package that would violate the dependency
// directions in canonical.md §2.
//
// Windows arrives in M2 through flock_stub.go's build tag.

func flockExclusiveNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func isWouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
