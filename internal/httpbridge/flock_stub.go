//go:build !darwin && !linux

package httpbridge

import (
	"errors"
	"os"
)

// Cross-process locking is darwin/linux only until the Windows port (M2).
// The stubs keep the store's call sites compiling; on an unsupported
// platform a transaction fails loudly rather than writing unserialized.

func flockExclusiveNB(_ *os.File) error { return errors.ErrUnsupported }

func flockUnlock(_ *os.File) error { return errors.ErrUnsupported }

func isWouldBlock(_ error) bool { return false }
