package ctlapi

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/oauthflow"
)

// nrRefreshToken is the credential the fake store holds. No response may
// contain it: `auth status` reports WHETHER a refresh token exists, never
// which one.
const nrRefreshToken = "S3NT1NEL-refresh-token-8b03"

func nrAuthStore() *nrOAuth {
	now := time.Now().Unix()
	return &nrOAuth{
		states: map[string]*oauthflow.State{
			"github": {
				Issuer: "https://auth.example", Scope: "repo", TokenEndpoint: "https://auth.example/token",
				RefreshToken: nrRefreshToken, ExpiresAt: now + 3600, IssuedAt: now,
				RegistrarKind: "dcr",
			},
			"expired": {Issuer: "https://auth.example", ExpiresAt: now - 10, IssuedAt: now - 3600},
			"halfway": {Issuer: "https://auth.example"}, // state without a token
		},
		tokens: map[string]string{"github": "access-token", "expired": "access-token"},
	}
}

func TestAuthStatus(t *testing.T) {
	store := nrAuthStore()
	env := nrStart(t, func(d *NonRegistryDeps) { d.OAuth = store })
	seedServer(t, env.reg, "github", true)
	seedServer(t, env.reg, "expired", true)
	seedServer(t, env.reg, "halfway", true)
	seedServer(t, env.reg, "never", true)

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/auth", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out []AuthStatusWire
	nrData(t, body, &out)

	got := map[string]AuthStatusWire{}
	for _, r := range out {
		got[r.Server] = r
	}
	if got["github"].State != "authorized" || !got["github"].HasRefreshToken {
		t.Errorf("github = %+v", got["github"])
	}
	if got["github"].Issuer != "https://auth.example" || got["github"].ExpiresIn <= 0 {
		t.Errorf("github = %+v", got["github"])
	}
	if got["expired"].State != "expired" {
		t.Errorf("expired = %+v", got["expired"])
	}
	// State without an access token is "registered but unusable", not
	// "authorized" — and without a ?server= filter it is omitted as noise.
	if _, ok := got["halfway"]; ok {
		t.Errorf("a credential-less server appeared in the unfiltered listing: %+v", got["halfway"])
	}
	if _, ok := got["never"]; ok {
		t.Errorf("a server that never had credentials appeared: %+v", got["never"])
	}

	// The refresh token itself never travels.
	if nrContains(body, nrRefreshToken) {
		t.Fatalf("auth status leaked the refresh token: %s", body)
	}
}

// TestAuthStatusFilteredAlwaysAnswers: an explicit question about one server
// gets an explicit row, even when the answer is "none".
func TestAuthStatusFilteredAlwaysAnswers(t *testing.T) {
	env := nrStart(t, func(d *NonRegistryDeps) { d.OAuth = nrAuthStore() })

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/auth?server=halfway", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out []AuthStatusWire
	nrData(t, body, &out)
	if len(out) != 1 || out[0].Server != "halfway" || out[0].State != "none" {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Detail == "" {
		t.Errorf("a 'none' with stored registration must explain itself: %+v", out[0])
	}
}

// TestAuthStatusBrokenEntry: one unreadable entry is folded into its own row
// and does not hide the rest; the listing is a diagnostic and must still render.
func TestAuthStatusBrokenEntry(t *testing.T) {
	store := &nrOAuth{loadErr: errors.New("cannot decrypt secrets.enc")}
	env := nrStart(t, func(d *NonRegistryDeps) { d.OAuth = store })

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/auth?server=github", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out []AuthStatusWire
	nrData(t, body, &out)
	if len(out) != 1 || out[0].State != "error" || out[0].Detail == "" {
		t.Fatalf("out = %+v", out)
	}
}

func TestAuthRefresh(t *testing.T) {
	ref := &nrRefresher{state: &oauthflow.State{ExpiresAt: time.Now().Add(time.Hour).Unix()},
		token: "fresh-access-token"}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = nrAuthStore()
		d.Refresher = ref
	})

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/auth/github/refresh", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out AuthRefreshWire
	nrData(t, body, &out)
	if out.Server != "github" || out.Superseded || out.ExpiresIn <= 0 {
		t.Errorf("out = %+v", out)
	}
	if nrContains(body, "fresh-access-token") {
		t.Fatalf("the refresh response leaked the access token: %s", body)
	}
	if len(ref.calls) != 1 || ref.calls[0] != "github" {
		t.Errorf("calls = %+v", ref.calls)
	}
	if recs := nrFindAudit(env, "auth/refresh"); len(recs) != 1 || recs[0].Server != "github" {
		t.Errorf("audit = %+v", recs)
	}
}

