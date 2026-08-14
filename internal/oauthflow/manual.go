package oauthflow

import (
	"fmt"
	"net/url"
	"strings"
)

// ManualInstructions is what `auth login --manual` prints. Rendering lives
// in the CLI; this type is the data so the human table and the --json/NDJSON
// stream come from one structure (docs/subsystems/docs/subsystems/controlplane.md).
type ManualInstructions struct {
	// AuthorizationURL is opened by the user on ANY device with a browser.
	AuthorizationURL string
	// RedirectURI is the loopback URI the browser will fail to reach. That
	// failure is expected and is explained to the user: the address bar
	// still holds the code.
	RedirectURI string
	// State is shown so the user can sanity-check the URL they were given.
	// It is not a secret (it is in the URL the AS sees) — the secret is the
	// PKCE verifier, which never leaves this process.
	State string
}

// NewManualInstructions builds the printable half of the single-command
// paste loop of ruling A.2 #9.
//
// Why single-command and not a two-step "get URL" / "submit code" pair, and
// why not the `urn:ietf:wg:oauth:2.0:oob` redirect:
//
//   - oob is deprecated and refused by every major AS.
//   - A two-step form would have to persist the PKCE verifier and the state
//     between two process invocations, i.e. write a short-lived secret to
//     disk for no gain.
//   - Keeping both in memory for the duration of one command means state
//     validation still happens locally, with nothing to steal in between.
//
// The redirect_uri stays http://127.0.0.1:<port>/callback. That address
// refers to the USER's machine, not the headless one running agenthub, so
// the browser lands on a connection-refused page with code and state intact
// in the URL bar — which is exactly what the user pastes back.
//
// The port in redirectURI is real: manual mode still binds nothing, but the
// URI must be the one registered with the AS, so callers pass the same
// value they registered.
func NewManualInstructions(authorizationURL, redirectURI, state string) ManualInstructions {
	return ManualInstructions{AuthorizationURL: authorizationURL, RedirectURI: redirectURI, State: state}
}

// ManualRedirectURI is the redirect URI manual mode registers when the
// provider does not require an exact pre-registered port. It points at the
// USER's loopback interface, which the headless host never binds.
const ManualRedirectURI = "http://127.0.0.1:8642" + DefaultCallbackPath

// ParseManualCallback extracts the authorization code from whatever the
// user pasted, validating state whenever the input carries one.
//
// Accepted shapes:
//
//	http://127.0.0.1:5731/callback?code=X&state=S   full callback URL
//	https://example.com/cb?code=X&state=S           full callback URL (other host)
//	?code=X&state=S                                 query fragment
//	code=X&state=S                                  bare query string
//	X                                               bare code
//
// Surrounding whitespace, angle brackets and quotes (terminals and chat
// clients add all three) are stripped first.
//
// State rules, and why they differ by shape:
//
//   - Input that is or contains a query string MUST carry a state, and it
//     MUST match. A missing state on a URL is treated as a mismatch, not as
//     "no check possible": every AS echoes state when it was sent, so its
//     absence means the URL is not this flow's callback.
//   - A bare code cannot be state-checked — there is nothing to check. It
//     is still accepted, because the user who pastes a bare code has
//     usually stripped the URL by hand. PKCE remains: an intercepted code
//     is useless without the verifier held in this process.
//
// Failure direction: an `error` parameter in the pasted URL is surfaced as
// the AS's own denial — with its `iss`, so the caller can apply RFC 9207
// before acting on it — and anything yielding no code is
// ErrMalformedCallback, never an empty code handed onward.
func ParseManualCallback(input, wantState string) (code, iss string, err error) {
	s := cleanPastedInput(input)
	if s == "" {
		return "", "", newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("%w: nothing pasted", ErrMalformedCallback))
	}
	q, isQuery := pastedQuery(s)
	if !isQuery {
		// Bare code. Reject anything containing whitespace or an obvious
		// URL fragment: that is a mangled paste, not a code. A bare code
		// carries no iss (RFC 9207): the user stripped the URL by hand, so
		// its absence is expected, not suspicious.
		if strings.ContainsAny(s, " \t\n\r") || strings.Contains(s, "://") {
			return "", "", newFlowError(ErrorTypeAuthorization,
				fmt.Errorf("%w: input is neither a callback URL nor a bare code", ErrMalformedCallback))
		}
		return s, "", nil
	}
	// A query carrying neither member is not a callback at all, and saying
	// "state mismatch" about it would send the reader looking for the wrong
	// thing.
	if q.Get("code") == "" && q.Get("error") == "" {
		return "", "", newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("%w: pasted URL has no code parameter", ErrMalformedCallback))
	}
	// State next, and for the error case too: RFC 6749 §4.1.2.1 obliges the
	// AS to echo state on an error response exactly as on a success, so a
	// URL that fails this is not this flow's callback whichever member it
	// carries.
	if got := q.Get("state"); got != wantState {
		e := newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("%w: pasted callback belongs to a different authorization request", ErrStateMismatch))
		e.Suggestion = "paste the URL from the browser tab this login opened, not from an earlier attempt"
		return "", "", e
	}
	if e := q.Get("error"); e != "" {
		te := &TokenError{Code: e, Description: q.Get("error_description"), URI: q.Get("error_uri")}
		fe := newFlowError(ErrorTypeAuthorization, te)
		if e == errAccessDenied {
			fe.Err = fmt.Errorf("%w: %w", ErrAuthorizationDenied, te)
		}
		// iss travels with the failure so the caller can apply RFC 9207
		// before it acts on or displays the AS's error members.
		return "", q.Get("iss"), fe
	}
	return q.Get("code"), q.Get("iss"), nil
}

// cleanPastedInput strips the decorations terminals, mail clients and chat
// apps wrap URLs in.
func cleanPastedInput(in string) string {
	s := strings.TrimSpace(in)
	s = strings.Trim(s, "<>")
	s = strings.Trim(s, "\"'")
	return strings.TrimSpace(s)
}

// pastedQuery decides whether the input carries query parameters and
// returns them. It never guesses: an input is a query only if it parses as
// an absolute URL with a query, or is itself a `k=v&...` string.
func pastedQuery(s string) (url.Values, bool) {
	if strings.HasPrefix(s, "?") {
		v, err := url.ParseQuery(strings.TrimPrefix(s, "?"))
		if err != nil {
			return nil, false
		}
		return v, true
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return nil, false
		}
		// A URL with no query at all is malformed input for this purpose,
		// not a bare code; report it as a query with nothing in it so the
		// caller produces "no code parameter" rather than treating the
		// whole URL as the code.
		return u.Query(), true
	}
	if strings.Contains(s, "code=") || strings.Contains(s, "error=") {
		v, err := url.ParseQuery(s)
		if err != nil {
			return nil, false
		}
		return v, true
	}
	return nil, false
}
