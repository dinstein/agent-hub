package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/oauthlogin"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/skills"
)

// This file assembles the collaborators behind the non-registry control
// plane: credentials, skills, agent tokens, client adapters and OAuth
// status. The endpoints themselves live in internal/ctlapi and were routed
// before this wiring existed, which meant they answered "this daemon does
// not serve that endpoint yet" — the routes were real, the dependencies
// were nil. Declaring an interface is not the same as satisfying it, and
// the gap is invisible from either side alone.
//
// Every dependency is optional by design: a vault that cannot be opened
// costs the secrets endpoints and nothing else. The daemon must keep
// coordinating everything it still can, so each failure logs and continues
// rather than refusing to start.

// serverStateForgetters builds the out-of-registry cleanups that
// DELETE /v1/servers/{id} runs, so the daemon strips exactly the footprint
// `agenthub server rm` does. Removing a server must not leave a cached
// catalog behind for whatever is re-added under that id to inherit.
//
// Same optional-dependency discipline as the rest of this file: a store that
// will not open is omitted rather than failing the daemon, and confops turns
// whatever is missing into a warning on the response.
func serverStateForgetters(resolver *platform.Resolver) []confops.StateForgetter {
	var out []confops.StateForgetter
	out = append(out, confops.StateFunc{
		Name: "the cached tool list",
		Forget: func(_ context.Context, id string) error {
			return gateway.ForgetToolCache(resolver, id)
		},
	})
	return out
}

// nonRegistryDeps builds what the non-registry endpoints need. vault may be
// nil, in which case the production chain over <data>/secrets is used —
// the same chain the OAuth refresher gets, because two chains over one
// vault would be two caches of the same rotating credential.
//
// The agent-token store is returned CONCRETELY alongside the deps: the
// control plane only needs the ctlapi.TokenStore interface, but the data
// plane's authenticator needs *httpbridge.Store, and both must be the same
// object — a second store would be a second HMAC-key cache over one file.
func nonRegistryDeps(cfg Config, dataDir string, vault secrets.Store, log *slog.Logger, events *eventlog.Stream, coord func() *oauthflow.Coordinator) (ctlapi.NonRegistryDeps, *httpbridge.Store) {
	secretsDir := filepath.Join(dataDir, "secrets")
	if vault == nil {
		vault = secrets.NewChain(secrets.ChainConfig{Dir: secretsDir})
	}
	deps := ctlapi.NonRegistryDeps{
		CallsRoot:     calllog.DirFor(dataDir),
		EventLogPath:  filepath.Join(dataDir, "logs", eventlog.FileName),
		LogsDir:       filepath.Join(dataDir, "logs"),
		CallsKeys:     vault,
		SecretsDir:    secretsDir,
		ClientBaseDir: cfg.ClientBaseDir,
		Clients:       clients.Default(),
	}
	if chain, ok := vault.(*secrets.Chain); ok {
		// SecretVault needs List, which only the chain exposes; an injected
		// test store that cannot enumerate simply leaves the endpoint off
		// rather than pretending the vault is empty.
		deps.Secrets = chain
	}
	oauthStore := oauthflow.NewStore(vault)
	deps.OAuth = oauthStore
	deps.TestDeps = testDeps(vault, coord)
	deps.Logins = loginSessions(oauthStore, log, events)

	if lib, err := skills.Open(filepath.Join(dataDir, "skills"), skills.Options{}); err != nil {
		log.Warn("skills library unavailable; its endpoints stay off", "error", err)
	} else {
		deps.Skills = lib
	}
	var tokens *httpbridge.Store
	if store, err := httpbridge.OpenStore(dataDir); err != nil {
		log.Warn("agent-token store unavailable; its endpoints stay off", "error", err)
	} else {
		tokens = store
		deps.Tokens = store
	}
	if exe, err := os.Executable(); err == nil {
		deps.Executable = exe
	}
	return deps, tokens
}

