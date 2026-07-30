//go:build !darwin && !linux && !windows

package registry

import (
	"errors"
	"os"
)

// Cross-process locking is implemented for darwin/linux only. On every
// other platform these stubs make Open/Update/Reload fail rather than run
// unlocked: the store's whole consistency story is "one writer at a time",
// and silently dropping that on a platform nobody has tested would corrupt
// configuration instead of refusing to touch it.
//
// Windows needs LockFileEx/UnlockFileEx via golang.org/x/sys/windows (no
// new module — it is already a dependency). Keeping the call sites intact
// means that port does not touch the store logic. See docs/windows.md.

func flockExclusiveNB(_ *os.File) error { return errors.ErrUnsupported }

func flockUnlock(_ *os.File) error { return errors.ErrUnsupported }

func isWouldBlock(_ error) bool { return false }
