//go:build !darwin && !linux && !windows

package accesslog

import "os"

// Failure direction: bounded storage fails closed when this build cannot
// serialize the inspect-prune-write decision across gateway processes.
const crossProcessLockSupported = false

func flockExclusive(*os.File) error { return nil }
func flockUnlock(*os.File) error    { return nil }
