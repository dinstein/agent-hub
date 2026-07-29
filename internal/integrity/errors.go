package integrity

import (
	"errors"
	"fmt"
	"time"
)

// ErrStoreCorrupt is the sentinel matched by errors.Is when a state file
// exists but cannot be parsed (or carries an unsupported version).
//
// Fail direction: fail-closed. Callers must treat this as "state unknown —
// block the affected operations and alert loudly", never as an empty store.
// Distinguishing this from ErrNotFound is load-bearing: treating a
// transient decode error as "record missing" would let an auto-approve path
// overwrite a Pending record.
var ErrStoreCorrupt = errors.New("integrity: state store corrupt")

// CorruptError is the typed form of a corrupt state file. The file is left
// exactly where it was — never renamed, never truncated — so operators can
// inspect it and tamper evidence is preserved.
type CorruptError struct {
	Path string // the unreadable state file
	Err  error  // underlying parse/validation error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("integrity: %s is corrupt (fail-closed, file left in place): %v", e.Path, e.Err)
}

// Is makes errors.Is(err, ErrStoreCorrupt) succeed.
func (e *CorruptError) Is(target error) bool { return target == ErrStoreCorrupt }

func (e *CorruptError) Unwrap() error { return e.Err }

// ErrNotFound reports that a requested record does not exist in a healthy
// store. It is deliberately distinct from ErrStoreCorrupt (see above).
var ErrNotFound = errors.New("integrity: record not found")

// ErrLockTimeout is the sentinel matched by errors.Is when the
// cross-process lock could not be acquired within the configured timeout.
var ErrLockTimeout = errors.New("integrity: lock acquisition timed out")

// LockTimeoutError is the typed form of a lock-acquisition timeout.
type LockTimeoutError struct {
	Path    string        // lock file path
	Timeout time.Duration // configured timeout that elapsed
}

func (e *LockTimeoutError) Error() string {
	return fmt.Sprintf("integrity: timed out after %s waiting for lock %s", e.Timeout, e.Path)
}

// Is makes errors.Is(err, ErrLockTimeout) succeed.
func (e *LockTimeoutError) Is(target error) bool { return target == ErrLockTimeout }

// TransitionError reports a state-machine transition that the transition
// table forbids. The record is left untouched and the tool stays blocked
// (fail-closed); callers should log it loudly — a forbidden transition
// attempt is a bug or an attack, never routine.
type TransitionError struct {
	From   ToolState
	To     ToolState
	Reason TransitionReason
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("integrity: forbidden transition %s -> %s (reason %s); record unchanged, tool stays blocked",
		e.From, e.To, e.Reason)
}
