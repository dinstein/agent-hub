package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Optimistic concurrency for every control-plane WRITE.
//
// The registry has five writers (N gateways, the daemon, the CLI, the GUI
// and any third party embedding this package). A file lock keeps a write
// from tearing; it does NOT keep one writer from overwriting another's
// change. A long-lived window whose data is minutes old would silently
// discard edits made elsewhere, so every write carries the generation the
// caller last read:
//
//   - expectedGeneration == 0 means "do not check" — the non-interactive
//     path used by scripts and by the CLI, whose behaviour is unchanged.
//   - a non-zero value is compared under the registry lock, BEFORE the
//     mutation. A mismatch writes nothing and answers 409.
//
// Failure direction: a stale write is REFUSED, never merged and never
// applied last-writer-wins. The caller re-reads and decides.
//
// The daemon accepts the precondition in the body OR in the query string.
// This client always uses the QUERY, for one shape across every method:
// DELETE carries no body, so a body-only spelling could not guard it at all,
// and sending it twice would be two spellings that can disagree.
const generationParam = "expected_generation"

// preconditionQuery returns q with the precondition appended. q may be nil.
// A zero generation adds nothing, so "do not check" is spelled by absence
// and can never be confused with "expect generation 0".
func preconditionQuery(q url.Values, expectedGeneration uint64) url.Values {
	if expectedGeneration == 0 {
		return q
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set(generationParam, strconv.FormatUint(expectedGeneration, 10))
	return q
}

// WriteResult is what every mutating call returns: where the registry now
// stands, whether anything actually changed, and any non-fatal report the
// operator must still hear about.
//
// CONTRACT: mirrors internal/confops.Result (api cannot import internal/*).
// Generation is the value to feed back as the next expectedGeneration, so a
// caller performing a burst of edits never has to re-read between them.
type WriteResult struct {
	Generation uint64 `json:"generation"`
	// Changed is false when the write was a no-op (the value was already
	// what the caller asked for). It is not a failure.
	Changed bool `json:"changed"`
	// Warnings are healed-corruption reports and fail-closed side effects
	// (e.g. "this client now resolves to an EMPTY scope"). They accompany a
	// SUCCESSFUL write and must be surfaced, not swallowed.
	Warnings []string `json:"warnings,omitempty"`
}

// ConflictError reports that a write lost the optimistic-concurrency check:
// the registry moved between the caller's read and its write, and NOTHING
// was written.
//
// CurrentGeneration is what the registry actually stands at. The intended
// caller behaviour is re-read → re-apply the user's intent → retry with the
// new generation, never a blind retry with the same body.
type ConflictError struct {
	// Err is the daemon's error body, passed through verbatim.
	Err *Error
	// CurrentGeneration is the generation the registry is at now. It is 0
	// when the daemon did not report one; a caller must then re-read to
	// find out rather than assuming 0 (which would mean "do not check").
	CurrentGeneration uint64
}

// Error implements the error interface.
func (e *ConflictError) Error() string {
	if e.CurrentGeneration == 0 {
		return e.Err.Error() + " (registry generation moved; re-read and retry)"
	}
	return fmt.Sprintf("%s (registry is now at generation %d; re-read and retry)",
		e.Err.Error(), e.CurrentGeneration)
}

// Unwrap exposes the underlying *Error so IsCode and errors.As keep working
// on a conflict.
func (e *ConflictError) Unwrap() error { return e.Err }

// IsConflict reports whether err is an optimistic-concurrency refusal, i.e.
// whether "re-read and try again" is the right response.
func IsConflict(err error) bool {
	var c *ConflictError
	return errors.As(err, &c)
}

// AsConflict extracts the typed *ConflictError, giving access to
// CurrentGeneration.
func AsConflict(err error) (*ConflictError, bool) {
	var c *ConflictError
	if errors.As(err, &c) {
		return c, true
	}
	return nil, false
}

// asConflict promotes a stale-precondition refusal to *ConflictError.
//
// The test is deliberately narrow: 409 alone is NOT enough, and neither is
// "some conflict code". The daemon also answers 409 for a name already taken
// (E_SERVER_EXISTS, E_PROFILE_EXISTS, E_TOKEN_EXISTS) and for a skills target
// that drifted (E_CONFLICT). Re-reading and retrying fixes none of those, and telling a
// frontend "your view was stale" for a duplicate name would send it into a
// retry loop that can never succeed.
func asConflict(err error) error {
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict {
		return err
	}
	if e.Code != ErrCodeStalePrecondition {
		return err
	}
	return &ConflictError{Err: e, CurrentGeneration: e.Generation}
}

// doWrite performs one mutating call carrying the precondition, mapping a
// stale-precondition refusal onto *ConflictError.
func (c *Client) doWrite(
	ctx context.Context, method, path string, q url.Values,
	expectedGeneration uint64, in, out any,
) error {
	return asConflict(c.do(ctx, method, path, preconditionQuery(q, expectedGeneration), in, out))
}
