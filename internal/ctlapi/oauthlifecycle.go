package ctlapi

import (
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/oauthflow"
)

// The OAuth lifecycle decision, in ONE copy.
//
// Three surfaces answer "what is stored for this server, and what should I
// run": `agenthub auth status`, which is OAuth-only and reads values;
// `agenthub server ls` / `server inspect`, which cover every credential kind
// and are index-first so they never pop a keychain dialog; and `GET /v1/auth`,
// which is the same question over HTTP. Those differences are real and their
// wire shapes stay separate because of them.
//
// What is NOT different, and was nevertheless written three times, is the
// decision in the middle: is there an access token at all, has the provider
// refused the grant, is the token past its deadline, is it inside the refresh
// grace, and can it be renewed without a human.
//
// It has drifted twice, and each time the same way. First the refresh action
// was taught to one CLI copy, so one server read `refresh` in one command and
// `login` in the other; those two were folded together in internal/cli. Then
// the refused-grant work taught the fold to both of them and left the HTTP
// copy behind, so `auth status` read `revoked` where `GET /v1/auth` read
// `authorized` — and told a frontend it could renew unattended. Living in
// internal/cli is what let the third surface not see it: ctlapi cannot import
// that package, while internal/cli already imports this one.
//
// It decides from STORED STATE ALONE and says nothing about whether the
// provider still accepts the credential — that is a live 401's answer, and the
// ban on a persisted needsAuth is the same rule seen from here.

// OAuthLifecycle is what the stored state implies, in the vocabulary every
// caller already emits: an api.AuthState* value, an api.Action* suggestion,
// the sentence explaining a state that would otherwise puzzle a reader, and
// whether an unattended renewal exists.
type OAuthLifecycle struct {
	State  string
	Action string
	Detail string
	// HasRefreshToken is "stored AND usable", never "stored". A refused grant
	// leaves the bytes in place, so the two differ exactly where it matters —
	// this is the field a frontend reads to decide whether to offer a renewal
	// that needs nobody present, and offering one for a grant the provider has
	// already refused is offering the repair that cannot work.
	//
	// It is decided HERE rather than beside each caller's other fields because
	// that is what it was, and one of the three producers was left spelling it
	// `st.RefreshToken != ""`. A rule three call sites apply by hand is a rule
	// one of them eventually does not.
	HasRefreshToken bool
}

// OAuthLifecycleOf decides one server's OAuth lifecycle. hasAccessToken is
// passed in rather than read here because the callers learn it differently —
// one loads the token, one asks its index whether the key exists — and that
// difference is the whole cost distinction between them.
func OAuthLifecycleOf(st *oauthflow.State, hasAccessToken bool, now time.Time) OAuthLifecycle {
	// One formula, evaluated once, for every branch below — including the two
	// that return early. Stored AND usable: a refused grant answers false here
	// however many bytes are still in the vault.
	usable := st.RefreshToken != "" && !st.GrantRevoked()
	if st.GrantRevoked() {
		// Outranks every rung below, the missing-token one included: whatever
		// else this record lacks, the provider has already answered the
		// question all of them lead to. The action is FIXED at login — a
		// refused grant makes `auth refresh` a command that can only fail,
		// whatever bytes are still stored.
		return OAuthLifecycle{
			State:           api.AuthStateRevoked,
			Action:          api.ActionLogin,
			Detail:          revokedDetail(st),
			HasRefreshToken: usable, // false, by the formula and by the contract
		}
	}
	if !hasAccessToken {
		// State without a token is the DCR-credentials-only shape: a login was
		// started (or the token write failed) and nothing usable came of it.
		// The action stays login even when a refresh token is somehow present:
		// there is no access token here to renew.
		return OAuthLifecycle{
			State:           api.AuthStateNone,
			Action:          api.ActionLogin,
			Detail:          "client registration stored, no access token",
			HasRefreshToken: usable,
		}
	}
	lc := OAuthLifecycle{HasRefreshToken: usable}
	renew := renewActionFor(usable)
	switch {
	case st.Expired(now):
		lc.State, lc.Action = api.AuthStateExpired, renew
	case st.NeedsRefresh(now):
		lc.State, lc.Action = api.AuthStateExpiring, renew
	default:
		lc.State = api.AuthStateAuthorized
	}
	return lc
}

// revokedDetail renders what the provider said, which is the only
// provider-specific part of a refusal an operator can act on: "consent
// withdrawn" and "refresh token expired" call for different conversations
// with the same command. It falls back to a bare statement rather than
// inventing a reason.
func revokedDetail(st *oauthflow.State) string {
	if st.GrantRevokedReason == "" {
		return "the authorization server refused the stored grant"
	}
	return "the authorization server refused the stored grant: " + st.GrantRevokedReason
}

// renewActionFor is the machine-readable half of "which repair to offer": the
// one that needs no browser where it exists, a login where it does not.
func renewActionFor(hasRefreshToken bool) string {
	if hasRefreshToken {
		return api.ActionRefresh
	}
	return api.ActionLogin
}
