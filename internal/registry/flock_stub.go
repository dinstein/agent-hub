//go:build !darwin && !linux && !windows

package registry

import (
	"errors"
	"os"
)

// Cross-process locking is implemented for darwin, linux and windows in
// flock.go, all through internal/platform. On every other platform these
// stubs make Open/Update/Reload fail rather than run unlocked: the store's
// whole consistency story is "one writer at a time", and silently dropping
// that on a platform nobody has tested would corrupt configuration instead
// of refusing to touch it.

func flockExclusiveNB(_ *os.File) error { return errors.ErrUnsupported }

func flockUnlock(_ *os.File) error { return errors.ErrUnsupported }

func isWouldBlock(_ error) bool { return false }
