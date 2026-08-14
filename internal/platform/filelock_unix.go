//go:build darwin || linux

package platform

import (
	"errors"
	"os"
	"syscall"
)

// Cross-process file locking for Unix, the other half of the seam whose
// Windows side is filelock_windows.go. Every package that keeps a
// single-writer file (internal/calllog, httpbridge, oauthflow, ratelimit,
// registry, secrets, skills) calls in here through a four-line wrapper, so
// there is one implementation of each primitive rather than one per package.
//
// It used to be one per package, on the argument that flock(2) is a one-liner
// and seven copies of a one-liner cost less than a shared home. The copies
// then drifted: internal/secrets retried EINTR around the non-blocking lock
// and internal/oauthflow did not, so the same signal that cost secrets one
// retry made oauthflow's offline refresh report a hard failure. One EINTR
// policy, stated once, is the point of this file.
//
// syscall rather than golang.org/x/sys/unix: internal/platform is a
// zero-dependency foundation (docs/conventions.md#dependency-directions rule 4, depguard-enforced to
// $gostd only), the same reason filelock_windows.go dials kernel32 by hand.

// LockFile takes a blocking exclusive lock on f, for the callers whose
// contract is "wait your turn" (internal/calllog, internal/ratelimit).
//
// Failure direction: fail closed. Every error is returned, and callers must
// abandon the write rather than proceed unserialized — a lock that reports it
// could not be taken is recoverable, a write that silently raced is not.
func LockFile(f *os.File) error { return flock(f, syscall.LOCK_EX) }

// TryLockFile takes an exclusive lock on f without blocking. When another
// process holds it the error satisfies IsLockBusy, which is how callers tell
// "someone else has it" from "locking is broken here".
func TryLockFile(f *os.File) error { return flock(f, syscall.LOCK_EX|syscall.LOCK_NB) }

// UnlockFile releases the lock LockFile or TryLockFile took.
//
// Failure direction: an error here is reported, never swallowed. Closing the
// descriptor releases the lock regardless, so a leaked lock cannot outlive
// the open file description — but it can outlive the operation, and a caller
// that kept the file open while believing it had unlocked would deadlock
// against itself.
func UnlockFile(f *os.File) error { return flock(f, syscall.LOCK_UN) }

// flock applies op to f's open file description, retrying on EINTR.
//
// EINTR is the whole reason this is a function. A signal delivered while the
// blocking form waits — SIGCHLD from any subprocess the gateway reaps, or the
// Go runtime's own preemption signal — interrupts the syscall, and flock(2)
// does not restart it. Returning that to the caller reports "the lock is
// broken" for something that means "try the call again", which is why the
// retry lives below every entry point rather than in some of them.
//
// LOCK_UN and the LOCK_NB form do not block and so should never see EINTR;
// looping around them costs nothing and keeps one rule for all three.
func flock(f *os.File, op int) error {
	for {
		err := syscall.Flock(int(f.Fd()), op)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// IsLockBusy reports whether err means "another process holds the lock",
// which is the answer TryLockFile's callers act on rather than fail on.
//
// Failure direction: an error this does not recognise is NOT busy. A caller
// then treats it as a real failure and refuses to touch the file, which is
// the safe direction — reading an unknown error as "merely contended" would
// turn a broken lock into a retry loop that eventually gives up and writes
// unlocked.
//
// Both spellings are tested although darwin and linux define EAGAIN and
// EWOULDBLOCK to the same errno: POSIX permits them to differ, and the cost
// of the second comparison is nothing against a lock conflict read as a crash.
func IsLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
