//go:build !darwin && !linux && !windows

package skills

import (
	"errors"
	"os"
)

// Cross-process locking is implemented for darwin/linux only in M1; Windows
// (LockFileEx) is scheduled for M2. Compiling stubs preserve the call sites
// so the port does not touch the store logic.

func flockExclusiveNB(_ *os.File) error { return errors.ErrUnsupported }

func flockUnlock(_ *os.File) error { return errors.ErrUnsupported }

func isWouldBlock(_ error) bool { return false }
