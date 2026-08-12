package ctlapi

import "sync"

// This file is the OTHER half of a server's runtime state, and it exists
// because the half that was here could not answer the question.
//
// GatewayStates folds what connected gateways report, and a gateway reports
// what it can see: conn, tools, detail, needs_auth. Nothing in a gateway
// looks at token lifetimes, so ComputeHealth's token rung had no producer at
// all — it was live code reached only by tests, and the GUI learned about an
// expired credential from a failed connection or not at all.
//
// Worse, it could not have been fixed there. GatewayStates.ServerRuntime
// reports ok=false when no gateway currently holds a server, which is the
// steady state of a daemon nobody is connected to — and "the token died
// while nobody was looking" is precisely the case worth reporting.
//
// So the token half is sourced from the vault instead, published by the
// component that already reads every server's OAuth state on a timer: the
// daemon's proactive refresher (internal/daemon/oauth.go). That costs no
// extra vault access, which is the constraint that decided it — a read per
// /v1/servers request could pop an OS keychain dialog, and a command that
// pops a keychain dialog is a command people stop running.

// TokenFacts is one server's credential lifecycle, as the vault knows it.
type TokenFacts struct {
	// State is the lifecycle rung.
	State TokenState
	// HasRefreshToken means a refresh token is stored AND is not known to
	// have been refused — i.e. an unattended repair is actually AVAILABLE,
	// which is the question the health contract's action is asking. A
	// revoked grant reports false: the bytes are still there and offering
	// `auth refresh` for them would send an operator to a command that can
	// only fail.
	HasRefreshToken bool
}

// TokenStateSource supplies the vault half of a server's runtime state.
// ok=false means "nothing is known about this server", which is the honest
// answer for a stdio server, a server with no OAuth state, and every server
// at all when no producer is wired up.
type TokenStateSource interface {
	TokenState(serverID string) (TokenFacts, bool)
}

// TokenStates is the holder a producer publishes into and the control plane
// reads back. It is constructed before the server it feeds so the wiring
// needs no late binding: an empty holder answers ok=false for everything,
// which is exactly the behaviour of no producer at all.
type TokenStates struct {
	mu sync.Mutex
	m  map[string]TokenFacts
}

// NewTokenStates builds an empty holder.
func NewTokenStates() *TokenStates { return &TokenStates{} }

var _ TokenStateSource = (*TokenStates)(nil)

// Replace swaps in a whole snapshot.
//
// Whole rather than incremental on purpose: the producer rebuilds the set
// every scan anyway, and a server that has been deleted, disabled or logged
// out then disappears by construction. An incremental API would need a
// matching Forget on every one of those paths, and the failure of forgetting
// one is a health badge for a credential that no longer exists.
func (t *TokenStates) Replace(facts map[string]TokenFacts) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m = facts
}

// TokenState implements TokenStateSource.
func (t *TokenStates) TokenState(serverID string) (TokenFacts, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.m[serverID]
	return f, ok
}
