//go:build !darwin && !linux && !windows

package oauthflow

import (
	"errors"
	"os"
)

// Cross-process locking is implemented for darwin/linux only in M1; Windows
// (LockFileEx) is scheduled for M2. The stubs keep the call sites compiling
// so the port does not touch the refresh logic.
//
// Failure direction on an unsupported platform: acquiring the refresh lock
// fails, so the offline refresh path refuses to run rather than running
// unserialized. Two processes racing a one-time refresh token is worse than
// a refresh that reports "unsupported".

func flockExclusiveNB(_ *os.File) error { return errors.ErrUnsupported }

func flockUnlock(_ *os.File) error { return errors.ErrUnsupported }

func isWouldBlock(_ error) bool { return false }
