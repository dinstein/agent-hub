package api

import "errors"

// ErrCodeBadResponse is synthesized client-side when the daemon's response
// cannot be positively identified as a well-formed envelope. It never
// originates from the server.
const ErrCodeBadResponse = "E_BAD_RESPONSE"

// Daemon error codes a frontend is expected to branch on.
//
// CONTRACT: these mirror the constants in internal/ctlapi (which api must
// not import, canonical.md §2 rule 1). Codes are part of the wire contract:
// they are matched, not displayed — the human text is Message/Hint.
const (
	// ErrCodeNotFound: unknown path or unknown resource. A control-plane
	// endpoint that a newer frontend knows about but this daemon does not
	// serve yet also answers with it — render "unavailable", not "broken".
	ErrCodeNotFound = "E_NOT_FOUND"
	// ErrCodeBadRequest: malformed body or parameters.
	ErrCodeBadRequest = "E_BAD_REQUEST"
	// ErrCodeAPIVersion: the daemon refuses this client's APIVersion.
	ErrCodeAPIVersion = "E_API_VERSION"
	// ErrCodeConflict: a concurrent change lost a compare-and-swap.
	ErrCodeConflict = "E_CONFLICT"
	// ErrCodeStalePrecondition: a write carried an expectedGeneration that
	// no longer matches the registry. NOTHING was written. The daemon
	// reports the current generation in ErrorBody.Generation; the client
	// surfaces both as *ConflictError (see IsConflict).
	ErrCodeStalePrecondition = "E_STALE_PRECONDITION"
	// ErrCodeAlreadyDecided: another frontend decided first. Idempotent by
	// design, not a failure (docs/modules/controlplane.md).
	ErrCodeAlreadyDecided = "E_ALREADY_DECIDED"
	// ErrCodeExpired: the approval deadline passed; the request auto-denied.
	ErrCodeExpired = "E_EXPIRED"
	// ErrCodeStale: the arguments changed since the request was raised.
	ErrCodeStale = "E_STALE"
	// ErrCodeInternal: the daemon failed to serve the request.
	ErrCodeInternal = "E_INTERNAL"
	// ErrCodeAuthRequired: a live downstream self-test was rejected with
	// HTTP 401/403. A frontend may offer Auth.StartLogin for that server.
	ErrCodeAuthRequired = "E_AUTH_REQUIRED"
)

// IsCode reports whether err is a control-plane *Error carrying code.
// Transport failures (daemon offline) match no code, so a caller that only
// tests codes still has to treat a plain error as a failure.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// ErrorBody is the wire error shape shared by the REST API and the CLI
// `--json` convention (docs/modules/controlplane.md): {"code","message","hint"}.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Error is the error type returned for every failed control-plane call.
// The server's error body is passed through verbatim; Status and RequestID
// carry the transport-level context (RequestID is the echoed X-Request-Id,
// usable with `agenthub audit tail --request-id`).
type Error struct {
	ErrorBody
	// Status is the HTTP status code of the response (0 if none was read).
	Status int `json:"-"`
	// RequestID is the X-Request-Id echoed by the daemon.
	RequestID string `json:"-"`
	// Generation is the registry generation the daemon reported alongside
	// an optimistic-concurrency refusal (ErrCodeStalePrecondition). It is 0
	// on every other error and must never be read as "the current
	// generation" in the general case — see ConflictError, which is the
	// typed form a caller branches on.
	Generation uint64 `json:"-"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	s := "agenthub api: " + e.Code + ": " + e.Message
	if e.Hint != "" {
		s += " (hint: " + e.Hint + ")"
	}
	return s
}
