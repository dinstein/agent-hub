package oauthflow

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestFlow(t *testing.T, as *fakeAS) (*Flow, *fakeVault) {
	t.Helper()
	v := newFakeVault()
	f := NewFlow(as.client(), NewStore(v))
	f.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return f, v
}

// TestLoginLoopbackEndToEnd runs the whole docs/modules/oauth.md(a) sequence
// against a real random loopback port: discovery → DCR → bind → browser →
// callback → PKCE exchange → vault.
func TestLoginLoopbackEndToEnd(t *testing.T) {
	as := newFakeAS(t)
	f, v := newTestFlow(t, as)

	res, err := f.Login(context.Background(), LoginRequest{
		ServerID:    "gh",
		Issuer:      as.issuer(),
		ResourceURL: "",
		Scopes:      []string{"read"},
		ClientName:  "agenthub",
		Mode:        ModeLoopback,
		Open:        as.browserFor(t),
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.Mode != ModeLoopback {
		t.Fatalf("mode = %s", res.Mode)
	}
	if res.AccessToken != "access-1" {
		t.Fatalf("token = %q", res.AccessToken)
	}
	if res.State.ClientID != "client-abc" || res.State.RegistrarKind != "dcr" {
		t.Fatalf("state = %+v", res.State)
	}
	// The callback port must be persisted: providers requiring an exact
	// redirect_uri need it again after a restart.
	if res.State.CallbackPort == 0 || !strings.Contains(res.State.RedirectURI, "127.0.0.1") {
		t.Fatalf("callback bookkeeping = %+v", res.State)
	}
	if res.State.ExpiresAt != f.now().Unix()+3600 {
		t.Fatalf("expires_at = %d", res.State.ExpiresAt)
	}
	// Both vault entries, in the right order.
	if got := v.writeLog(); len(got) != 2 || got[0] != "__oauth_state__" || got[1] != "__http_auth__" {
		t.Fatalf("vault write order = %v", got)
	}
	// The exchange really carried the verifier: the fake AS rejects a
	// mismatch, so reaching here proves PKCE round-tripped.
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.exchangeCount != 1 {
		t.Fatalf("exchanges = %d", as.exchangeCount)
	}
	if as.lastTokenForm.Get("code_verifier") == "" {
		t.Fatal("no code_verifier was sent")
	}
	if as.lastTokenForm.Get("client_secret") != "" {
		t.Fatal("a public client must not send a client_secret")
	}
}

// TestLoginLoopbackRegistersTheBoundPort: the redirect URI registered with
// the AS must be the port actually bound — the bind happens first — or an
// exact-match provider rejects the callback.
func TestLoginLoopbackRegistersTheBoundPort(t *testing.T) {
	as := newFakeAS(t)
	f, _ := newTestFlow(t, as)
	res, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), Mode: ModeLoopback,
		Open: as.browserFor(t), Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	as.mu.Lock()
	body := as.lastRegBody
	as.mu.Unlock()
	uris, _ := body["redirect_uris"].([]any)
	if len(uris) != 1 || uris[0] != res.State.RedirectURI {
		t.Fatalf("registered %v but used %q", uris, res.State.RedirectURI)
	}
}

// TestLoginManualEndToEnd runs docs/modules/oauth.md(b): the URL is printed, the
// user pastes the callback back, state is validated locally.
func TestLoginManualEndToEnd(t *testing.T) {
	as := newFakeAS(t)
	f, _ := newTestFlow(t, as)

	var seen ManualInstructions
	res, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh",
		Issuer:   as.issuer(),
		Scopes:   []string{"read"},
		Mode:     ModeManual,
		Paste: func(_ context.Context, instr ManualInstructions) (string, error) {
			seen = instr
			// Play the user: complete the authorization elsewhere, then
			// paste the (unreachable) callback URL out of the address bar.
			return userCompletesAuthorization(t, as, instr), nil
		},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.Mode != ModeManual || res.AccessToken != "access-1" {
		t.Fatalf("result = %+v", res)
	}
	if seen.AuthorizationURL == "" || seen.State == "" {
		t.Fatalf("instructions = %+v", seen)
	}
	if !strings.Contains(seen.RedirectURI, "127.0.0.1") {
		t.Fatalf("manual redirect uri = %q; it must point at the USER's loopback", seen.RedirectURI)
	}
}

// TestLoginManualRejectsForeignCallback: pasting a callback from a
// different authorization request must be refused, with nothing persisted.
func TestLoginManualRejectsForeignCallback(t *testing.T) {
	as := newFakeAS(t)
	f, v := newTestFlow(t, as)

	_, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), Mode: ModeManual,
		Paste: func(_ context.Context, instr ManualInstructions) (string, error) {
			pasted := userCompletesAuthorization(t, as, instr)
			// Swap in somebody else's state.
			return strings.Replace(pasted, "state="+instr.State, "state=someone-elses", 1), nil
		},
	})
	if !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("err = %v, want ErrStateMismatch", err)
	}
	if len(v.writeLog()) != 0 {
		t.Fatalf("a rejected callback wrote to the vault: %v", v.writeLog())
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.exchangeCount != 0 {
		t.Fatal("a state mismatch must be caught before the token exchange")
	}
}

