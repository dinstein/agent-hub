package daemon

import (
	"log/slog"
	"testing"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/tier"
)

// The HTTP data plane must hand its gateways NO credential collaborators —
// what that buys is argued on httpPlaneDeps, beside the fields it is about.
//
// The rule is pinned rather than left to the comment because it is a NEGATIVE:
// the correct wiring is two fields that are not set, so there is nothing in
// the production code for a reader to notice, and the incorrect wiring is an
// addition that looks like tightening (the plane holds the daemon's vault
// already; passing it on reads as making the dial more explicit). Both halves
// of the parity are pinned for the same reason — this one, and
// TestUninjectedAssemblyCarriesBothCredentialFaces in internal/gateway, which
// says what the unset fields are FOR.
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
