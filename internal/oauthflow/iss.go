package oauthflow

import (
	"errors"
	"fmt"
)

// validateIss enforces RFC 9207 (the iss authorization-response parameter)
// before an authorization code is redeemed. iss is compared to the
// discovered issuer by simple string equality, exactly as §2.4 prescribes.
//
// Failure direction (fail closed, both ways that matter):
//
//   - A PRESENT iss that differs from the discovered issuer always fails:
//     that is the mix-up attack shape, and the code must not be redeemed.
//   - A MISSING iss fails when requireAdvertised is set and the AS metadata
//     advertises authorization_response_iss_parameter_supported — the
//     genuine AS promised the parameter, so its absence means the response
//     came from somewhere else. An attacker controls which AS answers, not
//     what the genuine AS's metadata advertises.
//
// requireAdvertised is true for the loopback flow, where the callback URL
// arrives intact from the browser. The manual flow passes false: a pasted
// input may have been hand-trimmed to a bare code, which loses the
// parameter without implying an attack (PKCE and state still guard that
// path).
func validateIss(md *AuthServerMetadata, iss string, requireAdvertised bool) error {
	if md == nil {
		return nil // nothing discovered to compare against
	}
	switch {
	case iss == "":
		if requireAdvertised && md.AuthorizationResponseIssParameterSupported {
			e := newFlowError(ErrorTypeAuthorization, fmt.Errorf(
				"%w: %s advertises authorization_response_iss_parameter_supported but the response carries no iss",
				ErrIssuerMismatch, md.Issuer))
			e.Issuer = md.Issuer
			e.Suggestion = "the authorization response did not come from the discovered authorization server; re-run the login and check for a proxy or captive portal in the path"
			return e
		}
		return nil
	case iss != md.Issuer:
		e := newFlowError(ErrorTypeAuthorization, fmt.Errorf(
			"%w: authorization response names issuer %q, discovery produced %q",
			ErrIssuerMismatch, iss, md.Issuer))
		e.Issuer = md.Issuer
		e.Suggestion = "the code was issued by a different authorization server than the one discovered; nothing was redeemed"
		return e
	default:
		return nil
	}
}

// issThenCallback applies RFC 9207 to a callback that FAILED, and returns
// whichever error the caller may act on.
//
// The specification extends iss validation to error responses in as many
// words: "on mismatch the client MUST NOT act on or display `error`,
// `error_description`, or `error_uri`". Those members are attacker-supplied
// text that reaches an operator's terminal and the event log, so a response
// whose issuer does not check out must not be the thing that explains why
// the login failed. The issuer error replaces it.
//
// It applies ONLY to a failure that carries an AS error response — a
// *TokenError in the chain. Every other way a login fails (the browser
// would not open, the endpoint was refused, nothing arrived before the
// deadline) produced no response at all, and validating an iss that was
// never sent would replace the real cause with a fabricated issuer
// mismatch. That is not a hypothetical: it is what the first version of
// this function did, and three tests said so.
func issThenCallback(md *AuthServerMetadata, iss string, requireAdvertised bool, callbackErr error) error {
	var te *TokenError
	if !errors.As(callbackErr, &te) {
		return callbackErr
	}
	if err := validateIss(md, iss, requireAdvertised); err != nil {
		return err
	}
	return callbackErr
}
