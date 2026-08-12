// Package fakeas is a scriptable OAuth authorization server with the MCP
// resource server it protects, on one loopback origin.
//
// It exists because there were two of these. internal/oauthflow has an
// in-package fixture for the protocol itself, and test/e2e grew a second,
// weaker one for the CLI — so the suite that tests what a USER meets could
// not reach the failures the protocol tests could reach, and every knob added
// to one was absent from the other. This is the copy the out-of-package tests
// share; the in-package one stays where it is, because it reaches unexported
// types this package cannot.
//
// What it answers, in the order a login needs them: the RFC 9728
// protected-resource pointer, RFC 8414 authorization-server metadata, RFC
// 7591 dynamic client registration, the RFC 8628 device grant, the token
// endpoint (device and refresh grants), and the MCP endpoint itself.
//
// # Why the counters
//
// Several behaviours worth testing are absences: a refusal that is not asked
// about again, a credential with no expiry that is never renewed, a
// concurrent pair that spends one refresh token rather than two. None of them
// leaves a distinguishable log line — "backs off" and "stops" produce the
// same one — so every knob here has a counter beside it, and the assertions
// are about how many times this server was asked.
package fakeas

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Grant types this server accepts. Anything else is unsupported_grant_type:
// a token handed out for the wrong grant would let a test pass against a
// flow that never ran the one it claims to.
const (
	grantDevice  = "urn:ietf:params:oauth:grant-type:device_code"
	grantRefresh = "refresh_token"
)

// Options configures a Server. The zero value is a conformant provider that
// issues hour-long tokens and 401s a stale one.
type Options struct {
	// MCP handles requests to /mcp once the bearer has been accepted. nil
	// answers 200 with an empty body, which is enough for tests that only
	// care about the credential.
	MCP http.Handler
	// AccessTTL is the advertised expires_in, in seconds. Zero takes the
	// hour-long default; to omit expires_in, set NoExpiry.
	AccessTTL int
	// NoExpiry omits expires_in from every token response. It is a separate
	// field rather than AccessTTL: 0 because "no expires_in" and
	// "expires_in: 0" are different statements — the first means never
	// expires and no proactive refresh may touch it — and a zero value that
	// meant the rarer of the two would be read wrongly every time.
	NoExpiry bool
	// StaleIs200 answers a rotated-away bearer with 200 and an error RESULT
	// instead of 401. Real providers do this, and it is the shape that makes
	// the passive refresh path unreachable: nothing rejects the credential,
	// so nothing triggers a renewal. A MISSING Authorization header is 401
	// regardless — otherwise no first login would be possible.
	StaleIs200 bool
	// NoDeviceEndpoint omits device_authorization_endpoint from the
	// metadata, which is how a provider says "browser or nothing".
	NoDeviceEndpoint bool
}

// Server is a running fake provider. Every field is guarded: the knobs are
// meant to be turned while a hub is talking to it, which is the whole point
// of a fixture for renewal.
type Server struct {
	srv  *httptest.Server
	base string
	opts Options

	mu sync.Mutex
	// granted is the access token this provider currently accepts. A refresh
	// ROTATES it, which is what makes "the call succeeded" and "the renewed
	// credential reached the downstream" the same observation.
	granted     string
	accessTTL   int
	refuseGrant bool
	refuseClnt  bool
	counts      Counts
}

// Counts is everything this server observed. Compare it before and after,
// rather than reading a log: the interesting properties are all absences.
type Counts struct {
	// Registrations is dynamic client registrations performed.
	Registrations int
	// DeviceGrants is device-code grants redeemed.
	DeviceGrants int
	// Refreshes is refresh grants redeemed SUCCESSFULLY.
	Refreshes int
	// TokenRequests is every request the token endpoint saw, refusals
	// included. This is the counter that separates "stopped asking" from
	// "asked and was told no".
	TokenRequests int
	// Challenges is MCP requests answered 401.
	Challenges int
	// LastGrant is the grant_type of the last token request.
	LastGrant string
	// Accepted is the access token currently accepted.
	Accepted string
}

// New starts a provider and registers its shutdown with t.
func New(t *testing.T, opts Options) *Server {
	t.Helper()
	ttl := opts.AccessTTL
	switch {
	case opts.NoExpiry:
		ttl = 0
	case ttl <= 0:
		ttl = 3600
	}
	s := &Server{opts: opts, granted: "granted-access-token", accessTTL: ttl}
	// Unstarted, because the documents this provider serves name its own
	// address: writing the URL into the handler after Start would be a race
	// against the requests it is already able to answer.
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.route))
	s.base = "http://" + s.srv.Listener.Addr().String()
	s.srv.Start()
	t.Cleanup(s.srv.Close)
	return s
}

// Base is the provider's origin.
func (s *Server) Base() string { return s.base }

// MCPURL is the endpoint a registry entry points at.
func (s *Server) MCPURL() string { return s.base + "/mcp" }

