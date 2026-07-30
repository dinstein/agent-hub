package oauthflow

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinels. Every error this package returns is either one of these or a
// *FlowError that unwraps to one, so callers classify with errors.Is and
// never by string matching.
var (
	// ErrBlocked reports a request refused by the SSRF screen (host
	// predicate or dial-time control). Fail-closed: uncertainty blocks.
	ErrBlocked = errors.New("oauthflow: destination blocked")

	// ErrInsecureTransport reports a non-HTTPS endpoint that is not an
	// explicitly allowed loopback address.
	ErrInsecureTransport = errors.New("oauthflow: insecure transport")

	// ErrDiscovery reports that no authorization server metadata document
	// could be obtained.
	ErrDiscovery = errors.New("oauthflow: authorization server metadata not found")

	// ErrRegistration reports a failed dynamic client registration.
	ErrRegistration = errors.New("oauthflow: client registration failed")

	// ErrNotImplemented reports a seam that exists but has no
	// implementation yet (see clientIDMetadataRegistrar).
	ErrNotImplemented = errors.New("oauthflow: not implemented")

	// ErrEntropy reports a crypto/rand failure. It is NEVER downgraded to
	// a weaker random source or to PKCE method "plain".
	ErrEntropy = errors.New("oauthflow: secure random source unavailable")

	// ErrStateMismatch reports a callback whose state parameter does not
	// match the one this flow generated (cross-flow confusion or a pasted
	// callback from somebody else's session).
	ErrStateMismatch = errors.New("oauthflow: state mismatch")

	// ErrIssuerMismatch reports an authorization response whose iss
	// parameter (RFC 9207) does not identify the authorization server this
	// flow discovered — the mix-up attack shape. The code is never redeemed.
	ErrIssuerMismatch = errors.New("oauthflow: authorization response issuer mismatch")

	// ErrRedirect reports a 3xx on a request that carries credentials.
	// Zero redirects is a hard rule, not a limit.
	ErrRedirect = errors.New("oauthflow: credential request was redirected")

	// ErrTimeout reports that the user did not complete authorization in
	// time (loopback: 180s total; device: expires_in).
	ErrTimeout = errors.New("oauthflow: authorization timed out")

	// ErrAuthorizationDenied reports an explicit denial by the user or AS.
	ErrAuthorizationDenied = errors.New("oauthflow: authorization denied")

	// ErrNoState reports that a server has no stored OAuth state at all.
	ErrNoState = errors.New("oauthflow: no stored oauth state")

	// ErrNoToken reports that OAuth state exists (e.g. DCR credentials
	// only) but no access token does. Distinct from ErrNoState and from an
	// empty token on purpose: the reconnect path must stop, not retry with
	// "" (docs/modules/oauth.md).
	ErrNoToken = errors.New("oauthflow: no access token stored")

	// ErrNoRefreshToken reports a refresh attempt on state that carries no
	// refresh token. The only remedy is an interactive login.
	ErrNoRefreshToken = errors.New("oauthflow: no refresh token stored")

	// ErrRefreshSuperseded reports that another writer already refreshed
	// this server's token while we waited for the lock, so this refresh
	// was abandoned. It is a success-shaped outcome, not a failure: the
	// caller should re-read the vault.
	ErrRefreshSuperseded = errors.New("oauthflow: refresh superseded by another writer")

	// ErrMalformedCallback reports pasted input that yields no code.
	ErrMalformedCallback = errors.New("oauthflow: malformed callback input")
)

// ErrorType is the coarse classification carried by FlowError. It is the
// `error_type` field of docs/modules/oauth.md's structured flow error and is what
// the CLI/ctlapi switch on to pick an operator-facing remedy.
type ErrorType string

const (
	ErrorTypeUnknown       ErrorType = "unknown"
	ErrorTypeBlocked       ErrorType = "blocked"
	ErrorTypeTransport     ErrorType = "transport"
	ErrorTypeDiscovery     ErrorType = "discovery"
	ErrorTypeRegistration  ErrorType = "registration"
	ErrorTypeEntropy       ErrorType = "entropy"
	ErrorTypeAuthorization ErrorType = "authorization"
	ErrorTypeTokenExchange ErrorType = "token_exchange"
	ErrorTypeRefresh       ErrorType = "refresh"
	ErrorTypePersistence   ErrorType = "persistence"
)

