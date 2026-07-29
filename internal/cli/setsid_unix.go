//go:build unix

package cli

import "syscall"

// detachProcessGroup detaches the gateway from the spawning client's
// process group by starting a new session (setsid). This prevents
// downstream child processes from raising SIGTTIN/SIGTTOU against a TUI
// client's terminal (docs/flows.md). It fails with EPERM when the process
// is already a process-group leader (e.g. launched from an interactive
// shell) — callers must treat any failure as non-fatal.
func detachProcessGroup() error {
	_, err := syscall.Setsid()
	return err
}
