//go:build !darwin && !linux && !windows

package audit

import "os"

// Stub for platforms with no flock implementation. Darwin, linux and
// windows all have one; the build tag above names exactly who lands here.
//
// Failure direction: with no lock, cross-process dedup degrades to
// best-effort — duplicate security events may be emitted, but nothing is
// ever suppressed that should not be (fail-open, same direction as
// SecurityStream.shouldEmit).

func flockExclusive(*os.File) error { return nil }

func flockUnlock(*os.File) error { return nil }
