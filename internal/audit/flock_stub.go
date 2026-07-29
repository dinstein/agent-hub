//go:build !darwin && !linux

package audit

import "os"

// Stub for platforms without a flock implementation yet (Windows is M2).
//
// Failure direction: with no lock, cross-process dedup degrades to
// best-effort — duplicate security events may be emitted, but nothing is
// ever suppressed that should not be (fail-open, same direction as
// SecurityStream.shouldEmit).

func flockExclusive(*os.File) error { return nil }

func flockUnlock(*os.File) error { return nil }
