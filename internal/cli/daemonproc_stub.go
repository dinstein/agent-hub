//go:build !unix

package cli

import (
	"syscall"

	"github.com/dinstein/agent-hub/internal/platform"
)

// Non-unix stubs (Windows lands in M2 with Job Objects / named pipes).
// Failure direction: every operation reports unsupported rather than
// pretending to have signaled anything.

func daemonSysProcAttr() *syscall.SysProcAttr { return nil }

func daemonSignalStop(int) error { return platform.ErrUnsupportedPlatform }

func daemonKillGroup(int) error { return platform.ErrUnsupportedPlatform }

func daemonAlive(int) bool { return false }
