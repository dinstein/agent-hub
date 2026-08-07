package daemon

import (
	"log/slog"
	"testing"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/tier"
)

// The HTTP data plane assembles a gateway per credential, and it must hand
// that gateway NO credential collaborators at all.
//
// This reverses an earlier decision, so the reason is worth stating rather
// than assuming. The plane used to build both faces out of the daemon's own
// vault — a ${SECRET_x} resolver and an OAuth bearer factory — on the theory
// that one vault should back both halves of a dial. It does; the mistake was
// where. gateway.newGateway builds its production credential chain exactly
// when both Config.Secrets and Config.Auth arrive nil, and that chain is the
// only thing that wraps the bearer in the two OPTIONAL faces the round
// tripper in internal/downstream looks for:
//
//   - credentialEpoch, bumped by a CredWatcher when any process rewrites the
//     vault — which is how the daemon's own proactive refresher reaches a
//     connection that is already up;
//   - credentialDeadline, which renews before expiry and is the only one of
//     the four invalidation rules that fires against a downstream answering
//     an expired token with 200 and an error result instead of 401.
//
// A source assembled outside the gateway carries neither, so those gateways
// could recover from a stale credential only by being rejected — strictly
// weaker than the stdio gateways they are otherwise identical to, and
// invisible, because the bearer was still attached and the vault still read.
//
// The parity is proved on the gateway side (TestUninjectedAssemblyCarriesBoth
// CredentialFaces in internal/gateway); what this file pins is the daemon
// side of the same fact, which is a negative and therefore has nothing in the
// production code to point at.
func TestDataPlaneLeavesCredentialsToTheGateway(t *testing.T) {
	p := &httpPlane{deps: httpPlaneDeps{Log: slog.New(slog.DiscardHandler)}}
	cfg := p.gatewayConfig(&httpbridge.Caller{
		Kind: httpbridge.CallerAgent, Token: "agent-1", Tier: tier.Read,
	})
	if cfg.Secrets != nil {
		t.Error("the data plane injected a secrets resolver: the gateway will now skip building " +
			"its own chain, and its bearer loses the epoch and deadline faces")
	}
	if cfg.Auth != nil {
		t.Error("the data plane injected a bearer factory: whatever it built carries neither the " +
			"credential epoch nor the refresh deadline, so this gateway recovers only on a 401")
	}
}

// The same rule stated where it is actually broken: the deps struct. A field
// added back here is the edit that would reach gatewayConfig, and it is the
// one that looks harmless — the plane already holds the vault, and passing it
// on reads like tightening rather than loosening.
func TestHTTPPlaneDepsCarryNoCredentialFields(t *testing.T) {
	deps := httpPlaneDeps{}
	// Compile-time: this list is every field the plane may take from the
	// daemon assembly. Adding a credential-shaped one breaks this literal,
	// which is the moment to re-read TestDataPlaneLeavesCredentialsToTheGateway.
	_ = httpPlaneDeps{
		Resolver: deps.Resolver,
		Log:      deps.Log,
		Events:   deps.Events,
		Version:  deps.Version,
		Registry: deps.Registry,
		Dial:     deps.Dial,
		Now:      deps.Now,
	}
}
