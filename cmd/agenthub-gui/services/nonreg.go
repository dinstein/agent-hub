package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dinstein/agent-hub/api"
)

// Surfaces whose subject is NOT the registry: the credential vault, the
// skills library, the agent-token store, AI-client configuration files and
// stored OAuth state.
//
// None of them takes an expectedGeneration, and that is a statement rather
// than an omission: there is no shared document here to lose a
// compare-and-swap against, so a page must not offer a "reload, someone else
// changed it" affordance on these — it would be describing a race that
// cannot happen. Each store serializes its own writes with its own lock.
//
// Audit has a mixed read/write lifecycle and lives in calls.go.

// secretRedacted replaces any message that would have carried a credential
// to the frontend. It says what happened and nothing else.
const secretRedacted = "the credential vault refused the write " +
	"(details withheld: the failure text contained the value)"

// ---------------------------------------------------------------------------
// Credential vault
// ---------------------------------------------------------------------------

// ListSecrets returns stored credential REFERENCES — server, scope, key and
// backend. A non-empty server narrows the listing.
//
// RED LINE (docs/subsystems/docs/subsystems/controlplane.md rule 5): there is no value, and not because this
// method declines to fill one in — api.SecretRef has no field one could
// occupy. Nothing downstream of here (page state, a rendered list, a
// screenshot) can contain a credential, because it never arrived. A value is
// verified by making a REAL call (TestServer), never by reading it back.
func (h *Hub) ListSecrets(ctx context.Context, server string) ([]api.SecretRef, error) {
	return call(ctx, h, func(c *api.Client) ([]api.SecretRef, error) {
		return c.Secrets.List(ctx, server)
	})
}

// SetSecret stores one credential. value is an ARGUMENT and appears nowhere
// else: not in the answer (api.SecretChange names the reference and says what
// happened), not in an emitted event — no write on this service emits one —
// and not in a log, because this package has no logger.
//
// A page should use a password input, clear it on submit, and offer no reveal
// toggle: there is nothing on this API to reveal it with.
//
// The error path is guarded too. The daemon already fixes its own failure
// text so a vault backend cannot echo the value through it, but this is the
// last hop before a human's screen and a clipboard, so a message that
// contains the value regardless is replaced wholesale here rather than
// forwarded. The machine-readable code survives — a page still branches
// correctly, it just cannot render the secret.
func (h *Hub) SetSecret(ctx context.Context, server, scope, key, value string) (api.SecretChange, error) {
	out, err := call(ctx, h, func(c *api.Client) (api.SecretChange, error) {
		return c.Secrets.Set(ctx, server, scope, key, value)
	})
	if err != nil {
		return api.SecretChange{}, redactSecret(err, value)
	}
	return out, nil
}

// DeleteSecret removes one credential. Deleting an absent one also reports
// success: the vault's delete is idempotent by contract.
func (h *Hub) DeleteSecret(ctx context.Context, server, scope, key string) (api.SecretChange, error) {
	return call(ctx, h, func(c *api.Client) (api.SecretChange, error) {
		return c.Secrets.Delete(ctx, server, scope, key)
	})
}

// redactSecret returns err unless its rendering contains value, in which case
// it returns an equivalent error carrying a fixed message.
//
// The comparison is on the FULL rendering (errors.Error(), i.e. every wrapped
// layer) because the value could be anywhere in the chain. Both branches
// preserve what a caller branches on: the api error keeps its code, status
// and request id, and an offline failure stays errors.Is(err, ErrOffline).
//
// Failure direction: a short value ("1") matches almost any message and
// over-redacts. That is the correct way to be wrong here — an operator loses
// a diagnostic, instead of a credential reaching a screenshot.
func redactSecret(err error, value string) error {
	if err == nil || value == "" || !strings.Contains(err.Error(), value) {
		return err
	}
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		clean := *apiErr
		clean.Message = secretRedacted
		if strings.Contains(clean.Hint, value) {
			clean.Hint = ""
		}
		return &clean
	}
	if errors.Is(err, ErrOffline) {
		return fmt.Errorf("%w: %s", ErrOffline, secretRedacted)
	}
	return errors.New(secretRedacted)
}

// ---------------------------------------------------------------------------
// Skills library
// ---------------------------------------------------------------------------

