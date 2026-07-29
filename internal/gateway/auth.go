package gateway

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// This file is the gateway's OAuth credential wiring: the bearer half of a
// downstream dial, which internal/downstream deliberately knows only through
// its TokenSource seam.
//
// Refresh serialization: the stdio gateway takes the OFFLINE path — the
// <server>.refresh.lock sibling file lock — for the same reason the CLI does
// (internal/cli/vault.go header). It is a separate process from the daemon,
// so an in-process singleflight would protect nothing across the two, and
// spending a one-time refresh token twice locks the user out of the server.
// Taking a lock that was not needed costs one syscall; that asymmetry is why
// Online stays nil here.

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
func vaultAuth(chain *secrets.Chain, dir string) func(string, string) downstream.TokenSource {
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
		return downstream.NewScopedVaultTokenSource(serverID, scopeName, resolve,
			func(ctx context.Context) (string, error) {
				_, tok, err := coord.Refresh(ctx, serverID)
				// ErrRefreshSuperseded means another writer already stored a
				// fresh credential; the token it returns IS usable, so it is
				// a success for the caller, not a failure.
				if err != nil && !errors.Is(err, oauthflow.ErrRefreshSuperseded) {
					return "", err
				}
				return tok, nil
			})
	}
}
