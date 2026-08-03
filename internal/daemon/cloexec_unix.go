//go:build unix

package daemon

import (
	"os"
	"syscall"
)

// markCloseOnExec keeps an inherited descriptor from being inherited again.
// See LifelineFromFD for why it matters; a failure is not worth refusing the
// start over, since the fallback is the pid poll noticing a few seconds later.
func markCloseOnExec(f *os.File) {
	syscall.CloseOnExec(int(f.Fd()))
}
