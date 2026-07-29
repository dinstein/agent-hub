package oauthflow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/guard/netguard"
)

// TestSSRFRefusesPrivateDestinations is the fail-closed half of the SSRF
// contract: without the explicit AllowLoopback opt-in, a private or
// loopback OAuth endpoint is refused BEFORE any request is made.
func TestSSRFRefusesPrivateDestinations(t *testing.T) {
	c := NewClient(Config{}) // AllowLoopback deliberately off
	cases := []struct {
		name string
		url  string
		want error
	}{
		{"loopback literal", "https://127.0.0.1:9999/.well-known/oauth-authorization-server", ErrBlocked},
		{"ipv6 loopback", "https://[::1]/token", ErrBlocked},
		{"rfc1918", "https://10.1.2.3/token", ErrBlocked},
		{"link local metadata service", "https://169.254.169.254/token", ErrBlocked},
		{"cgnat", "https://100.100.0.1/token", ErrBlocked},
		{"localhost name", "https://localhost/token", ErrBlocked},
		{"localhost subdomain", "https://as.localhost/token", ErrBlocked},
		{"plain http on a public host", "http://as.example.com/token", ErrInsecureTransport},
		{"non-http scheme", "file:///etc/passwd", ErrInsecureTransport},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.url)
			if err != nil {
				t.Fatal(err)
			}
			err = c.checkURL(u)
			if !errors.Is(err, tc.want) {
				t.Fatalf("checkURL(%s) = %v, want %v", tc.url, err, tc.want)
			}
			var fe *FlowError
			if !errors.As(err, &fe) {
				t.Fatalf("error is not a *FlowError: %v", err)
			}
			if fe.CorrelationID == "" {
				t.Fatal("FlowError carries no correlation id")
			}
		})
	}
}

// TestSSRFAllowLoopbackIsNarrow proves the opt-in unlocks loopback ONLY —
// RFC1918 and link-local stay blocked even with the switch on, and no
// hostname's DNS answer can unlock it.
func TestSSRFAllowLoopbackIsNarrow(t *testing.T) {
	c := NewClient(Config{AllowLoopback: true})
	allowed := []string{
		"http://127.0.0.1:8080/token",
		"http://127.0.0.5/token",
		"https://[::1]:9443/token",
		"http://localhost:1234/token",
		"http://as.localhost/token",
	}
	for _, raw := range allowed {
		u, _ := url.Parse(raw)
		if err := c.checkURL(u); err != nil {
			t.Fatalf("with AllowLoopback, %s should pass: %v", raw, err)
		}
	}
	stillBlocked := []string{
		"https://10.0.0.1/token",
		"https://192.168.1.1/token",
		"https://169.254.169.254/token",
		"http://as.example.com/token", // plain http off-loopback stays refused
	}
	for _, raw := range stillBlocked {
		u, _ := url.Parse(raw)
		if err := c.checkURL(u); err == nil {
			t.Fatalf("AllowLoopback must not unlock %s", raw)
		}
	}
}

// TestDialControlScreensResolvedAddress covers the DNS-rebind half: the
// dial hook sees the address the socket is about to connect to, so a name
// that passed checkURL and then resolved private is still refused.
func TestDialControlScreensResolvedAddress(t *testing.T) {
	c := NewClient(Config{})
	err := c.dialControl("tcp", "10.0.0.1:443", nil)
	if err == nil {
		t.Fatal("dial to a private address must be refused")
	}
	var blocked *netguard.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected a *netguard.BlockedError, got %v", err)
	}
	if err := c.dialControl("tcp", "127.0.0.1:443", nil); err == nil {
		t.Fatal("loopback dial must be refused without AllowLoopback")
	}
	cl := NewClient(Config{AllowLoopback: true})
	if err := cl.dialControl("tcp", "127.0.0.1:443", nil); err != nil {
		t.Fatalf("loopback dial with AllowLoopback: %v", err)
	}
	if err := cl.dialControl("tcp", "10.0.0.1:443", nil); err == nil {
		t.Fatal("AllowLoopback must not unlock RFC1918 at dial time")
	}
}

