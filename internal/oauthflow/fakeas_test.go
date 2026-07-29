package oauthflow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// secretRef aliases secrets.Ref so the fake vault reads compactly.
type secretRef = secrets.Ref

// fakeAS is a scriptable authorization server. Every test in this package
// talks to one; nothing here reaches the network.
//
// Because httptest binds 127.0.0.1 over plain http, every Client built for
// these tests sets AllowLoopback — which is itself part of what is under
// test (see TestSSRF*).
type fakeAS struct {
	srv *httptest.Server

	mu sync.Mutex

	// --- request scripting ---------------------------------------------
	// issuerPath is the path component of the issuer ("" = none).
	issuerPath string
	// servedMetadata lists the well-known paths that answer 200. Anything
	// else 404s, which is how candidate ordering is observed.
	servedMetadata map[string]bool
	// deviceEndpoint enables device_authorization_endpoint in metadata.
	deviceEndpoint bool
	// noRegistration omits registration_endpoint from metadata.
	noRegistration bool
	// plainOnlyPKCE advertises only the "plain" challenge method.
	plainOnlyPKCE bool
	// registrationStatus, when non-zero, makes /register answer it.
	registrationStatus int
	// tokenRedirect makes /token answer 302 (zero-redirect test).
	tokenRedirect bool
	// expiresIn is the advertised token lifetime; 0 omits expires_in.
	expiresIn int64
	// expiresInString sends expires_in as a JSON string.
	expiresInString bool
	// rotateRefresh issues a new refresh token on every refresh.
	rotateRefresh bool
	// devicePending is how many authorization_pending answers to give.
	devicePending int
	// deviceSlowDown is how many slow_down answers to give (before the
	// pending ones).
	deviceSlowDown int
	// deviceDenied makes the device grant answer access_denied.
	deviceDenied bool
	// deviceInterval is the advertised poll interval in seconds.
	deviceInterval int64
	// refreshDelay stalls the token endpoint on refresh grants, widening
	// the window concurrency tests need.
	refreshDelay time.Duration
	// noProtectedResource makes every oauth-protected-resource path 404 even
	// when servedMetadata would allow it, modelling a resource server that
	// publishes no RFC 9728 document.
	noProtectedResource bool
	// authorizeSuffix, when set, is appended to the advertised
	// authorization_endpoint. It models a provider whose per-resource
	// document names a different authorize URL than its generic one.
	authorizeSuffix string
	// prmScopes overrides the protected-resource document's
	// scopes_supported. nil keeps the default; an empty non-nil slice omits
	// the member entirely.
	prmScopes []string

	// --- observations ---------------------------------------------------
	hits          []string
	registerCount int
	refreshCount  int
	exchangeCount int
	deviceCalls   int
	lastTokenForm url.Values
	lastRegBody   map[string]any

	// --- state ------------------------------------------------------------
	codes        map[string]issuedCode
	refreshToken string
	nextSerial   int
}

