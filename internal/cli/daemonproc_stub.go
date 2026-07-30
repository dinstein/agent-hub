//go:build !unix

package cli

import (
	"syscall"

	"github.com/dinstein/agent-hub/internal/platform"
)

// Non-unix stubs. The Windows control plane is a named pipe and its file
// locks are LockFileEx, but daemon PROCESS control — detach, graceful stop,
// force-kill the group — has no Windows implementation: it needs Job
// Objects, and there is no machine to verify one on. `agenthub daemon stop`
// therefore reports unsupported there; docs/windows.md tracks it.
//
// Failure direction: every operation reports unsupported rather than
// pretending to have signaled anything. daemonAlive answers false for the
// same reason — claiming a daemon is running is the costlier wrong answer.

func daemonSysProcAttr() *syscall.SysProcAttr { return nil }

func daemonSignalStop(int) error { return platform.ErrUnsupportedPlatform }

func daemonKillGroup(int) error { return platform.ErrUnsupportedPlatform }

func daemonAlive(int) bool { return false }