// DiscoveryStatus records how far metadata discovery got by the time the
// flow failed. Provider-specific failures are diagnosable only with this:
// "DCR 403" means something completely different depending on whether the
// metadata document was real or synthesized from defaults.
type DiscoveryStatus string

const (
	DiscoveryNotAttempted DiscoveryStatus = "not_attempted"
	DiscoveryProtected    DiscoveryStatus = "protected_resource_metadata" // RFC 9728 hop succeeded
	DiscoveryOK           DiscoveryStatus = "ok"
	DiscoveryDefaults     DiscoveryStatus = "fell_back_to_default_endpoints"
	// DiscoveryPinnedAuthz marks metadata whose authorization_endpoint was
	// REPLACED by an operator-supplied value. Discovery itself may well
	// have succeeded; what this records is that the URL the user's browser
	// was sent to did not come from the provider's metadata. "Consent
	// screen 400" means something completely different under this status.
	DiscoveryPinnedAuthz DiscoveryStatus = "pinned_authorization_endpoint"
	// DiscoveryResourceOrigin marks metadata fetched from the RESOURCE
	// server's own origin rather than from an issuer. RFC 8414 §3 anchors
	// metadata at the issuer, so this document was found somewhere the spec
	// does not name, and its `issuer` member was NOT confirmed by the
	// location it came from. That distinction matters for diagnosis: under
	// this status the endpoints came from the host we were already talking
	// to, which is also the shape a mix-up attack takes.
	DiscoveryResourceOrigin DiscoveryStatus = "resource_origin_metadata"
	DiscoveryFailed         DiscoveryStatus = "failed"
)

// RegistrationStatus records the client-credential provenance.
type RegistrationStatus string

const (
	RegistrationNotAttempted RegistrationStatus = "not_attempted"
	RegistrationPreexisting  RegistrationStatus = "preexisting_client_id"
	RegistrationOK           RegistrationStatus = "ok"
	RegistrationFailed       RegistrationStatus = "failed"
)

// FlowError is the structured OAuth flow error of docs/modules/oauth.md. It travels
// unchanged through logs, ctlapi and the CLI so a provider-specific failure
// ("Figma: DCR 403 and empty client_id rejected") produces a precise
// configuration suggestion instead of a bare 401.
//
// FlowError always wraps a sentinel, so errors.Is keeps working through it.
type FlowError struct {
	// Type is the coarse classification.
	Type ErrorType
	// ServerID is the registry server this flow belongs to ("" if unknown).
	ServerID string
	// Issuer is the authorization server issuer under attempt ("" if not
	// yet known).
	Issuer string
	// Discovery is how far metadata discovery got.
	Discovery DiscoveryStatus
	// Registration is the client-credential provenance.
	Registration RegistrationStatus
	// Suggestion is the operator-facing remedy. Never contains secrets.
	Suggestion string
	// CorrelationID ties the log line, the ctlapi event and the CLI
	// message together.
	CorrelationID string
	// Err is the wrapped cause; it carries the sentinel.
	Err error
}

