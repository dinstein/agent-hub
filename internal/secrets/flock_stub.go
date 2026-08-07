//go:build !darwin && !linux && !windows

package secrets

import (
	"errors"
	"os"
)

// Stub for platforms with no flock implementation. Darwin, linux and windows
// all have one; the build tag above names exactly who lands here.
//
// Failure direction: FAIL CLOSED. Without a cross-process lock every vault
// write degrades to last-writer-wins, which is the credential loss the lock
// exists to stop — so acquireVaultLock refuses instead, and Set/Delete
// report that they could not run. A write that says it did not happen is
// recoverable; a credential that silently vanished is not.
//
// Nothing agenthub can run on reaches this file. platform.dataDirNamed
// returns ErrUnsupportedPlatform outside darwin/linux/windows, so a build
// landing here cannot resolve a data directory in the first place — the stub
// exists to keep the call sites compiling, and refusing costs no platform
// that works.
const crossProcessLockSupported = false

func flockExclusiveNB(*os.File) error { return errors.ErrUnsupported }

func flockUnlock(*os.File) error { return errors.ErrUnsupported }

func isWouldBlock(error) bool { return false }
