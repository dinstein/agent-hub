package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// Per-server credential state, as `server ls` and `server inspect` report it.
//
// WHAT THIS IS, AND WHAT IT IS NOT. It answers "what credential does THIS
// MACHINE hold for this server" — a local fact, readable with the daemon
// down and every downstream unreachable. It does NOT answer "does the server
// currently accept it": that is a live 401, it is discovered by connecting,
// and it is reported by the enable probe and the Health contract. The ban on
// a persisted `needsAuth` (registry.OAuthHint, api.AuthStatus) is precisely
// the ban on confusing the two, so nothing here may be worded as health.
//
// COST. `server ls` is the command every error hint sends people to, so the
// enrichment is index-first: one Chain.List — the enc-file map plus the
// self-managed keyring key registry, both plain files — tells us WHICH
// entries exist without touching the OS keychain, and only a server that
// actually has OAuth state costs a value read. `__http_auth__` is never read
// at all; its value is the token, and presence is the whole question. This is
// the rule doctor's checkVault states for the vault self-test: a command that
// pops a keychain dialog is a command people stop running.

// Credential kinds of ServerAuth.Kind.
const (
	// authKindNone: nothing to authorize with, and nothing configured that
	// says one is needed.
	authKindNone = "none"
	// authKindOAuth: an OAuth login (stored state, or hints saying one is
	// expected).
	authKindOAuth = "oauth"
	// authKindToken: a token in the shared __http_auth__ slot with no OAuth
	// state behind it — the hand-pasted case.
	authKindToken = "token"
	// authKindSecret: ${SECRET_X} placeholders in env/headers.
	authKindSecret = "secret"
	// authKindHeader: a literal Authorization header in the entry itself.
	authKindHeader = "header"
	// authKindUnknown: the vault could not be read. Never rendered as "no
	// credential" — see the fail direction on newAuthProbe.
	authKindUnknown = "unknown"
)

// States beyond the api.AuthState* set, which covers the OAuth lifecycle
// only. These two describe credentials that HAVE no lifecycle to report
// offline: a stored token or secret is present or absent, and whether it
// still works is a live 401's answer, not this file's.
const (
	// authStateStored: the credential exists in the vault.
	authStateStored = "stored"
	// authStateMissing: the entry names a vault key that does not exist.
	authStateMissing = "missing"
	// authStateConfigured: the credential is in the registry entry itself.
	authStateConfigured = "configured"
)

// ServerAuth is one server's credential state. It carries no value field of
// any kind, which is how docs/modules/controlplane.md rule 5 is held here:
// no formatting mistake can print a token that was never fetched.
type ServerAuth struct {
	// Kind is one of the authKind* constants.
	Kind string `json:"kind"`
	// State is an api.AuthState* constant for OAuth, or one of the authState*
	// constants above for the rest.
	State string `json:"state"`
	// Action is the api.Action* suggestion, the same vocabulary the daemon's
	// Health uses, so a frontend joining the two never has to translate.
	Action string `json:"action,omitempty"`
	Issuer string `json:"issuer,omitempty"`
	// ExpiresAt is Unix seconds; 0 means the provider advertised no expiry,
	// which is "never expires", NOT "expired" (docs/modules/oauth.md).
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	ExpiresIn int64 `json:"expiresIn,omitempty"`
	// HasRefreshToken decides whether an expiry is repairable without a
	// human; the token itself is never rendered.
	HasRefreshToken bool `json:"hasRefreshToken,omitempty"`
	// MissingSecrets names the vault keys the entry needs and does not have,
	// in the spelling `agenthub secret set` takes.
	MissingSecrets []string `json:"missingSecrets,omitempty"`
	Detail         string   `json:"detail,omitempty"`
	// Hint is the repair sentence the text output prints under the table,
	// carried on the row so --json does not have to rebuild it from kind,
	// state and action — the reconstruction that would be free to drift, and
	// that a caller has no way to check. Empty for a row that needs no
	// repair, which is what makes it usable as "is there anything to do".
	//
	// It names commands and the server id, never a credential: it is composed
	// by hint() from this struct, which holds no value field at all.
	Hint string `json:"hint,omitempty"`
}

// authProbe is the local credential store, narrowed to the two questions the
// classification asks: does this entry exist, and — for OAuth only — what
// does it say. Both are injected so the ladder can be tested without a vault.
type authProbe struct {
	// index holds the storage keys of every stored ref. nil with a nil
	// indexErr means "nothing is stored", which is a legitimate answer.
	index map[string]bool
	// indexErr short-circuits every row to an error state. It is the
	// difference between "no credential" and "I could not look", and
	// collapsing the two is the stale-green failure this package exists to
	// avoid elsewhere.
	indexErr  error
	loadState func(ctx context.Context, serverID string) (*oauthflow.State, error)
	lookupEnv func(string) (string, bool)
}

