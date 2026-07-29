package registry

import (
	"errors"
	"fmt"
	"time"
)

// ErrLockTimeout is the sentinel matched by errors.Is when the cross-process
// lock could not be acquired within the configured timeout. Callers in
// cmd/agenthub map it to exit code 7 (lock contention).
var ErrLockTimeout = errors.New("registry: lock acquisition timed out")

// LockTimeoutError is the typed form of a lock-acquisition timeout.
type LockTimeoutError struct {
	Path    string        // lock file path
	Timeout time.Duration // configured timeout that elapsed
}

func (e *LockTimeoutError) Error() string {
	return fmt.Sprintf("registry: timed out after %s waiting for lock %s", e.Timeout, e.Path)
}

// Is makes errors.Is(err, ErrLockTimeout) succeed.
func (e *LockTimeoutError) Is(target error) bool { return target == ErrLockTimeout }

// UnreadableError reports that a document file could not be parsed even after
// the read-retry window and has been quarantined (renamed, never destroyed).
// The store keeps working: the affected document is replaced by its default
// value and all other documents remain fully usable.
type UnreadableError struct {
	Kind           DocKind
	Path           string // original file path
	QuarantinePath string // <name>.json.unreadable-<timestamp>
	Err            error  // underlying parse error
}

func (e *UnreadableError) Error() string {
	return fmt.Sprintf("registry: %s is unreadable (quarantined to %s): %v",
		e.Path, e.QuarantinePath, e.Err)
}

func (e *UnreadableError) Unwrap() error { return e.Err }
