//go:build !darwin && !linux && !windows

package skills

import (
	"errors"
	"os"
)

// Cross-process locking is implemented for darwin, linux and windows in
// flock.go, all through internal/platform; the build tag above names exactly
// who lands here instead.
//
// Failure direction: a store operation on such a build fails rather than
// running unlocked. Three state files are written as one unit under one
// lock, and a second writer would split that unit.

func flockExclusiveNB(_ *os.File) error { return errors.ErrUnsupported }

func flockUnlock(_ *os.File) error { return errors.ErrUnsupported }

func isWouldBlock(_ error) bool { return false }
