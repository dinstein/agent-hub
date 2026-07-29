//go:build darwin || linux

package ratelimit

import (
	"os"
	"syscall"
)

// flock via syscall keeps the package free of third-party dependencies
// (golang.org/x/sys is deliberately not used, matching internal/registry
// and internal/audit). flock(2) exists on both darwin and linux; Windows
// arrives with the rest of M2's platform work.

// crossProcessLockSupported reports that this build holds a real exclusive
// lock across the whole read-decide-write cycle, so counters from several
// gateway processes MERGE instead of overwriting each other. New refuses to
// enforce rules on a build where this is false (flock_stub.go).
const crossProcessLockSupported = true

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
