//go:build windows

package api

import (
	"os"
	"os/exec"
)

// NOT VERIFIED ON REAL HARDWARE. See docs/status/windows.md.
//
// There is no lifeline here: os/exec cannot hand a child an extra descriptor
// on Windows, so a supervised daemon has only the owner-pid poll to notice
// that its application is gone (internal/daemon/owner.go). Returning nils is
// how that is said — the caller then omits --owner-lifeline-fd, and the
// daemon knows it has one mechanism rather than two, instead of waiting on a
// pipe that was never wired.
func newLifeline() (r, w *os.File, err error) { return nil, nil, nil }

// attachLifeline has nothing to attach and answers "no descriptor".
func attachLifeline(*exec.Cmd, *os.File) int { return 0 }

// terminate ends the daemon.
//
// Kill, not a graceful request, and the trade is deliberate: Windows has no
// SIGTERM to deliver, and the alternative to a hard stop is not a gentler
// stop but no stop at all — a hub left running with the application gone, no
// window, no tray, and nothing on the machine that will ever end it. An
// abandoned in-flight call is the smaller loss. When a Job Object lands here
// (docs/status/windows.md) this becomes a proper drain.
func terminate(p *os.Process) error { return p.Kill() }
