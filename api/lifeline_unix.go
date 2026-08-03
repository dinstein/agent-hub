//go:build unix

package api

import (
	"os"
	"os/exec"
	"syscall"
)

// newLifeline returns the read end to hand the daemon and the write end this
// process holds for its lifetime. The kernel closes the write end however
// this process dies, which is the whole mechanism.
func newLifeline() (r, w *os.File, err error) {
	return osPipe()
}

// osPipe exists so the one call is named at the point it is explained.
func osPipe() (*os.File, *os.File, error) { return os.Pipe() }

// attachLifeline gives r to the child and answers which descriptor it will
// arrive on. ExtraFiles[0] is fd 3 by definition — 0, 1 and 2 are the
// standard streams — and the number is passed to the child on its command
// line rather than left as a convention both sides have to remember.
func attachLifeline(cmd *exec.Cmd, r *os.File) int {
	cmd.ExtraFiles = append(cmd.ExtraFiles, r)
	return 2 + len(cmd.ExtraFiles)
}

// terminate asks the daemon to stop and drain (SIGTERM, its graceful path).
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