// Error renders a single line. Secrets are never interpolated here: only
// endpoint URLs, statuses and the AS's own error codes reach this string.
func (e *FlowError) Error() string {
	var b strings.Builder
	b.WriteString("oauthflow: ")
	b.WriteString(string(e.Type))
	if e.ServerID != "" {
		fmt.Fprintf(&b, " [server=%s]", e.ServerID)
	}
	if e.Issuer != "" {
		fmt.Fprintf(&b, " [issuer=%s]", e.Issuer)
	}
	fmt.Fprintf(&b, " [discovery=%s registration=%s]", e.discovery(), e.registration())
	if e.CorrelationID != "" {
		fmt.Fprintf(&b, " [corr=%s]", e.CorrelationID)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	if e.Suggestion != "" {
		b.WriteString("; ")
		b.WriteString(e.Suggestion)
	}
	return b.String()
}

// Unwrap exposes the wrapped sentinel to errors.Is/errors.As.
func (e *FlowError) Unwrap() error { return e.Err }

func (e *FlowError) discovery() DiscoveryStatus {
	if e.Discovery == "" {
		return DiscoveryNotAttempted
	}
	return e.Discovery
}

func (e *FlowError) registration() RegistrationStatus {
	if e.Registration == "" {
		return RegistrationNotAttempted
	}
	return e.Registration
}

// newFlowError builds a FlowError with a correlation ID and the default
// suggestion for its type. Callers override Suggestion when they know
// better.
func newFlowError(t ErrorType, err error) *FlowError {
	return &FlowError{
		Type:          t,
		Suggestion:    DefaultSuggestion(t),
		CorrelationID: correlationID(),
		Err:           err,
	}
}

// DefaultSuggestion maps an ErrorType to the generic operator remedy. It is
// exported so the CLI can render the same text for errors it synthesizes.
func DefaultSuggestion(t ErrorType) string {
	switch t {
	case ErrorTypeBlocked:
		return "the authorization server resolves to a private address; pass --allow-loopback only for a self-hosted AS you trust"
	case ErrorTypeTransport:
		return "OAuth endpoints must be https (http is accepted only for explicitly allowed loopback addresses)"
	case ErrorTypeDiscovery:
		return "check the server's URL/issuer, or configure authorization_endpoint and token_endpoint explicitly"
	case ErrorTypeRegistration:
		return "this provider may not support dynamic client registration; register a client manually and set its client_id"
	case ErrorTypeEntropy:
		return "the OS random source failed; this is never worked around — retry, and investigate the host if it persists"
	case ErrorTypeAuthorization:
		return "re-run the login and complete the consent screen; use --manual on a host without a browser"
	case ErrorTypeTokenExchange:
		return "verify the registered redirect_uri matches exactly and that the client is allowed the requested scopes"
	case ErrorTypeRefresh:
		return "the refresh token may have been rotated or revoked; run `agenthub auth login` for this server"
	case ErrorTypePersistence:
		return "the credential vault could not be written; check the secrets backend with `agenthub doctor`"
	default:
		return ""
	}
}

// TokenError is an RFC 6749 §5.2 error response from a token or
// registration endpoint.
type TokenError struct {
	// Code is the machine-readable `error` member.
	Code string
	// Description is the optional `error_description`.
	Description string
	// URI is the optional `error_uri`.
	URI string
	// HTTPStatus is the transport status that carried it.
	HTTPStatus int
}

func (e *TokenError) Error() string {
	s := fmt.Sprintf("oauth error %q (HTTP %d)", e.Code, e.HTTPStatus)
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

// RFC 6749 / RFC 8628 error codes this package reacts to by name.
const (
	errAuthorizationPending = "authorization_pending"
	errSlowDown             = "slow_down"
	errAccessDenied         = "access_denied"
	errExpiredToken         = "expired_token"
	errInvalidGrant         = "invalid_grant"
)

// IsAuthorizationPending reports the device-flow "keep polling" signal.
func (e *TokenError) IsAuthorizationPending() bool { return e.Code == errAuthorizationPending }

// IsSlowDown reports the device-flow backoff signal (RFC 8628 §3.5: the
// poll interval must be increased, permanently, by at least 5 seconds).
func (e *TokenError) IsSlowDown() bool { return e.Code == errSlowDown }

// IsInvalidGrant reports a spent/revoked/rotated grant. For a refresh this
// is terminal: only an interactive login recovers.
func (e *TokenError) IsInvalidGrant() bool { return e.Code == errInvalidGrant }

// Is lets errors.Is classify AS-side denials without the caller reaching
// for the code strings.
func (e *TokenError) Is(target error) bool {
	switch target {
	case ErrAuthorizationDenied:
		return e.Code == errAccessDenied
	case ErrTimeout:
		return e.Code == errExpiredToken
	}
	return false
}
