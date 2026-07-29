package daemon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/tier"
)

// The HTTP data plane assembles a gateway per credential. It passes a
// non-nil Secrets, which SUPPRESSES the gateway's own production credential
// default — so an Auth it forgets to pass is not filled in by anyone, and
// every OAuth downstream is dialed bare and answers 401 while the vault
// holds a valid token. That is the third site of one omission (the stdio
// gateway and the server self-test were the other two), which is why it is
// pinned here rather than left to an integration test that needs a real
// authorization server.

func TestDataPlaneGatewayCarriesTheOAuthBearer(t *testing.T) {
	p := &httpPlane{deps: httpPlaneDeps{
		Log: slog.New(slog.DiscardHandler),
		Secrets: func(context.Context, secrets.Ref) (string, bool, error) {
			return "", false, nil
		},
		Auth: planeAuth(secrets.NewChain(secrets.ChainConfig{Dir: t.TempDir()}), nil),
	}}
	cfg := p.gatewayConfig(&httpbridge.Caller{Kind: httpbridge.CallerAgent, Token: "agent-1", Tier: tier.Read})
	if cfg.Auth == nil {
		t.Fatal("the data plane assembles gateways with no bearer: every OAuth downstream would 401")
	}
	if ts := cfg.Auth("remote", secrets.DefaultScope); ts == nil {
		t.Fatal("no TokenSource for a downstream: the bearer would never be attached")
	}
}

// Secrets and Auth resolve against the SAME vault or a dial expands its
// ${SECRET_X} placeholders from one store and its bearer from another. The
// pair is chosen together at the daemon assembly point; nil vault means both
// stay nil and the gateway builds its own production chain — the one place
// allowed to do so.
func TestBothCredentialFacesComeFromOneVault(t *testing.T) {
	if got := dataPlaneSecrets(nil); got != nil {
		t.Error("a nil vault must leave Secrets nil so the gateway builds its own chain")
	}
	if got := planeAuth(nil, nil); got != nil {
		t.Error("a nil vault must leave Auth nil, or the faces disagree about which vault backs a dial")
	}
	vault := secrets.NewChain(secrets.ChainConfig{Dir: t.TempDir()})
	if dataPlaneSecrets(vault) == nil || planeAuth(vault, nil) == nil {
		t.Error("a configured vault must back BOTH faces")
	}
}

// The coordinator is created after the control plane, so the plane holds an
// accessor rather than a value. Resolving it per call is what keeps a
// gateway assembled before the refresher from capturing a nil forever.
func TestRefreshCoordinatorIsResolvedPerCall(t *testing.T) {
	var current *oauthflow.Coordinator
	auth := planeAuth(secrets.NewChain(secrets.ChainConfig{Dir: t.TempDir()}),
		func() *oauthflow.Coordinator { return current })

	// Assembled while the refresher does not exist yet: renewal is off, but
	// the stored token is still attached (degrade, do not fail closed).
	ts := auth("remote", secrets.DefaultScope)
	if ts == nil {
		t.Fatal("no TokenSource before the coordinator exists")
	}
	if _, err := ts.Refresh(context.Background()); err == nil {
		t.Error("refresh must report that no refresher is wired yet, not silently succeed")
	}

	// Once the daemon stores its coordinator, the SAME source must pick it
	// up — a captured nil would leave this gateway unable to renew for its
	// whole lifetime.
	current = oauthflow.NewCoordinator(oauthflow.CoordinatorConfig{
		Store:   oauthflow.NewStore(secrets.NewChain(secrets.ChainConfig{Dir: t.TempDir()})),
		Client:  oauthflow.NewClient(oauthflow.Config{}),
		LockDir: t.TempDir(),
	})
	if _, err := ts.Refresh(context.Background()); err == nil {
		t.Fatal("expected a refresh attempt against the now-present coordinator")
	} else if err.Error() == "downstream: no token refresher wired" {
		t.Fatal("the source captured a nil coordinator instead of resolving it per call")
	}
}
