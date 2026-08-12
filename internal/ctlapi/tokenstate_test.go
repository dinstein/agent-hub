package ctlapi

import (
	"testing"

	"github.com/dinstein/agent-hub/api"
)

// TestTokenStateReachesHealthWithNoGatewayAttached is the whole point of a
// second source. The gateway aggregator reports ok=false when nobody is
// connected, which is the steady state of a daemon with no client running —
// and a credential that died while nobody was looking is exactly the case an
// operator needs told.
func TestTokenStateReachesHealthWithNoGatewayAttached(t *testing.T) {
	tokens := NewTokenStates()
	client, env := startServer(t, func(o *Options) { o.TokenStates = tokens })
	seedServer(t, env.reg, "notion", true)

	// Nothing published yet: unchanged from before this existed.
	before := serversByID(t, client)
	if got := before["notion"].Health; got.Summary != "not observed" {
		t.Fatalf("with no facts published, health = %+v, want the old answer", got)
	}

	cases := []struct {
		name    string
		facts   TokenFacts
		level   string
		summary string
		action  string
	}{
		{"expiring with a refresh token repairs itself", TokenFacts{State: TokenExpiring, HasRefreshToken: true},
			api.HealthLevelDegraded, "token expiring soon", api.ActionRefresh},
		{"expired with a refresh token still repairs itself", TokenFacts{State: TokenExpired, HasRefreshToken: true},
			api.HealthLevelUnhealthy, "token expired", api.ActionRefresh},
		{"expired with nothing stored needs a human", TokenFacts{State: TokenExpired},
			api.HealthLevelUnhealthy, "token expired", api.ActionLogin},
		{"a refused grant needs a human whatever is stored",
			TokenFacts{State: TokenRevoked, HasRefreshToken: true},
			api.HealthLevelUnhealthy, "authorization revoked", api.ActionLogin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokens.Replace(map[string]TokenFacts{"notion": tc.facts})
			got := serversByID(t, client)["notion"].Health
			if got.Level != tc.level || got.Summary != tc.summary || got.Action != tc.action {
				t.Fatalf("health = %+v, want %s / %q / %s", got, tc.level, tc.summary, tc.action)
			}
		})
	}

	// A snapshot is the whole truth: a server logged out of, deleted or
	// disabled since the last scan stops being reported rather than freezing
	// on its last known state.
	tokens.Replace(map[string]TokenFacts{})
	if got := serversByID(t, client)["notion"].Health; got.Summary != "not observed" {
		t.Fatalf("after the server left the snapshot, health = %+v, want the unknown answer", got)
	}
}

func TestTokenStatesIsEmptyUntilPublished(t *testing.T) {
	var tokens TokenStates
	if _, ok := tokens.TokenState("anything"); ok {
		t.Fatal("an unpublished holder must report nothing known, not a zero-valued fact")
	}
	tokens.Replace(map[string]TokenFacts{"gh": {State: TokenRevoked}})
	f, ok := tokens.TokenState("gh")
	if !ok || f.State != TokenRevoked || f.HasRefreshToken {
		t.Fatalf("TokenState = %+v, %t", f, ok)
	}
	if _, ok := tokens.TokenState("elk"); ok {
		t.Fatal("a server outside the snapshot must report nothing known")
	}
}

// TestRevokedOutranksEveryOtherTokenState: the fold takes the worst, and a
// refusal must not be averaged away by an instance that saw the credential
// while it still worked.
func TestRevokedOutranksEveryOtherTokenState(t *testing.T) {
	for _, lesser := range []TokenState{TokenOK, TokenExpiring, TokenExpired} {
		if tokenSeverity(TokenRevoked) <= tokenSeverity(lesser) {
			t.Errorf("revoked does not outrank %q", lesser)
		}
	}
}
