package gateway

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// This file is the gateway's OAuth credential wiring: the bearer half of a
// downstream dial, which internal/downstream deliberately knows only through
// its TokenSource seam.
//
// Refresh serialization: a gateway takes the OFFLINE path — the
// <server>.refresh.lock sibling file lock — for the same reason the CLI does
// (internal/cli/vault.go header). Spending a one-time refresh token twice
// locks the user out of the server, and there is always another writer that
// an in-process singleflight cannot see: `agenthub auth login/refresh` writes
// the vault directly from its own process.
//
// THE REASON IS "ANOTHER WRITER", NOT "ANOTHER PROCESS", and the distinction
// became load-bearing when the daemon started hosting gateways of its own
// (internal/daemon/httpdata.go). Those run this file inside the same process
// as the daemon's proactive refresher, so a reader who takes the old wording
// literally concludes the lock is redundant there and reaches for Online.
// It is not redundant: every acquisition opens its own descriptor
// (oauthflow.Coordinator, refresh.go), and flock(2) is held per open file
// description, so two coordinators in one process exclude each other exactly
// as two processes do. Taking a lock that was not needed costs one syscall;
// that asymmetry is why Online stays nil here.
//
// Logging: this is the ONLY place a gateway's refresh can be recorded.
// The trigger lives one layer down, in internal/downstream's 401/403 round
// tripper, which deliberately swallows a refresh failure and hands the
// downstream's own 401 back (its WWW-Authenticate is the better diagnostic) —
// so without a line here the renewal that just failed leaves no trace at all,
// and a successful one is visible only as the credential announcement it
// happens to cause.
//
// The messages match internal/daemon/oauth.go's word for word, and the
// `trigger` field is what separates the two: which component renewed a token
// is a property of the DEPLOYMENT, not of the event, and the operator reading
// the log usually does not know whether a daemon was up. Prose only one side
// can be grepped for would make them answer that question by guessing.
//
// Same distinction as above, and now it decides more than wording: a
// data-plane gateway logs through the logger the daemon handed it, so both
// components' renewals land in the SAME daemon.log. The field is what tells
// them apart there, plus the client id every gateway line carries and the
// refresher's lines do not.

// secretsDir resolves <data>/secrets, the directory holding secrets.enc, the
// keyring key registry and the per-server refresh locks.
func secretsDir(resolver *platform.Resolver) (string, error) {
	data, err := resolver.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "secrets"), nil
}

// vaultAuth builds the per-server TokenSource factory: the access token
// comes from the vault, renewal goes through the offline refresh
// coordinator. One store and one coordinator are shared by every server so
// the lock and the vault handle are not rebuilt per dial.
//
// epochs is the credential announcement counter set (credwatch.go). Every
// source is wrapped in it, so a login or a refresh performed by ANY process
// drops the bearer this gateway has cached instead of waiting for the
// downstream to reject it. It is keyed by server, not by scope, on purpose —
// see credEpochs.
//
// log records every renewal this process performs; it must not be nil.
func vaultAuth(chain *secrets.Chain, dir string, epochs *credEpochs, log *slog.Logger) func(string, string) downstream.TokenSource {
	coord := oauthflow.NewCoordinator(oauthflow.CoordinatorConfig{
		Store:   oauthflow.NewStore(chain),
		Client:  oauthflow.NewClient(oauthflow.Config{}),
		LockDir: dir,
		// Online is deliberately nil: see the file comment.
	})
	resolve := chain.Resolver()
	// scopeName carries the derive key so a derived instance looks up its own
	// credential first and inherits the shared login only when it has none.
	// Refresh stays keyed on serverID: the refresh token is the server's, and
	// a per-scope lock would let two derivations spend it concurrently.
	return func(serverID, scopeName string) downstream.TokenSource {
		// The epoch is read by two layers for two different reasons: the
		// WithEpoch wrapper drops the CACHED BEARER when the vault changes,
		// and the proactive source drops its SCHEDULE. A renewal parked for a
		// day because the provider refused yesterday's grant must not survive
		// the login that replaced it.
		var epoch func() uint64
		if epochs != nil {
			epoch = func() uint64 { return epochs.get(serverID) }
		}
		// Two triggers, one coordinator. The inner source renews when a
		// downstream rejects the token (trigger=rejection); proactiveSource
		// renews before expiry, when a connection asks for the credential
		// (trigger=expiry, authfresh.go) — and that is the only one that
		// fires at all against a server which answers an expired token with
		// 200 rather than 401.
		ts := downstream.TokenSource(newProactiveSource(
			downstream.NewScopedVaultTokenSource(serverID, scopeName, resolve,
				loggedRenew(coord, serverID, scopeName, log)),
			coord, serverID, scopeName, epoch, log))
		if epoch == nil {
			return ts
		}
		return downstream.WithEpoch(ts, epoch)
	}
}

// loggedRenew is the renewal half of one server's TokenSource: it performs
// the refresh and records what happened.
//
// It takes the oauthflow.Refresher interface rather than *Coordinator so a
// test can assert the log without an authorization server — which is the only
// way to reach the SUCCESS branch here at all, since the coordinator this
// gateway builds screens loopback token endpoints out.
func loggedRenew(coord oauthflow.Refresher, serverID, scopeName string, log *slog.Logger) downstream.RefreshFunc {
	return func(ctx context.Context) (string, error) {
		// Debug, not Info: this line is the only way to tell a refresh that
		// hung on the sibling file lock (30s, held by another process doing a
		// network round trip) from one that was never attempted. Both look
		// identical from the outcome alone.
		log.Debug("refreshing a downstream access token", logx.Server(serverID),
			"trigger", oauthflow.TriggerRejection, "scope", scopeName)
		_, tok, err := coord.Refresh(ctx, serverID)
		switch {
		// ErrRefreshSuperseded means another writer already stored a fresh
		// credential; the token it returns IS usable, so it is a success for
		// the caller, not a failure.
		case err == nil, errors.Is(err, oauthflow.ErrRefreshSuperseded):
			log.Info("access token refreshed", logx.Server(serverID),
				"trigger", oauthflow.TriggerRejection, "superseded", err != nil)
			return tok, nil
		case oauthflow.NeedsLogin(err), oauthflow.IsUnmanaged(err):
			// Not transient: no retry and no amount of waiting fixes it, only
			// `agenthub auth login`. A grant the provider has already refused
			// is answered here from the vault, so this branch costs the
			// downstream one 401 and the provider nothing.
			log.Warn("token cannot be refreshed without a new login",
				logx.Server(serverID), "trigger", oauthflow.TriggerRejection, "error", err)
			return "", err
		default:
			// No attempt/retry_in here, and their absence is the information:
			// unlike the daemon's ladder, this path has no schedule of its own.
			// The next try is whenever the downstream rejects the token again.
			log.Warn("access token refresh failed",
				logx.Server(serverID), "trigger", oauthflow.TriggerRejection, "error", err)
			return "", err
		}
	}
}
