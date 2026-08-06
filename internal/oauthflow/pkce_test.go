package oauthflow

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
)

func TestNewPKCEIsAlwaysS256(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if p.Method != ChallengeMethodS256 {
		t.Fatalf("method = %q", p.Method)
	}
	// RFC 7636: 43..128 characters from the unreserved set.
	if len(p.Verifier) < 43 || len(p.Verifier) > 128 {
		t.Fatalf("verifier length %d out of RFC 7636 range", len(p.Verifier))
	}
	if strings.ContainsAny(p.Verifier, "+/=") {
		t.Fatalf("verifier is not base64url-unpadded: %q", p.Verifier)
	}
	sum := sha256.Sum256([]byte(p.Verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); p.Challenge != want {
		t.Fatalf("challenge = %q want %q", p.Challenge, want)
	}
	q, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if q.Verifier == p.Verifier {
		t.Fatal("two verifiers collided")
	}
}

// TestEntropyFailureIsNeverDowngraded is the load-bearing test of the whole
// PKCE story: when crypto/rand fails, every generator errors out. Nothing
// falls back to "plain", to a shorter token, or to a weaker source.
func TestEntropyFailureIsNeverDowngraded(t *testing.T) {
	orig := randRead
	t.Cleanup(func() { randRead = orig })
	randRead = func([]byte) (int, error) { return 0, errors.New("entropy pool drained") }

	if p, err := NewPKCE(); err == nil {
		t.Fatalf("NewPKCE succeeded with a broken rand: %+v", p)
	} else if !errors.Is(err, ErrEntropy) {
		t.Fatalf("err = %v, want ErrEntropy", err)
	}
	if _, err := NewState(); !errors.Is(err, ErrEntropy) {
		t.Fatalf("NewState err = %v, want ErrEntropy", err)
	}
	// Correlation IDs are diagnostics, not security: they degrade instead
	// of failing, so a broken rand cannot turn a working login into an
	// error via the error-reporting path itself.
	if got := correlationID(); got != "corr-unavailable" {
		t.Fatalf("correlationID = %q", got)
	}
}

// TestShortReadIsNotAcceptedSilently: a truncated read must fail, not
// produce a short verifier.
func TestShortReadIsNotAcceptedSilently(t *testing.T) {
	orig := randRead
	t.Cleanup(func() { randRead = orig })
	randRead = func(b []byte) (int, error) {
		if len(b) > 1 {
			return len(b) / 2, io.EOF // half the bytes, then end of stream
		}
		return len(b), nil
	}
	if _, err := NewPKCE(); err == nil {
		t.Fatal("a short read must not yield a verifier")
	}
}

// TestSupportsS256 pins the line between "this document says no PKCE" and
// "there is no document". Both MCP revisions make an absent
// code_challenge_methods_supported mean the server does not support PKCE and
// the client MUST refuse; a synthesized document has nothing to have omitted.
func TestSupportsS256(t *testing.T) {
	issuer, err := parseAbsoluteURL("https://as.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		md   *AuthServerMetadata
		want bool
	}{
		{
			// No document was fetched at all, so nothing omitted anything.
			// Whether to proceed against a provider that publishes no
			// metadata is the AllowDefaultEndpoints decision, taken before
			// this is reached.
			name: "no metadata at all", md: nil, want: true,
		},
		{
			name: "synthesized endpoints", md: DefaultEndpoints(issuer), want: true,
		},
		{
			// The change: this used to pass on the recorded grounds that
			// omission "is common". Measured against this repository's own
			// seed catalog, not one provider omitted it.
			name: "fetched document, field absent", want: false,
			md: &AuthServerMetadata{Issuer: "https://as.example.com", SourceURL: "https://as.example.com/.well-known/oauth-authorization-server"},
		},
		{
			name: "advertises S256", want: true,
			md: &AuthServerMetadata{SourceURL: "https://x/.well-known/oauth-authorization-server",
				CodeChallengeMethodsSupported: []string{"plain", "S256"}},
		},
		{
			// plain FIRST: nine of the measured servers order it this way,
			// so a client picking element zero would pick plain. Membership,
			// never an index.
			name: "advertises S256 after plain", want: true,
			md: &AuthServerMetadata{SourceURL: "https://x/.well-known/oauth-authorization-server",
				CodeChallengeMethodsSupported: []string{"S256", "plain"}},
		},
		{
			name: "plain only", want: false,
			md: &AuthServerMetadata{SourceURL: "https://x/.well-known/oauth-authorization-server",
				CodeChallengeMethodsSupported: []string{"plain"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SupportsS256(tt.md); got != tt.want {
				t.Fatalf("SupportsS256 = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildAuthorizeURL(t *testing.T) {
	md := &AuthServerMetadata{AuthorizationEndpoint: "https://as.example.com/authorize?tenant=t1"}
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := BuildAuthorizeURL(AuthorizeRequest{
		Metadata:    md,
		ClientID:    "client-abc",
		RedirectURI: "http://127.0.0.1:5731/callback",
		Scopes:      []string{"read", "write"},
		Resource:    "https://mcp.example.com/mcp",
		State:       "state-1",
		PKCE:        pkce,
		Extra:       url.Values{"prompt": {"consent"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             "client-abc",
		"redirect_uri":          "http://127.0.0.1:5731/callback",
		"scope":                 "read write",
		"resource":              "https://mcp.example.com/mcp",
		"state":                 "state-1",
		"code_challenge":        pkce.Challenge,
		"code_challenge_method": "S256",
		"prompt":                "consent",
		"tenant":                "t1", // endpoint's own query survives
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q want %q", k, q.Get(k), v)
		}
	}
}

// TestRequestedScopeIsVerbatim: offline_access is never added on the
// client's behalf — doing so silently escalates the consent scope.
func TestRequestedScopeIsVerbatim(t *testing.T) {
	md := &AuthServerMetadata{AuthorizationEndpoint: "https://as.example.com/authorize"}
	pkce, _ := NewPKCE()
	base := AuthorizeRequest{Metadata: md, ClientID: "c", State: "s", PKCE: pkce}

	raw, err := BuildAuthorizeURL(base)
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := url.Parse(raw); u.Query().Has("scope") {
		t.Fatalf("no scopes requested but scope was sent: %q", u.Query().Get("scope"))
	}

	withScopes := base
	withScopes.Scopes = []string{"read"}
	raw, err = BuildAuthorizeURL(withScopes)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustQuery(t, raw).Get("scope"); got != "read" {
		t.Fatalf("scope = %q, want exactly what the caller asked for", got)
	}
	if strings.Contains(raw, ScopeOfflineAccess) {
		t.Fatal("offline_access must never be added implicitly")
	}

	// Explicitly asking for it still works.
	explicit := base
	explicit.Scopes = []string{"read", ScopeOfflineAccess}
	raw, err = BuildAuthorizeURL(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustQuery(t, raw).Get("scope"); got != "read offline_access" {
		t.Fatalf("scope = %q", got)
	}
}

// TestNoResourceParamWhenUnset: RFC 8707 binding is opt-in; sending an
// empty resource makes some ASes reject the request outright.
func TestNoResourceParamWhenUnset(t *testing.T) {
	pkce, _ := NewPKCE()
	raw, err := BuildAuthorizeURL(AuthorizeRequest{
		Metadata: &AuthServerMetadata{AuthorizationEndpoint: "https://as/authorize"},
		ClientID: "c", State: "s", PKCE: pkce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mustQuery(t, raw).Has("resource") {
		t.Fatal("resource must be omitted when unset")
	}
}

func TestBuildAuthorizeURLRefusesMissingPKCE(t *testing.T) {
	md := &AuthServerMetadata{AuthorizationEndpoint: "https://as/authorize"}
	cases := []struct {
		name string
		req  AuthorizeRequest
	}{
		{"nil pkce", AuthorizeRequest{Metadata: md, ClientID: "c", State: "s"}},
		{"empty challenge", AuthorizeRequest{Metadata: md, ClientID: "c", State: "s", PKCE: &PKCE{Method: "S256"}}},
		{"plain method", AuthorizeRequest{Metadata: md, ClientID: "c", State: "s", PKCE: &PKCE{Challenge: "x", Method: "plain"}}},
		{"no state", AuthorizeRequest{Metadata: md, ClientID: "c", PKCE: &PKCE{Challenge: "x", Method: "S256"}}},
		{"no client id", AuthorizeRequest{Metadata: md, State: "s", PKCE: &PKCE{Challenge: "x", Method: "S256"}}},
		{"no endpoint", AuthorizeRequest{Metadata: &AuthServerMetadata{}, ClientID: "c", State: "s", PKCE: &PKCE{Challenge: "x", Method: "S256"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildAuthorizeURL(tc.req); err == nil {
				t.Fatal("expected refusal")
			}
		})
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}
