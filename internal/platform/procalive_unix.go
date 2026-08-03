//go:build unix

package platform

import (
	"errors"
	"syscall"
)

// ProcessAlive reports whether pid names a live process, and whether the
// question could be answered at all.
//
// The second return value is the whole point. "Is that process still there"
// has three answers, not two, and a caller that collapses "no" and "cannot
// tell" into one value has to pick a failure direction for both at once —
// which is how a probe that merely lacked permission comes to read as a
// process that exited. Callers decide what an unknown means for them: the
// daemon's owner watch treats it as alive (never shut down on a guess), while
// `daemon stop` treats it as not-ours-to-signal.
//
// Unix implementation: signal 0 is the documented existence probe. ESRCH is a
// definitive no; EPERM is a definitive yes about a process owned by somebody
// else; anything else — including a non-positive pid, which is a process
// group or "every process", not a process — is unknown.
func ProcessAlive(pid int) (alive, known bool) {
	if pid <= 0 {
		return false, false
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, syscall.ESRCH):
		return false, true
	case errors.Is(err, syscall.EPERM):
		return true, true
	default:
		return false, false
	}
}