// PRMURL is where the 401 challenge points. Naming it explicitly, rather than
// relying on the candidate search, is what makes a failure a failure of the
// login rather than of discovery — which has its own coverage elsewhere.
func (s *Server) PRMURL() string { return s.base + "/.well-known/oauth-protected-resource/mcp" }

// Counts returns a snapshot of everything observed.
func (s *Server) Counts() Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.counts
	c.Accepted = s.granted
	return c
}

// RotateAccessToken changes the bearer this provider accepts WITHOUT telling
// anybody — the way a provider ending a session does. Whatever the hub holds
// stops working at the next request.
func (s *Server) RotateAccessToken() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.granted = fmt.Sprintf("rotated-out-from-under-the-client-%d", s.counts.TokenRequests)
}

// RefuseGrants makes every token request answer 400 invalid_grant: the shape
// a spent, rotated-away or revoked refresh token gets, and the one no retry
// survives.
func (s *Server) RefuseGrants(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseGrant = on
}

// RefuseClient makes every token request answer 401 invalid_client, which is
// what a provider garbage-collecting an unused dynamic registration looks
// like.
func (s *Server) RefuseClient(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseClnt = on
}

// SetAccessTTL changes the advertised lifetime of tokens issued from now on.
// Zero or less omits expires_in — explicit here, unlike in Options, because a
// call is a statement and a zero field is a default.
func (s *Server) SetAccessTTL(seconds int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessTTL = seconds
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.Contains(r.URL.Path, "/.well-known/oauth-protected-resource"):
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":              s.MCPURL(),
			"authorization_servers": []string{s.base},
			"scopes_supported":      []string{"mcp.read"},
		})
	case strings.Contains(r.URL.Path, "/.well-known/"):
		s.serveMetadata(w)
	case r.URL.Path == "/register":
		s.mu.Lock()
		s.counts.Registrations++
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{
			"client_id": "e2e-registered-client", "client_id_issued_at": 1,
		})
	case r.URL.Path == "/device":
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code":      "e2e-device-code",
			"user_code":        "WDJB-MJHT",
			"verification_uri": s.base + "/activate",
			"expires_in":       300,
			"interval":         1,
		})
	case r.URL.Path == "/token":
		s.serveToken(w, r)
	case r.URL.Path == "/mcp":
		s.serveMCP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveMetadata(w http.ResponseWriter) {
	doc := map[string]any{
		"issuer":                           s.base,
		"authorization_endpoint":           s.base + "/authorize",
		"token_endpoint":                   s.base + "/token",
		"registration_endpoint":            s.base + "/register",
		"code_challenge_methods_supported": []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	}
	if !s.opts.NoDeviceEndpoint {
		doc["device_authorization_endpoint"] = s.base + "/device"
	}
	writeJSON(w, http.StatusOK, doc)
}

// serveToken answers the device and refresh grants, and the two refusals.
//
// A successful refresh ROTATES the accepted access token: the previous bearer
// stops working the moment a renewal happens, so a later call that succeeds
// can only have carried the new one and no test has to read the vault.
func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	grant := r.Form.Get("grant_type")

	s.mu.Lock()
	s.counts.TokenRequests++
	s.counts.LastGrant = grant
	refuseGrant, refuseClient := s.refuseGrant, s.refuseClnt
	s.mu.Unlock()

	switch {
	case refuseClient:
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "invalid_client", "error_description": "unknown client",
		})
		return
	case refuseGrant:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid_grant", "error_description": "consent withdrawn",
		})
		return
	case grant != grantDevice && grant != grantRefresh:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}

	s.mu.Lock()
	switch grant {
	case grantRefresh:
		s.counts.Refreshes++
		s.granted = fmt.Sprintf("rotated-access-token-%d", s.counts.Refreshes)
	case grantDevice:
		s.counts.DeviceGrants++
	}
	token, ttl := s.granted, s.accessTTL
	s.mu.Unlock()

	doc := map[string]any{
		"access_token":  token,
		"token_type":    "Bearer",
		"refresh_token": "e2e-refresh-token",
		"scope":         "mcp.read",
	}
	if ttl > 0 {
		doc["expires_in"] = ttl
	}
	writeJSON(w, http.StatusOK, doc)
}

// serveMCP refuses until it is shown the token this provider currently
// accepts, and the refusal carries the RFC 9728 pointer that starts a login.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	want := "Bearer " + s.granted
	s.mu.Unlock()

	if got := r.Header.Get("Authorization"); got != want {
		// A missing header is ALWAYS 401 — that is what makes a first login
		// possible. Only a present-but-stale one takes the 200 shape.
		if s.opts.StaleIs200 && strings.TrimSpace(got) != "" {
			writeJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]any{
					"isError": true,
					"content": []any{map[string]any{"type": "text", "text": "Invalid token"}},
				},
			})
			return
		}
		s.mu.Lock()
		s.counts.Challenges++
		s.mu.Unlock()
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="agenthub", resource_metadata="`+s.PRMURL()+`"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if s.opts.MCP == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.opts.MCP.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, doc map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(doc)
}
