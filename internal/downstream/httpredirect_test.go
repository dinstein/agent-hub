package downstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// staticToken is a TokenSource that always hands back the same credential.
// Refresh returning the same value keeps the 401 retry path from looping.
type staticToken string

func (s staticToken) Token(context.Context) (string, bool, error) { return string(s), true, nil }
func (s staticToken) Refresh(context.Context) (string, error)     { return string(s), nil }

// TestAuthClientDoesNotFollowCredentialAcrossOrigins is the regression for
// the credential-exfiltration-by-redirect the 2026-07-31 sweep confirmed.
//
// net/http strips Authorization itself when a redirect crosses to another
// domain — but the header this package injects is not on the request when
// that stripping runs. authRoundTripper sits BELOW the redirect loop, so it
// runs again for the redirected hop, sees the freshly emptied header, and
// re-attaches the vault bearer to a request now aimed at the redirect
// target. The stripping is therefore undone by the very layer it was meant
// to protect.
//
// A downstream that answers 3xx is all it takes: the destination is chosen
// by whoever runs the server the credential was minted for.
func TestAuthClientDoesNotFollowCredentialAcrossOrigins(t *testing.T) {
	seen := make(chan string, 4)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(headerAuthorization)
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	endpoint, err := url.Parse(origin.URL + "/mcp")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}

	client := newAuthClient(nil, staticToken("SECRET"), endpoint)
	req, err := http.NewRequest(http.MethodPost, endpoint.String(), strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Error("the cross-origin redirect was followed; a credential-carrying client must refuse it")
	}

	select {
	case got := <-seen:
		if got != "" {
			t.Fatalf("the redirect target received the credential: Authorization=%q", got)
		}
		t.Fatal("the redirect target was reached at all")
	default:
	}
}

// TestAuthClientFollowsSameOriginRedirect keeps the fix from becoming "no
// redirects at all". A downstream is allowed to move its own endpoint
// around within its own origin, and the credential travels with it.
func TestAuthClientFollowsSameOriginRedirect(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			http.Redirect(w, r, "/mcp/v2", http.StatusTemporaryRedirect)
			return
		}
		got = r.Header.Get(headerAuthorization)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	endpoint, err := url.Parse(srv.URL + "/mcp")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	client := newAuthClient(nil, staticToken("SECRET"), endpoint)
	resp, err := client.Post(endpoint.String(), "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("same-origin redirect was refused: %v", err)
	}
	_ = resp.Body.Close()
	if got != "Bearer SECRET" {
		t.Fatalf("credential did not survive a same-origin redirect: Authorization=%q", got)
	}
}

// TestAttachBearerRefusesForeignOrigin is the second, independent gate.
//
// CheckRedirect above stops the redirect being followed at all; this one
// stops the credential being attached even if a request for another origin
// somehow reaches the round tripper. Two gates, deliberately — per AGENTS.md
// a fail-closed path is never collapsed into the one above it.
func TestAttachBearerRefusesForeignOrigin(t *testing.T) {
	endpoint, err := url.Parse("https://vendor.example/mcp")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	rt := newAuthRoundTripper(nil, staticToken("SECRET"), endpoint)

	for _, target := range []string{
		"https://attacker.example/collect", // another host
		"http://vendor.example/mcp",        // scheme downgrade, same host
		"https://sub.vendor.example/mcp",   // subdomain: net/http would keep the header
		"https://vendor.example:8443/mcp",  // another port
	} {
		req, err := http.NewRequest(http.MethodPost, target, nil)
		if err != nil {
			t.Fatalf("build request for %s: %v", target, err)
		}
		out := rt.attach(req, "SECRET")
		if got := out.Header.Get(headerAuthorization); got != "" {
			t.Errorf("%s received the credential: Authorization=%q", target, got)
		}
	}

	req, err := http.NewRequest(http.MethodPost, "https://vendor.example/mcp/v2", nil)
	if err != nil {
		t.Fatalf("build same-origin request: %v", err)
	}
	if got := rt.attach(req, "SECRET").Header.Get(headerAuthorization); got != "Bearer SECRET" {
		t.Fatalf("the configured origin was denied its own credential: Authorization=%q", got)
	}
}
