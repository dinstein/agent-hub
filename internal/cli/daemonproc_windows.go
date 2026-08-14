//go:build windows

package cli

import (
	"syscall"

	"github.com/dinstein/agent-hub/internal/platform"
)

// Windows daemon process control.
//
// NOT VERIFIED ON REAL HARDWARE, like the rest of this project's Windows
// branch (docs/status/windows.md). Every call below is a documented Win32 API used
// the way Go's own os/exec uses it, and the logic is unit-tested through
// injected seams, but nothing here has been observed running.

const (
	// createNewProcessGroup is CREATE_NEW_PROCESS_GROUP. A background daemon
	// must not die with the console that launched it: without this the
	// terminal's Ctrl-C reaches the daemon as well as the CLI, and closing
	// the window takes it down.
	createNewProcessGroup = 0x00000200
	// createNoWindow is CREATE_NO_WINDOW. The daemon has no console UI, and
	// without this every `daemon start` flashes an empty console window that
	// then sits in the taskbar for the life of the hub.
	createNoWindow = 0x08000000
)

// daemonSysProcAttr detaches a background-started daemon from the launching
// console. It is the Windows counterpart of setsid: same intent — survive
// the terminal, take no job-control signal from it — through the only
// mechanism this platform has.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | createNoWindow,
		HideWindow:    true,
	}
}

// daemonKillGroup force-kills the daemon (--force).
//
// TerminateProcess, not a console control event: a daemon spawned with
// CREATE_NO_WINDOW has no console to receive one, and GenerateConsoleCtrlEvent
// can only reach a group that shares the CALLER's console — which a daemon
// started from another terminal, or by the GUI, never does. The unix path
// signals the process GROUP because setsid made the daemon a session leader;
// there is no cheap equivalent here, and the daemon spawns no children of its
// own that outlive it, so the process is the group in practice.
//
// Failure direction: an error is returned rather than swallowed. "I could not
// kill it" must not read as "it is gone".
func daemonKillGroup(pid int) error {
	h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	// 1 rather than 0: an exit code of 0 would claim the daemon shut down
	// cleanly to anything that reads it afterwards.
	return syscall.TerminateProcess(h, 1)
}

// daemonAlive reports whether pid names a live process.
//
// It delegates to platform.ProcessAlive, which answers the three-way question
// Windows actually poses — yes, no, and "this call may not look" — and folds
// the third into false here for the reason the unix side folds EPERM into
// false: stop and status must never signal a pid they could not verify.
func daemonAlive(pid int) bool {
	alive, known := platform.ProcessAlive(pid)
	return alive && known
}

// daemonStopBySignal is false here: Windows has no signal to send, so the
// graceful stop is asked for over the control plane. See runDaemonStop.
const daemonStopBySignal = false

// daemonSignalStop is unreachable on Windows — runDaemonStop asks instead —
// and answers unsupported rather than pretending, in case a future caller
// reaches it without consulting daemonStopBySignal.
func daemonSignalStop(int) error { return platform.ErrUnsupportedPlatform }