// ListSkillsForClient returns the library filtered to one client's install
// matrix; an empty client is the whole library (ListSkills).
func (h *Hub) ListSkillsForClient(ctx context.Context, client string) ([]api.Skill, error) {
	return call(ctx, h, func(c *api.Client) ([]api.Skill, error) {
		return c.Skills.ListForClient(ctx, client)
	})
}

// SetSkillEnabled flips one library skill's switch.
//
// The switch is COARSE: disabling does not unmaterialize anything already
// installed. The bytes stay on disk until a sync or an explicit removal
// converges the target, and the install receipts keep reporting them
// honestly — so a page must not present "disabled" as "removed".
func (h *Hub) SetSkillEnabled(ctx context.Context, id string, enabled bool) (api.Skill, error) {
	return call(ctx, h, func(c *api.Client) (api.Skill, error) {
		return c.Skills.SetEnabled(ctx, id, enabled)
	})
}

// PatchSkill edits a library skill. Enabled is the only editable field:
// content, fingerprint and the install matrix are derived from the library on
// disk, and writing them here would create a second source of truth.
func (h *Hub) PatchSkill(ctx context.Context, id string, patch api.SkillPatch) (api.Skill, error) {
	return call(ctx, h, func(c *api.Client) (api.Skill, error) {
		return c.Skills.Patch(ctx, id, patch)
	})
}

// InstallSkill materializes one skill into one client at one scope.
//
// A target edited outside agenthub refuses the write with a 409 unless the
// request sets AllowDrift. That 409 is NOT the optimistic-concurrency
// conflict (nothing here has a generation): re-reading fixes nothing, the
// user has to decide whether to overwrite their own edit. MarshalError keeps
// the two apart — only a stale precondition is stamped kind:"conflict".
func (h *Hub) InstallSkill(
	ctx context.Context, id string, req api.SkillInstallRequest,
) (api.SkillInstall, error) {
	return call(ctx, h, func(c *api.Client) (api.SkillInstall, error) {
		return c.Skills.Install(ctx, id, req)
	})
}

// ---------------------------------------------------------------------------
// Agent tokens
// ---------------------------------------------------------------------------

// ListTokens returns every stored agent token as metadata plus a 12-character
// prefix. There is no value and no hash: the plaintext is unrecoverable after
// creation, and the prefix identifies a row while revealing nothing usable.
func (h *Hub) ListTokens(ctx context.Context) ([]api.Token, error) {
	return call(ctx, h, func(c *api.Client) ([]api.Token, error) {
		return c.Tokens.List(ctx)
	})
}

// CreateToken mints one agent token. The answer is the ONLY place its value
// ever appears — the daemon keeps an HMAC and cannot print it again.
//
// A page must treat the value as write-only-to-the-user: show it once, offer
// a copy button, say plainly that closing the dialog loses it forever, and
// never persist it. It is deliberately not emitted as an event: the value
// reaches exactly the one call that asked for it.
func (h *Hub) CreateToken(ctx context.Context, spec api.TokenSpec) (api.TokenCreated, error) {
	return call(ctx, h, func(c *api.Client) (api.TokenCreated, error) {
		return c.Tokens.Create(ctx, spec)
	})
}

// RevokeToken withdraws one token. Revocation wins over expiry — it is the
// deliberate act.
func (h *Hub) RevokeToken(ctx context.Context, name string) (api.TokenRevoked, error) {
	return call(ctx, h, func(c *api.Client) (api.TokenRevoked, error) {
		return c.Tokens.Revoke(ctx, name)
	})
}

// ---------------------------------------------------------------------------
// AI-client configuration files
// ---------------------------------------------------------------------------

// DetectClients reports discovered AI-client configuration files as STATS
// ONLY — path, size, mtime, writability. Never content: on macOS, reading
// another application's configuration raises a privacy prompt, and a bulk
// scan that prompts a dozen times is worse than no scan at all.
//
// ClientDetected.Denied ("exists, may not be inspected") must be rendered
// differently from absent: the two call for opposite user actions.
func (h *Hub) DetectClients(ctx context.Context) (api.ClientDetectResult, error) {
	return call(ctx, h, func(c *api.Client) (api.ClientDetectResult, error) {
		return c.Clients.Detect(ctx)
	})
}

// InspectClient opens one named client's configuration and reports whether
// it contains an entry agenthub itself wrote. Unlike DetectClients this is a
// deliberate content read, so the frontend calls it only after an explicit
// per-row or "check connections" action.
func (h *Hub) InspectClient(ctx context.Context, client string) (api.ClientInspection, error) {
	return call(ctx, h, func(c *api.Client) (api.ClientInspection, error) {
		return c.Clients.Inspect(ctx, client)
	})
}