type issuedCode struct {
	challenge   string
	method      string
	redirectURI string
	resource    string
	clientID    string
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	f := &fakeAS{
		codes:        map[string]issuedCode{},
		expiresIn:    3600,
		refreshToken: "refresh-0",
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAS) issuer() string {
	if f.issuerPath == "" {
		return f.srv.URL
	}
	return f.srv.URL + "/" + strings.Trim(f.issuerPath, "/")
}

// client builds a Client wired to this server.
func (f *fakeAS) client() *Client {
	return NewClient(Config{AllowLoopback: true, Timeout: 5 * time.Second})
}

func (f *fakeAS) metadata() *AuthServerMetadata {
	md := &AuthServerMetadata{
		Issuer:                        f.issuer(),
		AuthorizationEndpoint:         f.srv.URL + "/authorize" + f.authorizeSuffix,
		TokenEndpoint:                 f.srv.URL + "/token",
		CodeChallengeMethodsSupported: []string{"S256"},
	}
	if !f.noRegistration {
		md.RegistrationEndpoint = f.srv.URL + "/register"
	}
	if f.deviceEndpoint {
		md.DeviceAuthorizationEndpoint = f.srv.URL + "/device"
	}
	if f.plainOnlyPKCE {
		md.CodeChallengeMethodsSupported = []string{"plain"}
	}
	return md
}

func (f *fakeAS) record(path string) {
	f.mu.Lock()
	f.hits = append(f.hits, path)
	f.mu.Unlock()
}

func (f *fakeAS) hitList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

func (f *fakeAS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.record(r.URL.Path)
	switch {
	case strings.Contains(r.URL.Path, "/.well-known/"):
		f.serveMetadata(w, r)
	case r.URL.Path == "/register":
		f.serveRegister(w, r)
	case r.URL.Path == "/device":
		f.serveDevice(w, r)
	case r.URL.Path == "/token":
		f.serveToken(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAS) serveMetadata(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	served := f.servedMetadata
	f.mu.Unlock()
	if served != nil && !served[r.URL.Path] {
		http.NotFound(w, r)
		return
	}
	if strings.Contains(r.URL.Path, wellKnownProtectedResrc) {
		f.mu.Lock()
		absent := f.noProtectedResource
		f.mu.Unlock()
		if absent {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		scopes := []string{"read"}
		if f.prmScopes != nil {
			scopes = f.prmScopes
		}
		f.mu.Unlock()
		doc := map[string]any{
			"resource":              f.srv.URL + "/mcp",
			"authorization_servers": []string{f.issuer()},
		}
		if len(scopes) > 0 {
			doc["scopes_supported"] = scopes
		}
		writeJSON(w, 200, doc)
		return
	}
	writeJSON(w, 200, f.metadata())
}

func (f *fakeAS) serveRegister(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.registerCount++
	status := f.registrationStatus
	f.mu.Unlock()
	if status != 0 {
		writeJSON(w, status, map[string]any{"error": "access_denied", "error_description": "no dcr here"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid_client_metadata"})
		return
	}
	f.mu.Lock()
	f.lastRegBody = body
	f.mu.Unlock()
	writeJSON(w, 201, map[string]any{
		"client_id":                  "client-abc",
		"client_id_issued_at":        1,
		"token_endpoint_auth_method": body["token_endpoint_auth_method"],
	})
}

func (f *fakeAS) serveDevice(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.deviceCalls++
	interval := f.deviceInterval
	f.mu.Unlock()
	resp := map[string]any{
		"device_code":      "device-code-1",
		"user_code":        "WDJB-MJHT",
		"verification_uri": f.srv.URL + "/activate",
		"expires_in":       1800,
	}
	if interval > 0 {
		resp["interval"] = interval
	}
	writeJSON(w, 200, resp)
}

func (f *fakeAS) serveToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	redirect := f.tokenRedirect
	f.mu.Unlock()
	if redirect {
		w.Header().Set("Location", "https://evil.example.com/collect?code=stolen")
		w.WriteHeader(http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid_request"})
		return
	}
	form := r.PostForm
	f.mu.Lock()
	f.lastTokenForm = form
	f.mu.Unlock()

	switch form.Get("grant_type") {
	case GrantAuthorizationCode:
		f.serveAuthCodeGrant(w, form)
	case GrantRefreshToken:
		f.serveRefreshGrant(w, form)
	case GrantDeviceCode:
		f.serveDeviceGrant(w, form)
	default:
		writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (f *fakeAS) serveAuthCodeGrant(w http.ResponseWriter, form url.Values) {
	f.mu.Lock()
	rec, ok := f.codes[form.Get("code")]
	if ok {
		delete(f.codes, form.Get("code"))
		f.exchangeCount++
	}
	f.mu.Unlock()
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "unknown code"})
		return
	}
	if got := s256(form.Get("code_verifier")); got != rec.challenge {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "pkce mismatch"})
		return
	}
	if rec.redirectURI != "" && form.Get("redirect_uri") != rec.redirectURI {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "redirect_uri mismatch"})
		return
	}
	if form.Get("resource") != rec.resource {
		writeJSON(w, 400, map[string]any{"error": "invalid_target", "error_description": "resource mismatch"})
		return
	}
	f.writeTokens(w, "access-1")
}

func (f *fakeAS) serveRefreshGrant(w http.ResponseWriter, form url.Values) {
	f.mu.Lock()
	delay := f.refreshDelay
	want := f.refreshToken
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if form.Get("refresh_token") != want {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "refresh token already used"})
		return
	}
	f.mu.Lock()
	f.refreshCount++
	n := f.refreshCount
	if f.rotateRefresh {
		f.refreshToken = fmt.Sprintf("refresh-%d", n)
	}
	f.mu.Unlock()
	f.writeTokens(w, fmt.Sprintf("access-refreshed-%d", n))
}

