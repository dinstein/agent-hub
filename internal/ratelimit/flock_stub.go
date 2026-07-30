//go:build !darwin && !linux && !windows

package ratelimit

import "os"

// Stub for platforms with no flock implementation. Darwin, linux and
// windows all have one; the build tag above names exactly who lands here.
//
// Failure direction: FAIL CLOSED AT CONFIGURATION TIME. Without a
// cross-process lock the read-modify-write cycle degrades to
// last-writer-wins — exactly the counter mutual-overwrite the locked path
// exists to fix — so the effective quota silently multiplies by the number
// of gateway processes. New therefore REFUSES to build a limiter that has
// rules on such a build instead of enforcing a number nobody can trust:
// a configuration that claims a quota must be honoured or reported, never
// silently degraded (the same rule that makes `runtime: docker` refuse to
// fall back to host execution).
//
// A build with no rules configured is unaffected: the limiter is a no-op
// that never opens the counter file.
const crossProcessLockSupported = false

func flockExclusive(*os.File) error { return nil }

func flockUnlock(*os.File) error { return nil }