// TestLoginDeviceEndToEnd runs docs/modules/oauth.md(c) including a slow_down.
func TestLoginDeviceEndToEnd(t *testing.T) {
	as := newFakeAS(t)
	as.deviceEndpoint = true
	as.deviceInterval = 2
	as.deviceSlowDown = 1
	as.devicePending = 2

	f, v := newTestFlow(t, as)
	sl := newSleeper()
	f.Sleep = sl.sleep

	var shown DeviceAuthorization
	res, err := f.Login(context.Background(), LoginRequest{
		ServerID:     "gh",
		Issuer:       as.issuer(),
		Scopes:       []string{"read"},
		Mode:         ModeAuto, // device must win automatically
		Open:         as.browserFor(t),
		OnDeviceCode: func(da DeviceAuthorization) { shown = da },
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.Mode != ModeDevice {
		t.Fatalf("mode = %s; device support must outrank loopback in auto mode", res.Mode)
	}
	if shown.UserCode != "WDJB-MJHT" || shown.VerificationURI == "" {
		t.Fatalf("the user code was never displayed: %+v", shown)
	}
	if res.AccessToken != "access-device" {
		t.Fatalf("token = %q", res.AccessToken)
	}
	want := []time.Duration{7 * time.Second, 7 * time.Second, 7 * time.Second}
	if got := sl.intervals(); len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("poll intervals = %v, want the slow_down ladder %v", got, want)
	}
	if got := v.writeLog(); len(got) != 2 || got[0] != "__oauth_state__" {
		t.Fatalf("vault write order = %v", got)
	}
	// The device registration must not have invented a redirect URI.
	as.mu.Lock()
	defer as.mu.Unlock()
	if uris, ok := as.lastRegBody["redirect_uris"]; ok && uris != nil {
		t.Fatalf("device registration sent redirect_uris = %v", uris)
	}
}

// TestSelectMode pins the docs/modules/oauth.md selection rule.
func TestSelectMode(t *testing.T) {
	plain := &AuthServerMetadata{}
	device := &AuthServerMetadata{DeviceAuthorizationEndpoint: "https://as/device"}
	cases := []struct {
		name     string
		explicit Mode
		md       *AuthServerMetadata
		browser  bool
		want     Mode
	}{
		{"explicit flag wins over device support", ModeManual, device, true, ModeManual},
		{"explicit loopback wins", ModeLoopback, device, false, ModeLoopback},
		{"auto: device support wins", ModeAuto, device, true, ModeDevice},
		{"auto: browser available", ModeAuto, plain, true, ModeLoopback},
		{"auto: no browser downgrades to manual", ModeAuto, plain, false, ModeManual},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SelectMode(tc.explicit, tc.md, tc.browser); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

// TestLoginAutoDowngradesToManualWhenBrowserFails: a host with no DISPLAY
// only finds out at open time (docs/modules/oauth.md).
func TestLoginAutoDowngradesToManualWhenBrowserFails(t *testing.T) {
	as := newFakeAS(t)
	f, _ := newTestFlow(t, as)
	pasted := false
	res, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), Mode: ModeAuto,
		Open: func(string) error { return errors.New("exec: xdg-open: not found") },
		Paste: func(_ context.Context, instr ManualInstructions) (string, error) {
			pasted = true
			return userCompletesAuthorization(t, as, instr), nil
		},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !pasted || res.Mode != ModeManual {
		t.Fatalf("expected a manual downgrade, got mode %s (pasted=%v)", res.Mode, pasted)
	}
}

// TestLoginDoesNotDowngradeAfterUserSawTheBrowser: only a browser-LAUNCH
// failure justifies the manual downgrade. A timeout or a denial means the
// user already interacted, and re-prompting would just ask them again.
func TestLoginDoesNotDowngradeAfterUserSawTheBrowser(t *testing.T) {
	as := newFakeAS(t)
	f, _ := newTestFlow(t, as)
	_, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), Mode: ModeAuto,
		Timeout: 60 * time.Millisecond,
		Open:    func(string) error { return nil }, // opens, then nothing happens
		Paste: func(context.Context, ManualInstructions) (string, error) {
			t.Fatal("must not fall back to manual after the browser opened")
			return "", nil
		},
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

// TestLoginReusesExistingRegistration: re-registering on every login spams
// the provider's client list and trips DCR rate limits.
func TestLoginReusesExistingRegistration(t *testing.T) {
	as := newFakeAS(t)
	f, _ := newTestFlow(t, as)
	req := LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), Mode: ModeLoopback,
		Open: as.browserFor(t), Timeout: 10 * time.Second,
	}
	if _, err := f.Login(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Login(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.registerCount != 1 {
		t.Fatalf("registered %d times, want 1", as.registerCount)
	}
}

func TestLoginFromResourceURLBindsTheResource(t *testing.T) {
	as := newFakeAS(t)
	f, _ := newTestFlow(t, as)
	res, err := f.Login(context.Background(), LoginRequest{
		ServerID:    "gh",
		ResourceURL: as.srv.URL + "/mcp",
		Mode:        ModeLoopback,
		Open:        as.browserFor(t),
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.State.Resource != as.srv.URL+"/mcp" {
		t.Fatalf("resource = %q", res.State.Resource)
	}
	// The fake AS rejects an exchange whose resource does not match the
	// authorization request, so reaching here proves both legs carried it.
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.lastTokenForm.Get("resource") != as.srv.URL+"/mcp" {
		t.Fatalf("token request resource = %q", as.lastTokenForm.Get("resource"))
	}
}

// TestLoginRefusesPlainOnlyAuthorizationServer: agenthub has no "plain"
// code path, so an AS advertising only that is refused rather than
// accommodated.
func TestLoginRefusesPlainOnlyAuthorizationServer(t *testing.T) {
	as := newFakeAS(t)
	as.plainOnlyPKCE = true
	f, v := newTestFlow(t, as)
	_, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), Mode: ModeLoopback,
		Open: as.browserFor(t), Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("a plain-only authorization server must be refused")
	}
	var fe *FlowError
	if !errors.As(err, &fe) || fe.ServerID != "gh" || fe.Suggestion == "" {
		t.Fatalf("classification: %v", err)
	}
	if len(v.writeLog()) != 0 {
		t.Fatal("nothing may be persisted for a refused authorization server")
	}
}

func TestLoginNeedsAnIssuerOrResource(t *testing.T) {
	f := NewFlow(NewClient(Config{}), NewStore(newFakeVault()))
	if _, err := f.Login(context.Background(), LoginRequest{ServerID: "gh"}); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("err = %v, want ErrDiscovery", err)
	}
}

func TestLoginRequiresServerID(t *testing.T) {
	f := NewFlow(NewClient(Config{}), NewStore(newFakeVault()))
	if _, err := f.Login(context.Background(), LoginRequest{Issuer: "https://as"}); err == nil {
		t.Fatal("login without a server id must fail")
	}
}

func TestLoginModeRequirements(t *testing.T) {
	as := newFakeAS(t)
	f, _ := newTestFlow(t, as)
	if _, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), Mode: ModeManual,
	}); err == nil {
		t.Fatal("manual mode without a paste reader must fail")
	}
	as.deviceEndpoint = true
	if _, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), Mode: ModeDevice,
	}); err == nil {
		t.Fatal("device mode without a display callback must fail")
	}
}