// TestSSRFBlockedBeforeAnyRequest proves the refusal happens without a
// round trip: the server must record zero hits.
func TestSSRFBlockedBeforeAnyRequest(t *testing.T) {
	as := newFakeAS(t)
	c := NewClient(Config{}) // no AllowLoopback: the httptest server is 127.0.0.1
	d := NewDiscoverer(c)
	_, err := d.DiscoverFromIssuer(context.Background(), as.issuer())
	if !errors.Is(err, ErrInsecureTransport) && !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want a blocked/insecure refusal", err)
	}
	if hits := as.hitList(); len(hits) != 0 {
		t.Fatalf("server was contacted despite the refusal: %q", hits)
	}
}

// TestCredentialPostFollowsZeroRedirects is the zero-redirect invariant: a
// 3xx on the token endpoint is an error, and the redirect target is never
// contacted.
func TestCredentialPostFollowsZeroRedirects(t *testing.T) {
	var sinkHits int
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sinkHits++
		writeJSON(w, 200, map[string]any{"access_token": "leaked", "token_type": "Bearer"})
	}))
	defer sink.Close()

	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, sink.URL+"/collect", http.StatusFound)
	}))
	defer as.Close()

	c := NewClient(Config{AllowLoopback: true})
	_, err := c.Exchange(context.Background(), ExchangeRequest{
		TokenEndpoint: as.URL + "/token",
		ClientID:      "c",
		Code:          "code",
		CodeVerifier:  "verifier",
	})
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("err = %v, want ErrRedirect", err)
	}
	if sinkHits != 0 {
		t.Fatalf("credential request followed the redirect: sink saw %d hits", sinkHits)
	}
	// The redirect target's query must not be echoed into the message: a
	// Location header on a credential request can carry back what we sent.
	if strings.Contains(err.Error(), "code=stolen") {
		t.Fatalf("error text leaked redirect query: %v", err)
	}
}

// TestRegistrationPostFollowsZeroRedirects covers the other
// credential-bearing POST.
func TestRegistrationPostFollowsZeroRedirects(t *testing.T) {
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://evil.example/")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer as.Close()

	c := NewClient(Config{AllowLoopback: true})
	reg := NewDCRRegistrar(c)
	_, err := reg.Register(context.Background(),
		&AuthServerMetadata{RegistrationEndpoint: as.URL + "/register"},
		RegistrationRequest{RedirectURIs: []string{"http://127.0.0.1:1/callback"}})
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("err = %v, want ErrRedirect", err)
	}
}

// TestDiscoveryFollowsBoundedRedirects: metadata GETs may follow redirects
// (providers relocate documents) but every hop is re-screened.
func TestDiscoveryRedirectIsRescreened(t *testing.T) {
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			// Redirect to a public-looking but plain-http destination,
			// which the re-screen must refuse.
			http.Redirect(w, r, "http://as.example.com/metadata", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer as.Close()

	c := NewClient(Config{AllowLoopback: true})
	d := NewDiscoverer(c)
	d.AllowDefaultEndpoints = false
	_, err := d.DiscoverFromIssuer(context.Background(), as.URL)
	if err == nil {
		t.Fatal("redirect to an unscreened destination must fail")
	}
	if !errors.Is(err, ErrInsecureTransport) && !errors.Is(err, ErrDiscovery) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBoundedResponseBody(t *testing.T) {
	big := strings.Repeat("x", maxBodyBytes+10)
	if _, err := readBounded(strings.NewReader(big)); err == nil {
		t.Fatal("oversized body must be rejected")
	}
	if _, err := readBounded(strings.NewReader("{}")); err != nil {
		t.Fatalf("small body: %v", err)
	}
}

func TestRedactLocation(t *testing.T) {
	got := redactLocation("https://evil.example/collect?code=stolen&state=s")
	if strings.Contains(got, "stolen") {
		t.Fatalf("redactLocation leaked the query: %q", got)
	}
	if got != "https://evil.example/collect" {
		t.Fatalf("got %q", got)
	}
	if redactLocation("") != "(no Location)" {
		t.Fatal("empty location")
	}
	if redactLocation("::::") != "(unparsable Location)" {
		t.Fatalf("got %q", redactLocation("::::"))
	}
}
