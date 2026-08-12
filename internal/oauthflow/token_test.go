package oauthflow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestParseTokenResponseExpiresIn: providers disagree about the JSON type
// of expires_in. A strict int64 field would fail the whole decode on the
// string-sending ones, losing a perfectly good token.
func TestParseTokenResponseExpiresIn(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"number", `{"access_token":"a","expires_in":3600}`, 3600},
		{"string", `{"access_token":"a","expires_in":"3600"}`, 3600},
		{"float", `{"access_token":"a","expires_in":3600.7}`, 3600},
		{"absent means never expires", `{"access_token":"a"}`, 0},
		{"null means never expires", `{"access_token":"a","expires_in":null}`, 0},
		{"unparsable degrades to never expires", `{"access_token":"a","expires_in":"soon"}`, 0},
		{"negative degrades to never expires", `{"access_token":"a","expires_in":-5}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := parseTokenResponse([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if tok.ExpiresIn != tc.want {
				t.Fatalf("expires_in = %d want %d", tok.ExpiresIn, tc.want)
			}
		})
	}
}

func TestParseTokenResponseRequiresAccessToken(t *testing.T) {
	if _, err := parseTokenResponse([]byte(`{"token_type":"Bearer"}`)); err == nil {
		t.Fatal("a response without access_token must be an error")
	}
	if _, err := parseTokenResponse([]byte(`not json`)); err == nil {
		t.Fatal("a non-JSON body must be an error")
	}
}

func TestTokenResponseExpiresAt(t *testing.T) {
	now := time.Unix(1000, 0)
	if got := (&TokenResponse{ExpiresIn: 60}).ExpiresAt(now); !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("expires at %s", got)
	}
	if got := (&TokenResponse{}).ExpiresAt(now); !got.IsZero() {
		t.Fatal("no expires_in must yield the zero time (never expires)")
	}
	var nilTok *TokenResponse
	if got := nilTok.ExpiresAt(now); !got.IsZero() {
		t.Fatal("nil token response")
	}
}

func TestExchangeRefusesMissingVerifier(t *testing.T) {
	c := NewClient(Config{AllowLoopback: true})
	_, err := c.Exchange(context.Background(), ExchangeRequest{
		TokenEndpoint: "http://127.0.0.1:1/token", ClientID: "c", Code: "x",
	})
	if err == nil {
		t.Fatal("an exchange without a PKCE verifier must be refused before any request")
	}
}

func TestRefreshRefusesMissingRefreshToken(t *testing.T) {
	c := NewClient(Config{AllowLoopback: true})
	_, err := c.Refresh(context.Background(), RefreshRequest{TokenEndpoint: "http://127.0.0.1:1/token"})
	if !errors.Is(err, ErrNoRefreshToken) {
		t.Fatalf("err = %v, want ErrNoRefreshToken", err)
	}
}

// TestInvalidGrantCarriesReLoginSuggestion: a spent or revoked refresh
// token is terminal; the operator needs to be told to re-login rather than
// watch a retry loop.
func TestInvalidGrantCarriesReLoginSuggestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "token revoked"})
	}))
	defer srv.Close()

	c := NewClient(Config{AllowLoopback: true})
	_, err := c.Refresh(context.Background(), RefreshRequest{
		TokenEndpoint: srv.URL + "/token", ClientID: "c", RefreshToken: "r",
	})
	var te *TokenError
	if !errors.As(err, &te) || !te.IsInvalidGrant() {
		t.Fatalf("err = %v, want an invalid_grant TokenError", err)
	}
	var fe *FlowError
	if !errors.As(err, &fe) || fe.Type != ErrorTypeRefresh {
		t.Fatalf("classification: %v", err)
	}
	if fe.Suggestion == "" {
		t.Fatal("invalid_grant must carry a re-login suggestion")
	}
}

// refuseRefresh answers every request with one RFC 6749 §5.2 error body and
// returns what Refresh made of it.
func refuseRefresh(t *testing.T, status int, code string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, status, map[string]any{"error": code, "error_description": "no"})
	}))
	defer srv.Close()
	c := NewClient(Config{AllowLoopback: true})
	_, err := c.Refresh(context.Background(), RefreshRequest{
		TokenEndpoint: srv.URL + "/token", ClientID: "c", RefreshToken: "r",
	})
	if err == nil {
		t.Fatal("a refused refresh must be an error")
	}
	return err
}

// TestTerminalGrantIsRecognizedOnlyInItsExactShape is the guard on the whole
// terminal classification: it decides whether a server is parked until a
// human logs in, so a 500 that happens to carry the word invalid_grant must
// not reach it.
func TestTerminalGrantIsRecognizedOnlyInItsExactShape(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		code    string
		want    error // nil = must NOT be terminal
		wantMsg string
	}{
		{"invalid_grant on 400", 400, "invalid_grant", ErrGrantRevoked, ""},
		{"invalid_grant on 401", 401, "invalid_grant", ErrGrantRevoked, ""},
		{"invalid_client on 401", 401, "invalid_client", ErrClientRejected, ""},
		{"invalid_client on 400", 400, "invalid_client", ErrClientRejected, ""},
		{"invalid_grant on 500 is the AS having a bad day", 500, "invalid_grant", nil, ""},
		{"invalid_grant on 502 is a proxy, not the AS", 502, "invalid_grant", nil, ""},
		{"invalid_request stays retryable", 400, "invalid_request", nil, ""},
		{"an empty error code stays retryable", 400, "", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseRefresh(t, tc.status, tc.code)
			if tc.want == nil {
				if NeedsLogin(err) {
					t.Fatalf("err = %v, want a retryable failure", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !NeedsLogin(err) {
				t.Fatalf("%v must satisfy NeedsLogin", tc.want)
			}
			// The TokenError has to survive the extra wrapping: it carries
			// the AS's own description, which is the only diagnosis an
			// operator gets for a provider-specific refusal.
			var te *TokenError
			if !errors.As(err, &te) || te.Code != tc.code {
				t.Fatalf("the authorization server's own error was lost: %v", err)
			}
			var fe *FlowError
			if !errors.As(err, &fe) || fe.Type != ErrorTypeRefresh || fe.Suggestion == "" {
				t.Fatalf("classification: %v", err)
			}
		})
	}
}

// TestASpentAuthorizationCodeIsNotATerminalGrant: the token exchange returns
// the same code for a consent screen used twice, and treating that as a dead
// credential would park a server on the strength of a mistyped login.
func TestASpentAuthorizationCodeIsNotATerminalGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "code already used"})
	}))
	defer srv.Close()

	c := NewClient(Config{AllowLoopback: true})
	_, err := c.Exchange(context.Background(), ExchangeRequest{
		TokenEndpoint: srv.URL + "/token", ClientID: "c", Code: "x", CodeVerifier: "v",
	})
	if NeedsLogin(err) {
		t.Fatalf("err = %v, want the exchange path to stay out of the terminal classification", err)
	}
	var fe *FlowError
	if !errors.As(err, &fe) || fe.Suggestion == "" {
		t.Fatalf("the exchange must keep its own suggestion: %v", err)
	}
}

func TestNeedsLoginAndIsUnmanaged(t *testing.T) {
	cases := []struct {
		err            error
		login, unmanag bool
	}{
		{ErrGrantRevoked, true, false},
		{ErrClientRejected, true, false},
		{ErrNoRefreshToken, true, false},
		{ErrNoState, false, true},
		{ErrNoToken, false, false},
		{ErrRefreshSuperseded, false, false},
		{newFlowError(ErrorTypeRefresh, ErrGrantRevoked), true, false},
		{nil, false, false},
	}
	for _, tc := range cases {
		if got := NeedsLogin(tc.err); got != tc.login {
			t.Errorf("NeedsLogin(%v) = %t, want %t", tc.err, got, tc.login)
		}
		if got := IsUnmanaged(tc.err); got != tc.unmanag {
			t.Errorf("IsUnmanaged(%v) = %t, want %t", tc.err, got, tc.unmanag)
		}
	}
}

func TestTokenErrorClassification(t *testing.T) {
	pending := &TokenError{Code: errAuthorizationPending}
	if !pending.IsAuthorizationPending() || pending.IsSlowDown() || pending.IsInvalidGrant() {
		t.Fatal("authorization_pending misclassified")
	}
	if !(&TokenError{Code: errSlowDown}).IsSlowDown() {
		t.Fatal("slow_down misclassified")
	}
	denied := &TokenError{Code: errAccessDenied, HTTPStatus: 400}
	if !errors.Is(denied, ErrAuthorizationDenied) {
		t.Fatal("access_denied must satisfy errors.Is(_, ErrAuthorizationDenied)")
	}
	if errors.Is(denied, ErrTimeout) {
		t.Fatal("access_denied is not a timeout")
	}
	if got := denied.Error(); got == "" {
		t.Fatal("empty error text")
	}
	withDesc := &TokenError{Code: "x", Description: "why", HTTPStatus: 400}
	if !contains(withDesc.Error(), "why") {
		t.Fatalf("description missing from %q", withDesc.Error())
	}
}

// TestFlowErrorIsStructured checks that the 7.7 fields survive the error
// chain: error_type, discovery status, registration status, suggestion and
// correlation id all have to reach the CLI.
func TestFlowErrorIsStructured(t *testing.T) {
	base := errors.New("boom")
	fe := &FlowError{
		Type:          ErrorTypeRegistration,
		ServerID:      "figma",
		Issuer:        "https://as.figma.com",
		Discovery:     DiscoveryOK,
		Registration:  RegistrationFailed,
		Suggestion:    "register a client manually",
		CorrelationID: "abc123",
		Err:           base,
	}
	s := fe.Error()
	for _, want := range []string{"registration", "figma", "as.figma.com", "discovery=ok",
		"registration=failed", "abc123", "boom", "register a client manually"} {
		if !contains(s, want) {
			t.Fatalf("error text %q is missing %q", s, want)
		}
	}
	if !errors.Is(fe, base) {
		t.Fatal("FlowError must unwrap to its cause")
	}
	// Zero statuses render as not_attempted rather than empty.
	bare := &FlowError{Type: ErrorTypeUnknown, Err: base}
	if !contains(bare.Error(), "discovery=not_attempted") ||
		!contains(bare.Error(), "registration=not_attempted") {
		t.Fatalf("bare error text = %q", bare.Error())
	}
}

func TestDefaultSuggestionCoversEveryType(t *testing.T) {
	types := []ErrorType{
		ErrorTypeBlocked, ErrorTypeTransport, ErrorTypeDiscovery, ErrorTypeRegistration,
		ErrorTypeEntropy, ErrorTypeAuthorization, ErrorTypeTokenExchange, ErrorTypeRefresh,
		ErrorTypePersistence,
	}
	for _, ty := range types {
		if DefaultSuggestion(ty) == "" {
			t.Fatalf("%s has no default suggestion", ty)
		}
	}
	if DefaultSuggestion(ErrorTypeUnknown) != "" {
		t.Fatal("the unknown type should have no canned suggestion")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
