package oauthflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Grant types used by this package.
const (
	GrantAuthorizationCode = "authorization_code"
	GrantRefreshToken      = "refresh_token"
	GrantDeviceCode        = "urn:ietf:params:oauth:grant-type:device_code"
)

// ScopeOfflineAccess is named here only so the invariant below can be
// stated precisely: this package NEVER adds it to a request the caller did
// not ask for.
//
// Adding offline_access unilaterally looks like a convenience (you get a
// refresh token!) and is actually a consent-scope escalation: on several
// providers it turns a session-lifetime grant into a durable one, and on
// others it makes the whole authorization fail because the client is not
// allowed that scope. The requested scope set is the caller's, verbatim.
const ScopeOfflineAccess = "offline_access"

// AuthorizeRequest is one authorization request.
type AuthorizeRequest struct {
	// Metadata supplies authorization_endpoint. Required.
	Metadata *AuthServerMetadata
	// ClientID is required.
	ClientID string
	// RedirectURI must match the registered value exactly. Empty omits it
	// (legal only when the client has exactly one registered URI).
	RedirectURI string
	// Scopes is the requested scope set, sent verbatim (see
	// ScopeOfflineAccess).
	Scopes []string
	// Resource is the RFC 8707 resource indicator. Empty omits the
	// parameter. Binding the token to the resource is what stops a token
	// minted for one MCP server from being replayed against another.
	Resource string
	// State is the CSRF/cross-flow binding, verified locally on callback.
	State string
	// PKCE is required; there is no path that omits it.
	PKCE *PKCE
	// Extra carries provider-specific parameters (audience=, prompt=, ...).
	// It can add parameters but never override the ones above.
	Extra url.Values
}

// BuildAuthorizeURL renders the authorization request URL.
//
// Failure direction: missing endpoint, client_id, state or PKCE are errors.
// In particular a nil PKCE never yields a URL without a code_challenge —
// an authorization code obtained without PKCE is interceptable, and this
// package has no mode in which that is acceptable.
func BuildAuthorizeURL(req AuthorizeRequest) (string, error) {
	if req.Metadata == nil || strings.TrimSpace(req.Metadata.AuthorizationEndpoint) == "" {
		return "", newFlowError(ErrorTypeAuthorization, fmt.Errorf("oauthflow: no authorization_endpoint"))
	}
	if strings.TrimSpace(req.ClientID) == "" {
		return "", newFlowError(ErrorTypeAuthorization, fmt.Errorf("oauthflow: no client_id"))
	}
	if strings.TrimSpace(req.State) == "" {
		return "", newFlowError(ErrorTypeAuthorization, fmt.Errorf("oauthflow: no state"))
	}
	if req.PKCE == nil || req.PKCE.Challenge == "" || req.PKCE.Method != ChallengeMethodS256 {
		return "", newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: authorization requires an S256 PKCE challenge"))
	}
	u, err := url.Parse(req.Metadata.AuthorizationEndpoint)
	if err != nil {
		return "", newFlowError(ErrorTypeAuthorization,
			fmt.Errorf("oauthflow: bad authorization_endpoint %q: %w", req.Metadata.AuthorizationEndpoint, err))
	}
	q := u.Query()
	// Provider extras first, so the protocol parameters below always win.
	for k, vs := range req.Extra {
		q.Del(k)
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	q.Set("response_type", "code")
	q.Set("client_id", req.ClientID)
	q.Set("state", req.State)
	q.Set("code_challenge", req.PKCE.Challenge)
	q.Set("code_challenge_method", req.PKCE.Method)
	if req.RedirectURI != "" {
		q.Set("redirect_uri", req.RedirectURI)
	}
	if s := strings.TrimSpace(strings.Join(req.Scopes, " ")); s != "" {
		q.Set("scope", s)
	}
	if req.Resource != "" {
		q.Set("resource", req.Resource)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// TokenResponse is an RFC 6749 §5.1 successful token response.
type TokenResponse struct {
	AccessToken  string
	TokenType    string
	RefreshToken string
	Scope        string
	// ExpiresIn is the advertised lifetime in seconds. Zero means the
	// provider sent none, which per docs/modules/oauth.md means "never expires",
	// NOT "already expired".
	ExpiresIn int64
	// IDToken is captured but unused; agenthub authenticates to resource
	// servers, it does not consume OIDC identity.
	IDToken string
}

// lenientNumber decodes a JSON member that providers send inconsistently
// as a number, a quoted number, a float, or null.
//
// Failure direction: it NEVER fails. An undecodable value becomes 0, and
// every consumer here reads 0 as "the provider told us nothing" — which for
// expires_in means "never expires" (docs/modules/oauth.md). The alternative,
// failing the decode, would throw away a perfectly usable access token
// because of a field we only need for scheduling.
type lenientNumber int64

// UnmarshalJSON implements json.Unmarshaler.
func (n *lenientNumber) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*n = lenientNumber(v)
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*n = lenientNumber(int64(f))
		return nil
	}
	*n = 0
	return nil
}

// rawTokenResponse decodes the wire form.
type rawTokenResponse struct {
	AccessToken  string        `json:"access_token"`
	TokenType    string        `json:"token_type"`
	RefreshToken string        `json:"refresh_token"`
	Scope        string        `json:"scope"`
	ExpiresIn    lenientNumber `json:"expires_in"`
	IDToken      string        `json:"id_token"`
}

func parseTokenResponse(body []byte) (*TokenResponse, error) {
	var raw rawTokenResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, newFlowError(ErrorTypeTokenExchange,
			fmt.Errorf("oauthflow: token response is not JSON: %w", err))
	}
	if strings.TrimSpace(raw.AccessToken) == "" {
		return nil, newFlowError(ErrorTypeTokenExchange,
			fmt.Errorf("oauthflow: token response carries no access_token"))
	}
	// A malformed or negative expires_in degrades to 0 = "never expires"
	// rather than failing the login. The reverse (treating it as expired)
	// would produce an immediate refresh storm against a provider that
	// just told us something we could not parse.
	expires := int64(raw.ExpiresIn)
	if expires < 0 {
		expires = 0
	}
	return &TokenResponse{
		AccessToken:  raw.AccessToken,
		TokenType:    raw.TokenType,
		RefreshToken: raw.RefreshToken,
		Scope:        raw.Scope,
		ExpiresIn:    expires,
		IDToken:      raw.IDToken,
	}, nil
}

