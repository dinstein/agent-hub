//go:build unix

package cli

import "syscall"

// daemonSysProcAttr detaches a background-started daemon into its own
// session (setsid): it must survive the CLI's terminal and never receive
// the shell's job-control signals.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// daemonSignalStop requests a graceful stop (SIGTERM → daemon.Run drains).
func daemonSignalStop(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// daemonKillGroup force-kills the daemon's process group (--force). The
// daemon is a session leader (Setsid at spawn), so -pid addresses its whole
// group; a plain pid kill is the fallback for daemons started foreground.
func daemonKillGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}

// daemonAlive reports whether pid exists (signal 0 probe). Failure
// direction: any error (ESRCH, EPERM, ...) reads as "not ours to manage" —
// false, so stop/status never signal a pid they cannot verify.
func daemonAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}