// userCompletesAuthorization plays the user on another device: it visits
// the authorization URL, consents, and returns the callback URL the browser
// ends up on (which the headless host cannot reach — that is the point).
func userCompletesAuthorization(t *testing.T, as *fakeAS, instr ManualInstructions) string {
	t.Helper()
	u := mustQuery(t, instr.AuthorizationURL)
	code := as.issueCode(issuedCode{
		challenge:   u.Get("code_challenge"),
		method:      u.Get("code_challenge_method"),
		redirectURI: u.Get("redirect_uri"),
		resource:    u.Get("resource"),
		clientID:    u.Get("client_id"),
	})
	return instr.RedirectURI + "?code=" + code + "&state=" + instr.State
}

// --- pinned authorization endpoint (off-spec escape hatch) --------------

// A provider may serve a real authorization endpoint it never advertises,
// which RFC 8414 makes unreachable (that endpoint has exactly one legal
// source: the metadata document). The pin replaces it, and the flow must
// send the browser THERE, not to the discovered endpoint.
func TestLoginPinnedAuthorizationEndpointOverridesDiscovery(t *testing.T) {
	as := newFakeAS(t)
	f, _ := newTestFlow(t, as)

	// Same fake AS, a different path: reachable, but not what discovery says.
	pinned := as.issuer() + "/oauth/authorize/tenant1"

	var sentTo string
	res, err := f.Login(context.Background(), LoginRequest{
		ServerID:              "pha",
		Issuer:                as.issuer(),
		AuthorizationEndpoint: pinned,
		ClientName:            "agenthub",
		Mode:                  ModeLoopback,
		Open: func(u string) error {
			sentTo = u
			return as.browserFor(t)(u)
		},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("login with a pinned authorization endpoint: %v", err)
	}
	if !strings.HasPrefix(sentTo, pinned+"?") {
		t.Fatalf("browser was sent to %q, want the pinned endpoint %q", sentTo, pinned)
	}
	if res.Discovery == nil || res.Discovery.Status != DiscoveryPinnedAuthz {
		t.Fatalf("discovery status = %+v, want %s (a pinned endpoint must stay diagnosable)",
			res.Discovery, DiscoveryPinnedAuthz)
	}
}

// The pin decides where a user's authorization code is sent, so it is
// screened exactly like a discovered URL. FAIL-CLOSED: a blocked or
// unparsable endpoint aborts the login and never silently falls back to
// the discovered one.
func TestLoginPinnedAuthorizationEndpointIsScreened(t *testing.T) {
	for _, tc := range []struct{ name, endpoint string }{
		{"plaintext http", "http://evil.example.com/authorize"},
		{"not a url", "://nope"},
		{"unsupported scheme", "file:///etc/passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			as := newFakeAS(t)
			f, _ := newTestFlow(t, as)
			_, err := f.Login(context.Background(), LoginRequest{
				ServerID:              "pha",
				Issuer:                as.issuer(),
				AuthorizationEndpoint: tc.endpoint,
				ClientName:            "agenthub",
				Mode:                  ModeLoopback,
				Open: func(string) error {
					t.Fatal("browser opened: a rejected endpoint must never reach the user")
					return nil
				},
				Timeout: 10 * time.Second,
			})
			if err == nil {
				t.Fatalf("endpoint %q was accepted, want a refusal", tc.endpoint)
			}
		})
	}
}

