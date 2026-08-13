package cli

import (
	"fmt"
	"time"

	"github.com/dinstein/agent-hub/api"
)

// How the CLI RENDERS an OAuth lifecycle. What one IS lives in
// ctlapi.OAuthLifecycleOf.
//
// Two commands here answer "what is stored for this server, and what should I
// run": `auth status`, which is OAuth-only and reads values, and `server ls` /
// `server inspect`, which cover every credential kind and are index-first so
// they never pop a keychain dialog. Those differences are real and their wire
// shapes stay separate because of them — `auth status` has scope and the
// registrar, `server ls` has kind, missing secrets and a hint for every kind.
//
// The decision in the middle was the same for both and had been written
// twice, so it was folded into one copy here. A third surface, `GET /v1/auth`,
// turned out never to have been folded in at all — internal/ctlapi cannot
// import this package, so the fold was invisible from there. The decision
// moved to internal/ctlapi, which internal/cli already imports, and all three
// now read one answer; that file's header carries the rest of the story.
//
// What stays here is the English. A hint names commands, and commands are the
// CLI's vocabulary rather than the control plane's.

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