func (f *fakeAS) serveDeviceGrant(w http.ResponseWriter, _ url.Values) {
	f.mu.Lock()
	switch {
	case f.deviceDenied:
		f.mu.Unlock()
		writeJSON(w, 400, map[string]any{"error": "access_denied"})
		return
	case f.deviceSlowDown > 0:
		f.deviceSlowDown--
		f.mu.Unlock()
		writeJSON(w, 400, map[string]any{"error": "slow_down"})
		return
	case f.devicePending > 0:
		f.devicePending--
		f.mu.Unlock()
		writeJSON(w, 400, map[string]any{"error": "authorization_pending"})
		return
	}
	f.mu.Unlock()
	f.writeTokens(w, "access-device")
}

func (f *fakeAS) writeTokens(w http.ResponseWriter, access string) {
	f.mu.Lock()
	out := map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"refresh_token": f.refreshToken,
		"scope":         "read",
	}
	if f.expiresIn > 0 {
		if f.expiresInString {
			out["expires_in"] = fmt.Sprint(f.expiresIn)
		} else {
			out["expires_in"] = f.expiresIn
		}
	}
	f.mu.Unlock()
	writeJSON(w, 200, out)
}

// issueCode registers an authorization code as if the user had consented.
func (f *fakeAS) issueCode(rec issuedCode) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSerial++
	code := fmt.Sprintf("code-%d", f.nextSerial)
	f.codes[code] = rec
	return code
}

// browserFor returns a BrowserOpener that plays the role of the user's
// browser: it validates the authorization request, mints a code and hits
// the loopback callback exactly as a 302 would.
func (f *fakeAS) browserFor(t *testing.T) BrowserOpener {
	t.Helper()
	return func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("response_type") != "code" {
			return fmt.Errorf("response_type = %q", q.Get("response_type"))
		}
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			return fmt.Errorf("missing S256 PKCE challenge")
		}
		code := f.issueCode(issuedCode{
			challenge:   q.Get("code_challenge"),
			method:      q.Get("code_challenge_method"),
			redirectURI: q.Get("redirect_uri"),
			resource:    q.Get("resource"),
			clientID:    q.Get("client_id"),
		})
		cb, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			return err
		}
		cbq := cb.Query()
		cbq.Set("code", code)
		cbq.Set("state", q.Get("state"))
		cb.RawQuery = cbq.Encode()
		go func() {
			resp, err := http.Get(cb.String()) //nolint:noctx // test browser stand-in
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// --- fake vault -----------------------------------------------------------

// fakeVault is an in-memory secrets.Store with fault injection, used to
// observe the state-before-token write ordering.
type fakeVault struct {
	mu     sync.Mutex
	data   map[string]string
	writes []string // ordered log of written entry keys
	// failKey makes Set fail for that entry key (secrets.KeyHTTPAuth,
	// secrets.KeyOAuthState).
	failKey string
}

func newFakeVault() *fakeVault { return &fakeVault{data: map[string]string{}} }

func vaultKey(serverID, scope, key string) string { return serverID + "|" + scope + "|" + key }

func (v *fakeVault) Get(_ context.Context, ref secretRef) (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.data[vaultKey(ref.ServerID, ref.Scope, ref.Key)]
	return s, ok, nil
}

func (v *fakeVault) Set(_ context.Context, ref secretRef, val string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failKey != "" && ref.Key == v.failKey {
		return fmt.Errorf("injected write failure for %s", ref.Key)
	}
	v.writes = append(v.writes, ref.Key)
	v.data[vaultKey(ref.ServerID, ref.Scope, ref.Key)] = val
	return nil
}

func (v *fakeVault) Delete(_ context.Context, ref secretRef) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.data, vaultKey(ref.ServerID, ref.Scope, ref.Key))
	return nil
}

func (v *fakeVault) writeLog() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.writes...)
}

func (v *fakeVault) get(serverID, key string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.data[vaultKey(serverID, "_global", key)]
}

func (v *fakeVault) put(serverID, key, val string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.data[vaultKey(serverID, "_global", key)] = val
}

func (v *fakeVault) setFailKey(k string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.failKey = k
}