// scopeOf returns the `scope` query parameter of an authorization URL.
func scopeOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	return u.Query().Get("scope")
}

// TestLoginScopeFromProtectedResourceMetadata proves the discovered scope
// set actually reaches the authorization request. Before this, both
// scopes_supported fields were parsed and had zero consumers: a provider
// that mints an empty-privilege token when asked for no scope would let
// login succeed and then 403 every call, with the answer sitting unread in
// its own metadata.
func TestLoginScopeFromProtectedResourceMetadata(t *testing.T) {
	as := newFakeAS(t)
	as.prmScopes = []string{"read", "profile"}
	f, _ := newTestFlow(t, as)

	var sentTo string
	if _, err := f.Login(context.Background(), LoginRequest{
		ServerID:    "srv",
		ResourceURL: as.srv.URL + "/mcp",
		ClientName:  "agenthub",
		Mode:        ModeLoopback,
		Open: func(u string) error {
			sentTo = u
			return as.browserFor(t)(u)
		},
		Timeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := scopeOf(t, sentTo); got != "read profile" {
		t.Fatalf("scope = %q, want the protected-resource scopes_supported", got)
	}
}

// TestLoginExplicitScopesOverrideDiscovery pins the operator's authority.
//
// --scopes is frequently used to NARROW a provider's default — a read-only
// token against a server whose metadata also advertises write. Unioning the
// discovered set back in would widen exactly the grant the operator sat down
// to restrict, so an explicit set is sent verbatim and alone.
func TestLoginExplicitScopesOverrideDiscovery(t *testing.T) {
	as := newFakeAS(t)
	as.prmScopes = []string{"read", "write", "admin"}
	f, _ := newTestFlow(t, as)

	var sentTo string
	if _, err := f.Login(context.Background(), LoginRequest{
		ServerID:    "srv",
		ResourceURL: as.srv.URL + "/mcp",
		Scopes:      []string{"read"},
		ClientName:  "agenthub",
		Mode:        ModeLoopback,
		Open: func(u string) error {
			sentTo = u
			return as.browserFor(t)(u)
		},
		Timeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := scopeOf(t, sentTo); got != "read" {
		t.Fatalf("scope = %q; the operator's narrower set must not be widened", got)
	}
}

// TestLoginNoScopeWhenNothingDiscovered pins the fail-closed direction: a
// resource server publishing no protected-resource document gets no scope
// parameter, NOT the authorization server's full scopes_supported.
func TestLoginNoScopeWhenNothingDiscovered(t *testing.T) {
	as := newFakeAS(t)
	as.noProtectedResource = true
	f, _ := newTestFlow(t, as)

	var sentTo string
	if _, err := f.Login(context.Background(), LoginRequest{
		ServerID:    "srv",
		ResourceURL: as.srv.URL + "/mcp",
		ClientName:  "agenthub",
		Mode:        ModeLoopback,
		Open: func(u string) error {
			sentTo = u
			return as.browserFor(t)(u)
		},
		Timeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := scopeOf(t, sentTo); got != "" {
		t.Fatalf("scope = %q, want none: the AS scopes_supported must not be a fallback", got)
	}
}
