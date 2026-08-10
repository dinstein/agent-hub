//go:build !darwin && !linux && !windows

package httpbridge

import (
	"errors"
	"os"
)

// Cross-process locking is implemented for darwin, linux and windows in
// flock.go, all through internal/platform; the build tag above names exactly
// who lands here instead. On such a platform a transaction fails loudly
// rather than writing unserialized.

func flockExclusiveNB(_ *os.File) error { return errors.ErrUnsupported }

func flockUnlock(_ *os.File) error { return errors.ErrUnsupported }

func isWouldBlock(_ error) bool { return false }
