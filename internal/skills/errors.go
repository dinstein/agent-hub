package skills

import (
	"errors"
	"fmt"
	"time"
)

// ErrStoreCorrupt is matched by errors.Is when a skills state file exists
// but cannot be parsed.
//
// Fail direction: fail-closed. A corrupt store is never treated as an empty
// one and is never renamed aside — a ".corrupt" rename would make the next
// read look like a legitimate fresh store, which is exactly what silent
// re-baselining gives an attacker (internal/integrity/doc.go states the same
// rule, for the same reason).
var ErrStoreCorrupt = errors.New("skills: state store corrupt")

// CorruptError is the typed form of a corrupt state file. The file stays
// exactly where it was so an operator can inspect it.
type CorruptError struct {
	Path string
	Err  error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("skills: %s is corrupt (fail-closed, file left in place): %v", e.Path, e.Err)
}

func (e *CorruptError) Is(target error) bool { return target == ErrStoreCorrupt }

func (e *CorruptError) Unwrap() error { return e.Err }

// ErrNotFound reports a missing record in a healthy store. Deliberately
// distinct from ErrStoreCorrupt: treating a decode failure as "not found"
// would let an install path recreate state a corrupt file was still
// holding.
var ErrNotFound = errors.New("skills: not found")

// ErrExists reports an ID collision that the caller must resolve.
var ErrExists = errors.New("skills: already exists")

// ErrConflict is matched by errors.Is when a write target is not ours: an
// owned directory without our marker, a shadowing file, an over-cap render,
// or damaged sentinels. The target is left untouched.
var ErrConflict = errors.New("skills: target conflict")

// ConflictError is the typed form of ErrConflict. Reason is a short,
// stable, machine-comparable cause; Path is what we refused to touch.
type ConflictError struct {
	Path   string
	Reason string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("skills: refusing to write %s: %s", e.Path, e.Reason)
}

func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// SentinelError reports a damaged sentinel pair in a shared file. It is a
// conflict (errors.Is(err, ErrConflict) holds) because the fail direction
// is identical: we cannot tell our bytes from the user's, so we refuse.
// Blind overwriting here is how a "managed block" tool eats a user's file.
type SentinelError struct {
	Path    string
	SkillID string
	Reason  string
}

func (e *SentinelError) Error() string {
	return fmt.Sprintf("skills: sentinel block for %q in %s is damaged (%s); refusing to write, fix or remove the markers by hand",
		e.SkillID, e.Path, e.Reason)
}

func (e *SentinelError) Is(target error) bool { return target == ErrConflict }

// ErrDrifted is matched by errors.Is when a materialized copy was edited
// outside agenthub and a write would destroy those edits.
//
// Fail direction: refuse. Drift is a user telling us something, even if
// what they are saying is "I edited the wrong file"; silently reverting it
// is how a sync tool teaches people to distrust its receipts. Callers pass
// InstallRequest.AllowDrift once a human has decided.
var ErrDrifted = errors.New("skills: installed copy has local modifications")

// DriftError is the typed form of ErrDrifted.
type DriftError struct {
	SkillID string
	Path    string
}

func (e *DriftError) Error() string {
	return fmt.Sprintf("skills: %s at %s was modified outside agenthub; refusing to overwrite (re-run with --force to discard the local changes)",
		e.SkillID, e.Path)
}

func (e *DriftError) Is(target error) bool { return target == ErrDrifted }

// ErrTampered is matched by errors.Is when the library copy no longer
// matches its pin. Fail-closed: install and update refuse to propagate a
// library copy we cannot vouch for.
var ErrTampered = errors.New("skills: library copy does not match its pin")

// TamperError is the typed form of ErrTampered.
type TamperError struct {
	SkillID string
	Pinned  string
	Current string
}

func (e *TamperError) Error() string {
	return fmt.Sprintf("skills: library copy of %q changed outside agenthub (pinned %s, now %s); refusing to install or update",
		e.SkillID, e.Pinned, e.Current)
}

func (e *TamperError) Is(target error) bool { return target == ErrTampered }

// ErrGitFetchUnsupported reports a git-source operation that would require
// talking to git. Recording a pin is supported today; fetching is M2. The
// honest error is the point — reporting "up to date" without having looked
// would be a lie the user cannot detect.
var ErrGitFetchUnsupported = errors.New("skills: git fetch is not implemented (M2); re-run with an explicit local checkout path")

// ErrUnsupportedKind reports a target that does not accept a skill's kind.
var ErrUnsupportedKind = errors.New("skills: target does not support this skill kind")

// ErrLockTimeout is matched by errors.Is when the cross-process lock could
// not be acquired in time.
var ErrLockTimeout = errors.New("skills: lock acquisition timed out")

// LockTimeoutError is the typed form of a lock timeout.
type LockTimeoutError struct {
	Path    string
	Timeout time.Duration
}

func (e *LockTimeoutError) Error() string {
	return fmt.Sprintf("skills: timed out after %s waiting for lock %s", e.Timeout, e.Path)
}

func (e *LockTimeoutError) Is(target error) bool { return target == ErrLockTimeout }

// ImportError reports a source tree agenthub refuses to import. Every
// rejection is deliberate: symlinks, parent traversal, absolute paths,
// non-regular files and oversized trees are all ways an import turns into
// an arbitrary-write primitive.
type ImportError struct {
	Path   string
	Reason string
}

func (e *ImportError) Error() string {
	return fmt.Sprintf("skills: cannot import %s: %s", e.Path, e.Reason)
}
