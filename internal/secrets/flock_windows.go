//go:build windows

package secrets

import (
	"os"

	"github.com/dinstein/agent-hub/internal/platform"
)

// The Windows half of the vault lock, delegated to internal/platform:
// LockFileEx takes forty lines and the rationale for WHERE the lock byte
// sits (a byte past any plausible file length, because Windows locks are
// mandatory rather than advisory — see docs/windows.md), while the Unix side
// is a one-line syscall each. This is the seam ruling A.5 #23 asks for:
// nothing outside internal/platform branches on the platform beyond picking
// one of these files.
//
// TryLockFile, not LockFile: acquireVaultLock polls so it can honour the
// caller's context, which a blocking lock syscall cannot.
//
// UNVERIFIED on a real machine, like everything else Windows here — the
// gates are cross-compilation only (docs/windows.md).

const crossProcessLockSupported = true

func flockExclusiveNB(f *os.File) error { return platform.TryLockFile(f) }

func flockUnlock(f *os.File) error { return platform.UnlockFile(f) }

func isWouldBlock(err error) bool { return platform.IsLockBusy(err) }