// ConnectClient writes the gateway entry into one client's configuration, or
// previews it when the request sets DryRun.
//
// The answer carries the Backup path taken before the rewrite — the
// operator's undo, which a page should surface rather than drop. Changed is
// false on an idempotent re-connect; that is not a failure.
func (h *Hub) ConnectClient(
	ctx context.Context, client string, req api.ClientConnectRequest,
) (api.ClientConnection, error) {
	return call(ctx, h, func(c *api.Client) (api.ClientConnection, error) {
		return c.Clients.Connect(ctx, client, req)
	})
}

// DisconnectClient removes the entries agenthub itself wrote. Ownership
// decides, never the name alone, so a hand-written server that happens to
// share a name survives.
func (h *Hub) DisconnectClient(ctx context.Context, client string) (api.ClientDisconnected, error) {
	return call(ctx, h, func(c *api.Client) (api.ClientDisconnected, error) {
		return c.Clients.Disconnect(ctx, client)
	})
}

// ---------------------------------------------------------------------------
// Stored OAuth credentials
// ---------------------------------------------------------------------------

// AuthStatus reports one server's authorization state, or every server that
// has stored credentials when server is "".
//
// No token, no client secret, no refresh token: HasRefreshToken is a boolean.
// There is no reveal escape hatch on this API, and an ExpiresAt of 0 means
// the provider advertised no expiry — "never expires", NOT "expired"
// (docs/status/oauth.md).
func (h *Hub) AuthStatus(ctx context.Context, server string) ([]api.AuthStatus, error) {
	return call(ctx, h, func(c *api.Client) ([]api.AuthStatus, error) {
		return c.Auth.Status(ctx, server)
	})
}

// AuthRefresh renews one server's access token.
//
// AuthRefreshed.Superseded reports that another writer refreshed first and
// this call adopted its result: a SUCCESS with a different provenance, not a
// race lost. Refreshing anyway would burn the refresh token the other writer
// just stored.
func (h *Hub) AuthRefresh(ctx context.Context, server string) (api.AuthRefreshed, error) {
	return call(ctx, h, func(c *api.Client) (api.AuthRefreshed, error) {
		return c.Auth.Refresh(ctx, server)
	})
}

// AuthLogout drops the locally stored credential for one server. It does not
// revoke anything at the provider — a page must say so, or "logged out" reads
// as a guarantee agenthub cannot make.
func (h *Hub) AuthLogout(ctx context.Context, server string) (api.AuthLoggedOut, error) {
	return call(ctx, h, func(c *api.Client) (api.AuthLoggedOut, error) {
		return c.Auth.Logout(ctx, server)
	})
}

// AuthLoginStart begins an interactive login and returns before there is
// anything to show: picking a mode needs the authorization server's metadata.
// The frontend polls AuthLogin until the session is actionable.
//
// THE FRONTEND OPENS THE BROWSER, not this process and not the daemon. The
// daemon may be headless; this window is where the user actually is. Three
// separate calls rather than one composite, because a composite the CLI
// cannot perform would be a GUI privilege — the property bridge.ts exists to
// keep true.
func (h *Hub) AuthLoginStart(ctx context.Context, server string) (api.AuthLogin, error) {
	return call(ctx, h, func(c *api.Client) (api.AuthLogin, error) {
		return c.Auth.StartLogin(ctx, server)
	})
}

// AuthLogin reads one login session. A session that FAILED is a successful
// read: the phase says so and the reason is on the struct. Only an id naming
// no session at all is an error.
func (h *Hub) AuthLogin(ctx context.Context, id string) (api.AuthLogin, error) {
	return call(ctx, h, func(c *api.Client) (api.AuthLogin, error) {
		return c.Auth.Login(ctx, id)
	})
}

// AuthLoginCancel abandons a running login. It stops the WAIT, not the
// authorization: a consent already granted at the provider stays granted, and
// a login that had already stored a credential keeps it.
func (h *Hub) AuthLoginCancel(ctx context.Context, id string) (api.AuthLogin, error) {
	return call(ctx, h, func(c *api.Client) (api.AuthLogin, error) {
		return c.Auth.CancelLogin(ctx, id)
	})
}
