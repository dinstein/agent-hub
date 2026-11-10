package oauthflow

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// validateResponseIssuer 校验单次登录的上游 issuer 配置，避免含糊或非 HTTPS 的信任声明。
func validateResponseIssuer(issuer string) error {
	if issuer == "" {
		return nil
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(issuer) != issuer {
		return newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: response issuer must be an absolute HTTPS issuer URL"))
	}
	return nil
}

// validateIss enforces RFC 9207 (the iss authorization-response parameter)
// before an authorization code is redeemed. 默认按 §2.4 精确比较发现的 issuer。
// 代理 AS 将上游响应原样转发时，调用方可针对单次登录显式指定 responseIssuer。
//
// Failure direction (fail closed, both ways that matter):
//
//   - 已提供的 iss 与预期 issuer 不同时一律失败：
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
func validateIss(md *AuthServerMetadata, iss string, requireAdvertised bool, responseIssuer string) error {
	if md == nil {
		return nil // nothing discovered to compare against
	}
	expected := md.Issuer
	if responseIssuer != "" {
		expected = responseIssuer
	}
	switch {
	case iss == "":
		if responseIssuer != "" || (requireAdvertised && md.AuthorizationResponseIssParameterSupported) {
			message := fmt.Sprintf("%s advertises authorization_response_iss_parameter_supported but the response carries no iss", md.Issuer)
			suggestion := "the authorization response did not come from the discovered authorization server; re-run the login and check for a proxy or captive portal in the path"
			if responseIssuer != "" {
				message = fmt.Sprintf("expected authorization response issuer %q but the response carries no iss", expected)
				suggestion = "the authorization response did not identify its issuer; re-run the login and check the provider's redirect behavior"
			}
			e := newFlowError(ErrorTypeAuthorization, fmt.Errorf(
				"%w: %s", ErrIssuerMismatch, message))
			e.Issuer = md.Issuer
			e.Suggestion = suggestion
			return e
		}
		return nil
	case iss != expected:
		message := fmt.Sprintf("authorization response names issuer %q, discovery produced %q", iss, md.Issuer)
		suggestion := "the code was issued by a different authorization server than the one discovered; nothing was redeemed"
		if responseIssuer != "" {
			message = fmt.Sprintf("authorization response names issuer %q, expected %q", iss, expected)
			suggestion = "the code was issued by a different authorization server than expected; nothing was redeemed"
		}
		e := newFlowError(ErrorTypeAuthorization, fmt.Errorf(
			"%w: %s", ErrIssuerMismatch, message))
		e.Issuer = md.Issuer
		e.Suggestion = suggestion
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
func issThenCallback(md *AuthServerMetadata, iss string, requireAdvertised bool, callbackErr error, responseIssuer string) error {
	var te *TokenError
	if !errors.As(callbackErr, &te) {
		return callbackErr
	}
	if err := validateIss(md, iss, requireAdvertised, responseIssuer); err != nil {
		return err
	}
	return callbackErr
}
