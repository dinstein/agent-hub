//go:build windows

package calllog

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

const crossProcessLockSupported = true

func flockExclusive(f *os.File) error { return platform.LockFile(f) }
func flockUnlock(f *os.File) error    { return platform.UnlockFile(f) }
