//go:build !unix && !windows

package api

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// A platform with neither a pipe this package knows how to pass nor a signal
// it knows how to send. Everything else in api already reports
// ErrUnsupportedPlatform there rather than guessing; supervision says the
// same, at the call that would otherwise silently do nothing.

func newLifeline() (r, w *os.File, err error) { return nil, nil, nil }

func attachLifeline(*exec.Cmd, *os.File) int { return 0 }

func terminate(*os.Process) error {
	return fmt.Errorf("api: stopping a hub on %s: %w", runtime.GOOS, ErrUnsupportedPlatform)
}
