//go:build windows

package platform

import (
	"errors"
	"syscall"
)

// NOT VERIFIED ON REAL HARDWARE, like the rest of this package's Windows
// branch. See docs/status/windows.md.
//
// Windows has no signal-0 probe, and the obvious substitutes are both wrong:
// os.FindProcess always succeeds here, and a pid that OpenProcess accepts may
// still name a process that has already exited — the handle keeps the pid
// alive as a zombie until every handle is closed. So existence is two calls:
// open, then ask for the exit code.
const (
	// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION,
	// the least a caller can ask for and still read an exit code. The wider
	// PROCESS_QUERY_INFORMATION is refused across integrity levels, which
	// would turn "a process I may not inspect" into "unknown" for the common
	// case of an elevated owner.
	processQueryLimitedInformation = 0x1000
	// stillActive is STILL_ACTIVE (259): the exit code Windows reports for a
	// process that has not exited. A process really exiting with 259 is
	// therefore indistinguishable from a live one — it reads as alive, which
	// is the direction that never kills a hub on a guess.
	stillActive = 259
	// errorInvalidParameter is what OpenProcess returns for a pid that does
	// not exist. It is the only definitive "no" available here.
	errorInvalidParameter = syscall.Errno(87)
)

// ProcessAlive reports whether pid names a live process, and whether the
// question could be answered at all. See the unix implementation for the
// contract; only the mechanism differs.
func ProcessAlive(pid int) (alive, known bool) {
	if pid <= 0 {
		return false, false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		if errors.Is(err, errorInvalidParameter) {
			return false, true
		}
		// Access denied and everything else: the process may well be there,
		// this call simply may not look at it.
		return false, false
	}
	defer func() { _ = syscall.CloseHandle(h) }()

	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false, false
	}
	return code == stillActive, true
}