// newAuthProbe builds the probe for one CLI invocation.
//
// Failure direction: FAIL-OPEN for the listing, FAIL-VISIBLE for the cells.
// A vault that cannot be read must not take the server list down with it —
// the ids, transports and targets are registry facts and remain true — but
// every credential cell then reads "error", never "-". Rendering an
// unreadable vault as "no credential needed" is the same lie as a stale
// needsAuth:false, and it would be told on exactly the machine where the
// operator most needs the truth.
func (a *App) newAuthProbe(ctx context.Context) (authProbe, []string) {
	fail := func(err error, warning string) (authProbe, []string) {
		return authProbe{indexErr: err}, []string{warning}
	}
	deps, err := a.newOAuthDeps(false)
	if err != nil {
		return fail(err, "credential state unavailable: "+err.Error())
	}
	// An enc file nobody can decrypt makes List return the keyring half only,
	// silently. Asking first turns that into an error instead of an answer
	// that is short by however much secrets.enc was holding.
	if deps.chain.HasUnreadableEnc() {
		return fail(secrets.ErrEncUnreadable,
			"credential state incomplete: "+secrets.ErrEncUnreadable.Error()+
				"; set AGENTHUB_SECRET_KEY to include what it holds")
	}
	refs, err := deps.chain.List(ctx)
	if err != nil {
		return fail(err, "credential state unavailable: "+err.Error())
	}
	index := make(map[string]bool, len(refs))
	for _, ref := range refs {
		index[ref.StorageKey()] = true
	}
	return authProbe{
		index:     index,
		loadState: deps.store.LoadState,
		lookupEnv: os.LookupEnv,
	}, nil
}

// classify walks the ladder, first match wins:
//
//  1. a required secret is missing        2. a literal Authorization header
//  3. stored OAuth state                  4. a stored token
//  5. OAuth hints and nothing stored      6. secrets, all present
//  7. nothing
//
// Rungs 1 and 2 outrank the credential itself for two different reasons. A
// missing secret comes first to agree with ComputeHealth, whose rung 2 says
// the same thing to the GUI — one server must not be "authorized" here and
// "missing secrets" there. A literal Authorization header comes next because
// it is what will actually be SENT: attachBearer leaves an explicit header
// alone, so a stored token behind one is dead weight, and a column reporting
// the token would name a credential this server never uses.
//
// Rung 7 does not guess. An HTTP endpoint with no credential and no hints is
// "-", not "probably needs a login": whether it does is a live 401's answer.
//
// The repair sentence is attached HERE, once, rather than by each output
// mode. The text footer and the --json row then cannot say different things
// about the same server, which is the whole failure this file has already
// had once, between the action field and that sentence.
func (p authProbe) classify(ctx context.Context, id string, e registry.ServerEntry, now time.Time) ServerAuth {
	out := p.classifyStored(ctx, id, e, now)
	out.Hint = out.hint(id)
	return out
}

// classifyStored is the ladder itself; classify wraps it.
func (p authProbe) classifyStored(ctx context.Context, id string, e registry.ServerEntry, now time.Time) ServerAuth {
	if p.indexErr != nil {
		return ServerAuth{Kind: authKindUnknown, State: api.AuthStateError, Detail: p.indexErr.Error()}
	}
	refs := secretRefsOf(e)
	if missing := p.missingSecrets(id, refs); len(missing) > 0 {
		return ServerAuth{
			Kind: authKindSecret, State: authStateMissing,
			Action: api.ActionSetSecret, MissingSecrets: missing,
		}
	}
	if hasLiteralAuthorization(e) {
		out := ServerAuth{Kind: authKindHeader, State: authStateConfigured}
		if p.stored(id, secrets.KeyHTTPAuth) {
			out.Detail = "a literal Authorization header overrides the stored token"
		}
		return out
	}
	if p.stored(id, secrets.KeyOAuthState) {
		if out, ok := p.classifyOAuth(ctx, id, now); ok {
			return out
		}
	}
	if p.stored(id, secrets.KeyHTTPAuth) {
		return ServerAuth{Kind: authKindToken, State: authStateStored}
	}
	if e.OAuth != nil {
		return ServerAuth{Kind: authKindOAuth, State: api.AuthStateNone, Action: api.ActionLogin}
	}
	if len(refs) > 0 {
		return ServerAuth{Kind: authKindSecret, State: authStateStored}
	}
	return ServerAuth{Kind: authKindNone, State: api.AuthStateNone}
}

