package ctlapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/tier"
)

// nrTokenValue is the plaintext the fake store mints. The whole point of the
// tests below is that it appears in exactly one response and never again.
const nrTokenValue = "agh_S3NT1NEL-token-value-7f21"

func TestTokenCreateShowsValueExactlyOnce(t *testing.T) {
	store := &nrTokens{value: nrTokenValue}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Tokens = store })

	status, created := nrDo(t, env.sock, http.MethodPost, "/v1/tokens",
		TokenCreateRequest{Name: "ci", Tier: string(tier.Write), Servers: []string{"github"}})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", status, created)
	}
	var out TokenCreatedWire
	nrData(t, created, &out)
	if out.Value != nrTokenValue {
		t.Fatalf("the creating response must carry the plaintext, got %q", out.Value)
	}
	if out.Token.Name != "ci" || out.Token.Tier != string(tier.Write) {
		t.Errorf("token = %+v", out.Token)
	}

	// Every LATER read must be free of it — prefix and metadata only.
	_, list := nrDo(t, env.sock, http.MethodGet, "/v1/tokens", nil)
	if nrContains(list, nrTokenValue) {
		t.Fatalf("the listing leaked the plaintext: %s", list)
	}
	status, revoked := nrDo(t, env.sock, http.MethodDelete, "/v1/tokens/ci", nil)
	if status != http.StatusOK {
		t.Fatalf("revoke status = %d: %s", status, revoked)
	}
	if nrContains(revoked, nrTokenValue) {
		t.Fatalf("the revoke response leaked the plaintext: %s", revoked)
	}

	// And the listing row has no field that could hold one.
	var rows []map[string]any
	nrData(t, list, &rows)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, forbidden := range []string{"value", "hash", "secret"} {
		if _, ok := rows[0][forbidden]; ok {
			t.Errorf("listing row carries %q: %+v", forbidden, rows[0])
		}
	}
}

func TestTokensList(t *testing.T) {
	now := time.Now()
	exp := now.Add(time.Hour)
	store := &nrTokens{value: nrTokenValue, toks: []httpbridge.Token{
		{Name: "ci", Prefix: "agh_S3NT1NEL", Tier: tier.Read, Hash: "deadbeef",
			CreatedAt: now, ExpiresAt: exp},
		{Name: "old", Prefix: "agh_old12345", Tier: tier.Read, Hash: "cafe",
			CreatedAt: now, RevokedAt: now},
	}}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Tokens = store })

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/tokens", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out []TokenWire
	nrData(t, body, &out)
	if len(out) != 2 {
		t.Fatalf("rows = %+v", out)
	}
	if out[0].State != "active" || out[0].ExpiresAt == nil {
		t.Errorf("row 0 = %+v", out[0])
	}
	if out[1].State != "revoked" || out[1].RevokedAt == nil {
		t.Errorf("row 1 = %+v", out[1])
	}
	// The stored HMAC must not cross the boundary either.
	if nrContains(body, "deadbeef") || nrContains(body, "cafe") {
		t.Errorf("the listing leaked the stored HMAC: %s", body)
	}
}

func TestTokenCreateRequiresName(t *testing.T) {
	store := &nrTokens{value: nrTokenValue}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Tokens = store })

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/tokens", TokenCreateRequest{Name: "  "})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if len(store.toks) != 0 {
		t.Errorf("a nameless request minted a token")
	}
}

// TestTokenCreateDefaultsToRead pins the closed end of the tier ladder: a
// request that forgot to state a tier must not come out able to delete.
func TestTokenCreateDefaultsToRead(t *testing.T) {
	store := &nrTokens{value: nrTokenValue}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Tokens = store })

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/tokens", TokenCreateRequest{Name: "ci"})
	if status != http.StatusCreated {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out TokenCreatedWire
	nrData(t, body, &out)
	if out.Token.Tier != string(tier.Read) {
		t.Errorf("tier = %q, want %q", out.Token.Tier, tier.Read)
	}
}

// TestTokenCreateAllowlistNilVsEmpty: nil means "every server", an explicit
// empty list means "nothing". Collapsing them would silently widen a token.
func TestTokenCreateAllowlistNilVsEmpty(t *testing.T) {
	store := &nrTokens{value: nrTokenValue}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Tokens = store })

	if _, body := nrDo(t, env.sock, http.MethodPost, "/v1/tokens",
		json.RawMessage(`{"name":"all"}`)); body == nil {
		t.Fatal("no body")
	}
	if store.toks[0].Servers != nil {
		t.Errorf("omitted allowlist became %+v, want nil (every server)", store.toks[0].Servers)
	}
	if _, body := nrDo(t, env.sock, http.MethodPost, "/v1/tokens",
		json.RawMessage(`{"name":"none","servers":[]}`)); body == nil {
		t.Fatal("no body")
	}
	if s := store.toks[1].Servers; s == nil || len(s) != 0 {
		t.Errorf("explicit empty allowlist became %+v, want a non-nil empty slice", s)
	}
}

func TestTokenCreateExpiry(t *testing.T) {
	store := &nrTokens{value: nrTokenValue}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Tokens = store })

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/tokens",
		TokenCreateRequest{Name: "ci", ExpiresInSeconds: 3600})
	if status != http.StatusCreated {
		t.Fatalf("status = %d: %s", status, body)
	}
	if store.toks[0].ExpiresAt.IsZero() {
		t.Errorf("expires_in_seconds was dropped")
	}
}

func TestTokenErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{httpbridge.ErrTokenExists, http.StatusConflict, CodeConflict},
		{httpbridge.ErrTooManyTokens, http.StatusConflict, CodeConflict},
		{httpbridge.ErrInvalidTier, http.StatusBadRequest, CodeBadRequest},
		{httpbridge.ErrInvalidName, http.StatusBadRequest, CodeBadRequest},
	}
	for _, c := range cases {
		store := &nrTokens{value: nrTokenValue, createErr: c.err}
		env := nrStart(t, func(d *NonRegistryDeps) { d.Tokens = store })
		status, body := nrDo(t, env.sock, http.MethodPost, "/v1/tokens", TokenCreateRequest{Name: "ci"})
		if status != c.status {
			t.Errorf("%v: status = %d, want %d: %s", c.err, status, c.status, body)
		}
		if code := nrErrCode(t, body); code != c.code {
			t.Errorf("%v: code = %s, want %s", c.err, code, c.code)
		}
	}
}

func TestTokenRevokeUnknownIs404(t *testing.T) {
	store := &nrTokens{value: nrTokenValue}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Tokens = store })

	status, body := nrDo(t, env.sock, http.MethodDelete, "/v1/tokens/nope", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
	if code := nrErrCode(t, body); code != CodeNotFound {
		t.Errorf("code = %s", code)
	}
}

// TestTokenRevokeAlreadyRevokedIs409: the row exists and is still listed, so
// answering 404 would send the operator hunting for a typo.
func TestTokenRevokeAlreadyRevokedIs409(t *testing.T) {
	store := &nrTokens{value: nrTokenValue, revokeErr: httpbridge.ErrAlreadyRevoked}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Tokens = store })

	status, body := nrDo(t, env.sock, http.MethodDelete, "/v1/tokens/ci", nil)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", status, body)
	}
	if code := nrErrCode(t, body); code != CodeConflict {
		t.Errorf("code = %s", code)
	}
}
