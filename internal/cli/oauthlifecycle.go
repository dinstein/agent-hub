package cli

import (
	"fmt"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/oauthflow"
)

// The OAuth lifecycle decision, in ONE copy.
//
// Two commands answer "what is stored for this server, and what should I
// run": `auth status`, which is OAuth-only and reads values, and `server ls` /
// `server inspect`, which cover every credential kind and are index-first so
// they never pop a keychain dialog. Those differences are real and their wire
// shapes stay separate because of them — `auth status` has scope and the
// registrar, `server ls` has kind, missing secrets and a hint for every kind.
//
// What was NOT different, and was nevertheless written twice, is the decision
// in the middle: is there an access token at all, is it past its deadline, is
// it inside the refresh grace, and can it be renewed without a human. Branch
// for branch, down to the wording of the DCR-only detail. It drifted exactly
// once, exactly as a duplicated decision does: the refresh action was taught
// to one copy, so one server read `refresh` in one command and `login` in the
// other. Both copies now call this file.
//
// It decides from STORED STATE ALONE and says nothing about whether the
// provider still accepts the credential — that is a live 401's answer, and the
// ban on a persisted needsAuth is the same rule seen from here.

// oauthLifecycle is what the stored state implies, in the vocabulary both
// commands already emit: an api.AuthState* value, an api.Action* suggestion,
// and the sentence explaining a state that would otherwise puzzle a reader.
type oauthLifecycle struct {
	State  string
	Action string
	Detail string
}

// lifecycleOf decides one server's OAuth lifecycle. hasAccessToken is passed
// in rather than read here because the two callers learn it differently — one
// loads the token, the other asks its index whether the key exists — and that
// difference is the whole cost distinction between them.
func lifecycleOf(st *oauthflow.State, hasAccessToken bool, now time.Time) oauthLifecycle {
	if st.GrantRevoked() {
		// Outranks every rung below, the missing-token one included: whatever
		// else this record lacks, the provider has already answered the
		// question all of them lead to. The action is FIXED at login — a
		// refused grant makes `auth refresh` a command that can only fail,
		// whatever bytes are still stored.
		return oauthLifecycle{
			State:  api.AuthStateRevoked,
			Action: api.ActionLogin,
			Detail: revokedDetail(st),
		}
	}
	if !hasAccessToken {
		// State without a token is the DCR-credentials-only shape: a login was
		// started (or the token write failed) and nothing usable came of it.
		// The action stays login even when a refresh token is somehow present:
		// there is no access token here to renew.
		return oauthLifecycle{
			State:  api.AuthStateNone,
			Action: api.ActionLogin,
			Detail: "client registration stored, no access token",
		}
	}
	renew := renewActionFor(st.RefreshToken != "")
	switch {
	case st.Expired(now):
		return oauthLifecycle{State: api.AuthStateExpired, Action: renew}
	case st.NeedsRefresh(now):
		return oauthLifecycle{State: api.AuthStateExpiring, Action: renew}
	default:
		return oauthLifecycle{State: api.AuthStateAuthorized}
	}
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

// renewActionFor is renewCommandFor's machine-readable half: one predicate
// answering in two vocabularies. Keeping them adjacent is what stops a field
// and the sentence beside it from ever again naming different repairs.
func renewActionFor(hasRefreshToken bool) string {
	if hasRefreshToken {
		return api.ActionRefresh
	}
	return api.ActionLogin
}

// renewCommandFor prefers refresh over login when a refresh token exists: the
// repair that needs no browser is the one to offer first, and `auth login`
// stays the answer where nothing can be renewed without a human.
func renewCommandFor(id string, hasRefreshToken bool) string {
	if hasRefreshToken {
		return "agenthub auth refresh " + id
	}
	return "agenthub auth login " + id
}

// oauthHintFor is the repair sentence for one OAuth lifecycle state, or "" for
// a state that needs no repair. Both commands render it, so a server cannot be
// told two different things depending on which one you happened to run.
//
// It names commands and the server id and nothing else: no caller passes it a
// credential value, and there is none in the lifecycle to pass.
func oauthHintFor(id, state string, hasRefreshToken bool, expiresIn int64, detail string) string {
	switch state {
	case api.AuthStateNone:
		return fmt.Sprintf("%s: not signed in — run 'agenthub auth login %s'", id, id)
	case api.AuthStateRevoked:
		// Never renewCommandFor: this is the one state where the unattended
		// repair is known not to exist. The detail is the provider's own
		// words, which is what makes the sentence worth reading twice.
		return fmt.Sprintf("%s: %s — run 'agenthub auth login %s'", id, detail, id)
	case api.AuthStateExpired:
		return fmt.Sprintf("%s: sign-in expired — run '%s'", id, renewCommandFor(id, hasRefreshToken))
	case api.AuthStateExpiring:
		return fmt.Sprintf("%s: sign-in expires in %s — run '%s'",
			id, time.Duration(expiresIn)*time.Second, renewCommandFor(id, hasRefreshToken))
	default:
		return ""
	}
}