// classifyOAuth reads the stored state. The bool is false when the store says
// there is none after all — the index and the store disagreeing is not an
// error, it is a credential cleared between the two reads, and the ladder
// simply continues below rung 3.
//
// Every other failure becomes an error ROW, never an aborted listing: this is
// a diagnostic, and one corrupt entry must not hide the other servers (the
// rule authStatusOf already follows).
func (p authProbe) classifyOAuth(ctx context.Context, id string, now time.Time) (ServerAuth, bool) {
	st, err := p.loadState(ctx, id)
	switch {
	case errors.Is(err, oauthflow.ErrNoState):
		return ServerAuth{}, false
	case err != nil:
		return ServerAuth{Kind: authKindOAuth, State: api.AuthStateError, Detail: err.Error()}, true
	}
	out := ServerAuth{
		Kind:            authKindOAuth,
		Issuer:          st.Issuer,
		ExpiresAt:       st.ExpiresAt,
		ExpiresIn:       secondsUntil(st.ExpiresAt, now),
		HasRefreshToken: st.RefreshToken != "",
	}
	if !p.stored(id, secrets.KeyHTTPAuth) {
		// State without a token is the DCR-credentials-only shape: a login was
		// started (or the token write failed) and nothing usable came of it.
		out.State, out.Action = api.AuthStateNone, api.ActionLogin
		out.Detail = "client registration stored, no access token"
		return out, true
	}
	// The action follows the same rule renewCommand renders in the hint, and
	// for the same reason: a stored refresh token makes the repair unattended,
	// and a caller reading only `action` must not be sent to a browser for a
	// renewal no human needs to watch. The DCR-credentials-only branch above
	// keeps `login` on purpose — it has no access token to renew.
	switch {
	case st.Expired(now):
		out.State, out.Action = api.AuthStateExpired, out.renewAction()
	case st.NeedsRefresh(now):
		out.State, out.Action = api.AuthStateExpiring, out.renewAction()
	default:
		out.State = api.AuthStateAuthorized
	}
	return out, true
}

// renewAction is renewCommand's machine-readable half: the same choice, in
// the api.Action* vocabulary. Keeping them one line apart is what stops the
// column and the hint from ever again saying different things.
func (a *ServerAuth) renewAction() string {
	if a.HasRefreshToken {
		return api.ActionRefresh
	}
	return api.ActionLogin
}

// missingSecrets returns the keys of refs that resolve nowhere.
func (p authProbe) missingSecrets(id string, refs []string) []string {
	var out []string
	for _, key := range refs {
		if !p.stored(id, key) {
			out = append(out, key)
		}
	}
	return out
}

// stored reports whether (id, _global, key) resolves, WITHOUT reading it.
//
// Two blind spots, both inherited from the levels this consults and both
// shared with `secret ls`: a value put into the OS keychain by hand is not in
// the key registry, and a keyring whose availability probe is failing is
// skipped by the chain itself. Neither can be closed without reading values,
// which is the cost this whole file is arranged to avoid.
func (p authProbe) stored(id, key string) bool {
	if p.index[secrets.Ref{ServerID: id, Key: key}.StorageKey()] {
		return true
	}
	return p.storedInEnv(key)
}

// storedInEnv mirrors Chain.envValue: level 1 is AGENTHUB_SECRET_<KEY>
// (never AGENTHUB_SECRET_KEY, which is the enc-file key material), level 2 is
// the bare name behind AGENTHUB_ALLOW_BARE_SECRET_ENV=1 and never an
// AGENTHUB_* one.
//
// The environment levels are keyed by KEY ALONE, not by server, so an
// exported AGENTHUB_SECRET_GITHUB_TOKEN satisfies every server naming
// GITHUB_TOKEN. That is not an approximation made here — it is how the value
// will actually resolve at connect time, and a column that disagreed with the
// resolver would be worse than no column.
func (p authProbe) storedInEnv(key string) bool {
	nonEmpty := func(name string) bool {
		v, ok := p.lookupEnv(name)
		return ok && strings.TrimSpace(v) != ""
	}
	if name := secrets.EnvName(key); name != secrets.EnvEncKey && nonEmpty(name) {
		return true
	}
	if v, ok := p.lookupEnv(secrets.EnvAllowBare); !ok || v != "1" {
		return false
	}
	bare := secrets.BareEnvName(key)
	return !strings.HasPrefix(bare, "AGENTHUB_") && nonEmpty(bare)
}