// TestAuthRefreshSupersededIsSuccess: another writer got there first and this
// call adopted its result. Refreshing anyway would burn the token it stored.
func TestAuthRefreshSupersededIsSuccess(t *testing.T) {
	ref := &nrRefresher{
		state: &oauthflow.State{ExpiresAt: time.Now().Add(2 * time.Hour).Unix()},
		err:   oauthflow.ErrRefreshSuperseded,
	}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = nrAuthStore()
		d.Refresher = ref
	})

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/auth/github/refresh", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var out AuthRefreshWire
	nrData(t, body, &out)
	if !out.Superseded || out.ExpiresIn <= 0 {
		t.Errorf("out = %+v", out)
	}
	if recs := nrFindAudit(env, "auth/refresh"); len(recs) != 1 || recs[0].Decision != "allowed" {
		t.Errorf("a superseded refresh must audit as allowed: %+v", recs)
	}
}

func TestAuthRefreshNoStateIs404(t *testing.T) {
	ref := &nrRefresher{err: oauthflow.ErrNoState}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = nrAuthStore()
		d.Refresher = ref
	})

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/auth/never/refresh", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
	if code := nrErrCode(t, body); code != CodeNotFound {
		t.Errorf("code = %s", code)
	}
}

func TestAuthRefreshFailure(t *testing.T) {
	ref := &nrRefresher{err: errors.New("invalid_grant")}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = nrAuthStore()
		d.Refresher = ref
	})

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/auth/github/refresh", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if recs := nrFindAudit(env, "auth/refresh"); len(recs) != 1 || recs[0].Decision != "denied" {
		t.Errorf("audit = %+v", recs)
	}
}

// TestAuthRefreshUnwiredIs404: status and logout still work when no
// refresher was assembled; only the refresh route disappears.
func TestAuthRefreshUnwiredIs404(t *testing.T) {
	env := nrStart(t, func(d *NonRegistryDeps) { d.OAuth = nrAuthStore() })

	status, _ := nrDo(t, env.sock, http.MethodPost, "/v1/auth/github/refresh", nil)
	if status != http.StatusNotFound {
		t.Errorf("refresh status = %d, want 404", status)
	}
	if status, _ := nrDo(t, env.sock, http.MethodGet, "/v1/auth?server=github", nil); status != http.StatusOK {
		t.Errorf("status endpoint = %d, want 200", status)
	}
}

func TestAuthLogoutIsIdempotent(t *testing.T) {
	store := nrAuthStore()
	env := nrStart(t, func(d *NonRegistryDeps) { d.OAuth = store })

	for i := range 2 {
		status, body := nrDo(t, env.sock, http.MethodDelete, "/v1/auth/github", nil)
		if status != http.StatusOK {
			t.Fatalf("logout %d: status = %d: %s", i, status, body)
		}
		var out AuthLogoutWire
		nrData(t, body, &out)
		if out.Server != "github" {
			t.Errorf("out = %+v", out)
		}
	}
	if len(store.cleared) != 2 {
		t.Errorf("cleared = %+v", store.cleared)
	}
	if recs := nrFindAudit(env, "auth/logout"); len(recs) != 2 {
		t.Errorf("audit = %+v", recs)
	}
}

func TestAuthLogoutFailure(t *testing.T) {
	store := nrAuthStore()
	store.clearErr = errors.New("keyring is locked")
	env := nrStart(t, func(d *NonRegistryDeps) { d.OAuth = store })

	status, body := nrDo(t, env.sock, http.MethodDelete, "/v1/auth/github", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if recs := nrFindAudit(env, "auth/logout"); len(recs) != 1 || recs[0].Decision != "denied" {
		t.Errorf("audit = %+v", recs)
	}
}

func TestSecondsUntil(t *testing.T) {
	now := time.Unix(1000, 0)
	// 0 means "never expires" (docs/modules/oauth.md), not "expired".
	if got := secondsUntil(0, now); got != 0 {
		t.Errorf("never-expires = %d", got)
	}
	if got := secondsUntil(1600, now); got != 600 {
		t.Errorf("future = %d", got)
	}
	if got := secondsUntil(900, now); got != 0 {
		t.Errorf("past = %d, want clamped to 0", got)
	}
}
