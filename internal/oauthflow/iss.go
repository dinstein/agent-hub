package oauthflow

import (
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