// loginSessions builds the interactive-login manager behind
// POST /v1/auth/{server}/login.
//
// The flow is constructed PER LOGIN because AllowLoopback is baked into the
// oauthflow client's SSRF screen at construction time and follows the
// server's own provenance. Sharing one client across every login would mean
// screening them all against the loosest entry's rule — which is how one
// server declared local quietly exempts the other twenty-nine.
//
// The store is the daemon's own, the same one auth status and logout read, so
// a login lands where every other reader is already looking.
func loginSessions(store *oauthflow.Store, log *slog.Logger, events *eventlog.Stream) ctlapi.LoginSessions {
	m, err := oauthlogin.New(oauthlogin.Config{
		Flows: func(allowLoopback bool) oauthlogin.Flow {
			return oauthflow.NewFlow(oauthflow.NewClient(oauthflow.Config{
				AllowLoopback: allowLoopback,
			}), store)
		},
		Events: events,
		Log:    log,
	})
	if err != nil {
		// Never reached with a non-nil factory, but the endpoint staying off
		// is the right failure: the rest of the auth group keeps working and
		// the CLI's `auth login` is unaffected.
		log.Warn("interactive login unavailable; its endpoints stay off", "error", err)
		return nil
	}
	return m
}

// The HTTP data plane once built its gateways' credentials here, out of the
// daemon's vault, the way testDeps below still does for the self-test. It no
// longer does, and the asymmetry is the point rather than an inconsistency.
//
// A self-test is one dial that answers one question and ends; whatever it is
// handed is the whole of its credential life. A data-plane gateway is a
// long-lived connection holder, and for it the vault read is only the first
// of four things a credential needs — the other three are cache invalidation
// rules, and all three live in the chain the gateway builds for itself
// (internal/gateway/auth.go, authfresh.go, credwatch.go). Handing that
// gateway a TokenSource assembled out here suppressed the chain and
// delivered none of the three, so the plane now hands it nothing at all.
//
// TestDataPlaneLeavesCredentialsToTheGateway is what keeps that true.

// testDeps builds the credential collaborators for POST /v1/servers/{id}/test.
//
// Without it the handler probes with a bare downstream.Deps and every
// authorized server answers 401 — a failure that reads exactly like an
// expired token (the hint even tells the operator to log in again) while
// the real cause is that no credential was ever attached. The CLI's own
// `server test` has always passed these, which is why the same server would
// connect from the terminal and fail from the GUI.
//
// coord is late-bound: the refresh coordinator is created after the control
// plane, so this closure resolves it per call instead of capturing a nil.
// A nil coordinator (data dir unresolved — the daemon then runs without
// proactive refresh at all) leaves renewal off; the vault's stored token is
// still sent, so a self-test degrades to "works until the token expires"
// rather than failing outright.
func testDeps(vault secrets.Store, coord func() *oauthflow.Coordinator) func(string, downstream.Spec) downstream.Deps {
	chain, _ := vault.(*secrets.Chain)
	return func(id string, spec downstream.Spec) downstream.Deps {
		var deps downstream.Deps
		if chain != nil {
			deps.Secrets = chain.Resolver()
		}
		// Only HTTP transports carry a bearer; a stdio child gets its
		// credentials through the environment, which Secrets already covers.
		if !spec.IsHTTP() || chain == nil || coord == nil {
			return deps
		}
		c := coord()
		if c == nil {
			return deps
		}
		deps.Auth = downstream.NewVaultTokenSource(id, chain.Resolver(),
			func(ctx context.Context) (string, error) {
				_, tok, err := c.Refresh(ctx, id)
				// ErrRefreshSuperseded means another writer already stored a
				// fresh credential; the token it returns IS usable, so it is
				// a success for the caller, not a failure.
				if err != nil && !errors.Is(err, oauthflow.ErrRefreshSuperseded) {
					return "", err
				}
				return tok, nil
			})
		return deps
	}
}