// hasLiteralAuthorization reports an Authorization header whose value is the
// credential itself rather than a ${SECRET_X} placeholder. The test is narrow
// on purpose: any other header may or may not authenticate anything, and
// guessing would put a credential kind on entries that have none.
func hasLiteralAuthorization(e registry.ServerEntry) bool {
	for name, value := range e.Headers {
		if !strings.EqualFold(name, "authorization") {
			continue
		}
		if strings.TrimSpace(value) != "" && len(downstream.SecretKeysIn(value)) == 0 {
			return true
		}
	}
	return false
}

// cell renders the AUTH column. No value here may read as a verdict on
// whether the credential WORKS: "oauth" says one is stored, nothing more.
func (a *ServerAuth) cell() string {
	if a == nil || a.Kind == authKindNone {
		return "-"
	}
	if a.State == api.AuthStateError {
		return "error"
	}
	switch {
	case a.Kind == authKindOAuth:
		switch a.State {
		case api.AuthStateExpiring:
			return "oauth:expiring"
		case api.AuthStateExpired:
			return "oauth:expired"
		case api.AuthStateNone:
			return "oauth:login"
		default:
			return "oauth"
		}
	case a.Kind == authKindSecret && a.State == authStateMissing:
		return "secret:missing"
	default:
		return a.Kind
	}
}

// hintText is the repair sentence as CARRIED, nil-safe. Every renderer reads
// this rather than calling hint() again: the sentence is composed once, in
// classify, and the text output must print the same bytes --json carries.
func (a *ServerAuth) hintText() string {
	if a == nil {
		return ""
	}
	return a.Hint
}

// hint composes the one-line repair instruction for a row that needs one, or
// "". Called once per row by classify; read it back through hintText.
func (a *ServerAuth) hint(id string) string {
	if a == nil {
		return ""
	}
	if a.State == api.AuthStateError {
		return fmt.Sprintf("%s: the stored credential could not be read: %s",
			id, oneLine(a.Detail, descriptionColumnBytes))
	}
	switch {
	case a.Kind == authKindSecret && a.State == authStateMissing:
		return fmt.Sprintf("%s: secret %s not stored — run 'agenthub secret set %s %s'",
			id, strings.Join(a.MissingSecrets, ", "), id, a.MissingSecrets[0])
	case a.Kind != authKindOAuth:
		return ""
	}
	switch a.State {
	case api.AuthStateNone:
		return fmt.Sprintf("%s: not signed in — run 'agenthub auth login %s'", id, id)
	case api.AuthStateExpired:
		return fmt.Sprintf("%s: sign-in expired — run '%s'", id, a.renewCommand(id))
	case api.AuthStateExpiring:
		return fmt.Sprintf("%s: sign-in expires in %s — run '%s'",
			id, time.Duration(a.ExpiresIn)*time.Second, a.renewCommand(id))
	default:
		return ""
	}
}

// renewCommand prefers refresh over login when a refresh token exists: the
// repair that needs no browser is the one to offer first, and `auth login`
// stays the answer for the case where nothing can be renewed without a human.
func (a *ServerAuth) renewCommand(id string) string {
	if a.HasRefreshToken {
		return "agenthub auth refresh " + id
	}
	return "agenthub auth login " + id
}

// line renders the credential for `server inspect`, where one server has the
// whole width to itself and the expiry is worth spelling out.
func (a *ServerAuth) line() string {
	if a == nil {
		return "-"
	}
	parts := []string{a.Kind + " " + a.State}
	switch {
	case a.State == api.AuthStateError, a.Kind != authKindOAuth:
	case a.ExpiresAt == 0:
		parts = append(parts, "no expiry advertised")
	case a.ExpiresIn == 0:
		parts = append(parts, "expired")
	default:
		parts = append(parts, "expires in "+(time.Duration(a.ExpiresIn)*time.Second).String())
	}
	if a.Kind == authKindOAuth && a.State != api.AuthStateError {
		parts = append(parts, "refresh "+boolText(a.HasRefreshToken))
	}
	if len(a.MissingSecrets) > 0 {
		parts = append(parts, "not stored: "+strings.Join(a.MissingSecrets, ", "))
	}
	if a.Issuer != "" {
		parts = append(parts, "issuer "+a.Issuer)
	}
	if a.Detail != "" {
		parts = append(parts, oneLine(a.Detail, descriptionColumnBytes))
	}
	return strings.Join(parts, ", ")
}
