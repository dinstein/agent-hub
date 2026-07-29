package cli

import (
	"context"
	"path/filepath"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// This file is the CLI's credential wiring: one vault chain, one OAuth
// store, one refresh coordinator per invocation.
//
// Refresh serialization (ruling A.2 #10, docs/modules/oauth.md): the CLI takes the
// OFFLINE path — the <server>.refresh.lock sibling file lock plus the
// post-lock re-read of expires_at — even when a daemon is running.
//
// The design's online path ("daemon present ⇒ in-process singleflight is
// enough") is only sound while the daemon is the SOLE vault writer, which
// requires the CLI to delegate its refreshes over the control plane. That
// RPC does not exist yet, so the CLI writes the vault itself, and an
// in-process singleflight would protect nothing across the two processes.
// The failure directions are not symmetric: taking a lock that was not
// needed costs one syscall; skipping one that was needed spends a one-time
// refresh token twice and locks the user out.

// secretsDir resolves <data>/secrets, the directory holding secrets.enc,
// the keyring key registry and the per-server refresh locks.
func (a *App) secretsDir() (string, error) {
	data, err := a.resolver.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "secrets"), nil
}

// oauthDeps bundles the collaborators every auth subcommand needs.
type oauthDeps struct {
	chain       *secrets.Chain
	store       *oauthflow.Store
	oauthClient *oauthflow.Client
	coord       *oauthflow.Coordinator
	// dir is <data>/secrets, exposed so error messages can name the lock
	// file a stuck refresh is waiting on.
	dir string
}

// newOAuthDeps assembles the vault chain, the OAuth store and the refresh
// coordinator. allowLoopback relaxes the SSRF screen of the OAuth HTTP
// client to LITERAL loopback authorization servers (self-hosted providers
// and tests); it never unlocks RFC1918 or a DNS answer claiming to be
// local.
func (a *App) newOAuthDeps(allowLoopback bool) (*oauthDeps, error) {
	dir, err := a.secretsDir()
	if err != nil {
		return nil, err
	}
	chain := secrets.NewChain(secrets.ChainConfig{Dir: dir})
	store := oauthflow.NewStore(chain)
	client := oauthflow.NewClient(oauthflow.Config{AllowLoopback: allowLoopback})
	coord := oauthflow.NewCoordinator(oauthflow.CoordinatorConfig{
		Store:   store,
		Client:  client,
		LockDir: dir,
		// Online is deliberately nil: see the file comment.
	})
	return &oauthDeps{chain: chain, store: store, oauthClient: client, coord: coord, dir: dir}, nil
}

// tokenSource builds the downstream credential face for one server: the
// access token from the vault, renewal through the refresh coordinator.
func (d *oauthDeps) tokenSource(serverID string) downstream.TokenSource {
	return downstream.NewVaultTokenSource(serverID, d.chain.Resolver(),
		func(ctx context.Context) (string, error) {
			_, tok, err := d.coord.Refresh(ctx, serverID)
			// ErrRefreshSuperseded means another writer already stored a
			// fresh credential; the token it returns IS usable, so it is a
			// success for the caller, not a failure.
			if err != nil && !isSuperseded(err) {
				return "", err
			}
			return tok, nil
		})
}
