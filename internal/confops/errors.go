package confops

import (
	"errors"
	"fmt"
)

// Kind classifies a failure so a front end can map it onto its own status
// vocabulary without pattern-matching on messages: the CLI maps it to an
// exit code, the control plane to an HTTP status.
type Kind string

const (
	// KindUsage is a malformed or contradictory argument (CLI exit 2, HTTP 400).
	KindUsage Kind = "usage"
	// KindNotFound is a reference to something that does not exist (exit 3, 404).
	KindNotFound Kind = "not_found"
	// KindConflict is a name that is already taken (exit 1, 409).
	KindConflict Kind = "conflict"
	// KindDenied is a refusal by a guard, not by argument shape (exit 6, 403).
	KindDenied Kind = "denied"
	// KindState is a state file that could not be read and must NOT be
	// treated as empty (exit 7, 500).
	KindState Kind = "state"
	// KindStale is an optimistic-concurrency miss (exit 1, 409). See StaleError.
	KindStale Kind = "stale"
)

// Stable machine-readable codes. They are the SAME strings the CLI freezes
// in its error-copy golden file and the control plane returns in its error
// body; internal/cli asserts the two vocabularies agree.
const (
	CodeUsage                = "E_USAGE"
	CodeNotFound             = "E_NOT_FOUND"
	CodeServerNotFound       = "E_SERVER_NOT_FOUND"
	CodeServerExists         = "E_SERVER_EXISTS"
	CodeProfileNotFound      = "E_PROFILE_NOT_FOUND"
	CodeProfileExists        = "E_PROFILE_EXISTS"
	CodeToolNotFound         = "E_TOOL_NOT_FOUND"
	CodeConfigKeyUnknown     = "E_CONFIG_KEY_UNKNOWN"
	CodeUnsupportedTransport = "E_UNSUPPORTED_TRANSPORT"
	CodeDenied               = "E_GOVERNANCE_DENIED"
	CodeStateCorrupt         = "E_STATE_CORRUPT"
	CodeStalePrecondition    = "E_STALE_PRECONDITION"
)

// Error is the typed failure of a confops operation. Front ends render
// Message and Hint verbatim and translate Kind into their own status.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Hint    string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func usagef(format string, a ...any) *Error {
	return &Error{Kind: KindUsage, Code: CodeUsage, Message: fmt.Sprintf(format, a...)}
}

func notFoundf(code, format string, a ...any) *Error {
	return &Error{Kind: KindNotFound, Code: code, Message: fmt.Sprintf(format, a...)}
}

func conflictf(code, format string, a ...any) *Error {
	return &Error{Kind: KindConflict, Code: code, Message: fmt.Sprintf(format, a...)}
}

// ErrStalePrecondition is the sentinel every optimistic-concurrency miss
// matches with errors.Is; the concrete *StaleError carries the generations.
var ErrStalePrecondition = errors.New("registry generation moved since the caller last read it")

// StaleError reports that a Precondition did not hold. Want is what the
// caller believed the registry was at, Got is what it actually is — the
// front end echoes Got so the client can re-read and retry against a known
// version instead of guessing.
//
// Nothing was written when this is returned: the check runs inside the
// cross-process lock, before the mutation.
type StaleError struct {
	Want uint64
	Got  uint64
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("registry generation is %d, not the expected %d: it was modified elsewhere", e.Got, e.Want)
}

func (e *StaleError) Is(target error) bool { return target == ErrStalePrecondition }

// AsError extracts the typed *Error from err, if any. Front ends use it to
// map Kind/Code without importing the error-construction helpers.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// AsStale extracts the typed *StaleError from err, if any.
func AsStale(err error) (*StaleError, bool) {
	var e *StaleError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
