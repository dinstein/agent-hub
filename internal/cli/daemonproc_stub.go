//go:build !unix && !windows

package cli

import (
	"syscall"

	"github.com/dinstein/agent-hub/internal/platform"
)

// Stubs for the platforms with neither a unix nor a Windows implementation.
// Windows has its own file now (daemonproc_windows.go); what is left here is
// js/wasm and anything else Go grows, where there is no process model to
// speak of.
//
// Failure direction: every operation reports unsupported rather than
// pretending to have signaled anything. daemonAlive answers false for the
// same reason — claiming a daemon is running is the costlier wrong answer.

func daemonSysProcAttr() *syscall.SysProcAttr { return nil }

func daemonSignalStop(int) error { return platform.ErrUnsupportedPlatform }

func daemonKillGroup(int) error { return platform.ErrUnsupportedPlatform }

func daemonAlive(int) bool { return false }

// daemonStopBySignal: nothing here can stop a daemon at all.
const daemonStopBySignal = false