// ExchangeRequest is an authorization_code token request.
type ExchangeRequest struct {
	TokenEndpoint string
	ClientID      string
	ClientSecret  string
	Code          string
	RedirectURI   string
	// CodeVerifier is the PKCE secret. Required.
	CodeVerifier string
	// Resource is the RFC 8707 indicator; it must match the one sent on
	// the authorization request or the AS will reject the exchange.
	Resource string
}

// Exchange trades an authorization code for tokens.
//
// The request goes out on the zero-redirect credential client: it carries
// the code and the code_verifier, and a 3xx here is an exfiltration
// primitive, not a routing detail.
func (c *Client) Exchange(ctx context.Context, req ExchangeRequest) (*TokenResponse, error) {
	if strings.TrimSpace(req.CodeVerifier) == "" {
		return nil, newFlowError(ErrorTypeTokenExchange,
			fmt.Errorf("oauthflow: refusing a code exchange without a PKCE verifier"))
	}
	form := url.Values{}
	form.Set("grant_type", GrantAuthorizationCode)
	form.Set("code", req.Code)
	form.Set("client_id", req.ClientID)
	form.Set("code_verifier", req.CodeVerifier)
	if req.RedirectURI != "" {
		form.Set("redirect_uri", req.RedirectURI)
	}
	if req.ClientSecret != "" {
		form.Set("client_secret", req.ClientSecret)
	}
	if req.Resource != "" {
		form.Set("resource", req.Resource)
	}
	body, err := c.postForm(ctx, req.TokenEndpoint, form, nil)
	if err != nil {
		return nil, wrapTokenError(ErrorTypeTokenExchange, err)
	}
	return parseTokenResponse(body)
}

// RefreshRequest is a refresh_token grant request.
type RefreshRequest struct {
	TokenEndpoint string
	ClientID      string
	ClientSecret  string
	RefreshToken  string
	Resource      string
	// Scope may narrow the grant; empty sends none (the AS then reuses the
	// original scope). Never widened here.
	Scope string
}

// Refresh exchanges a refresh token for a new access token. Zero redirects,
// same as Exchange.
//
// Note for callers: the response may or may not carry a NEW refresh token.
// Providers that rotate send one and invalidate the old one immediately —
// which is exactly why Store.Save writes the state (holding the new refresh
// token) before the access token.
func (c *Client) Refresh(ctx context.Context, req RefreshRequest) (*TokenResponse, error) {
	if strings.TrimSpace(req.RefreshToken) == "" {
		return nil, newFlowError(ErrorTypeRefresh, ErrNoRefreshToken)
	}
	form := url.Values{}
	form.Set("grant_type", GrantRefreshToken)
	form.Set("refresh_token", req.RefreshToken)
	form.Set("client_id", req.ClientID)
	if req.ClientSecret != "" {
		form.Set("client_secret", req.ClientSecret)
	}
	if req.Resource != "" {
		form.Set("resource", req.Resource)
	}
	if req.Scope != "" {
		form.Set("scope", req.Scope)
	}
	body, err := c.postForm(ctx, req.TokenEndpoint, form, nil)
	if err != nil {
		return nil, wrapTokenError(ErrorTypeRefresh, err)
	}
	return parseTokenResponse(body)
}

// wrapTokenError keeps an already-typed FlowError intact (blocked, redirect
// and transport classifications must survive) and wraps a bare *TokenError
// so the caller sees a FlowError with the right type and suggestion.
func wrapTokenError(t ErrorType, err error) error {
	var fe *FlowError
	if errors.As(err, &fe) {
		return fe
	}
	e := newFlowError(t, err)
	var te *TokenError
	if errors.As(err, &te) && te.IsInvalidGrant() {
		e.Suggestion = "the grant was already used, rotated or revoked; run `agenthub auth login` for this server"
	}
	return e
}

// ExpiresAt converts an advertised lifetime into an absolute deadline.
// A zero/absent ExpiresIn yields the zero Time, which State encodes as 0 =
// never expires (docs/modules/oauth.md).
func (t *TokenResponse) ExpiresAt(now time.Time) time.Time {
	if t == nil || t.ExpiresIn <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}
