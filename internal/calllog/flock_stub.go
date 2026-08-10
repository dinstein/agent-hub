//go:build !darwin && !linux && !windows

package calllog

import "os"

// Stub for the platforms with no lock at all: darwin, linux and windows are
// served by flock.go through internal/platform, and the build tag above
// names exactly who lands here instead.
//
// Failure direction: bounded storage fails closed when this build cannot
// serialize the inspect-prune-write decision across gateway processes.
const crossProcessLockSupported = false

func flockExclusive(*os.File) error { return nil }
func flockUnlock(*os.File) error    { return nil }
