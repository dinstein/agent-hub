package services

import (
	"context"

	"github.com/dinstein/agent-hub/api"
)

// Registry-backed configuration: servers, profiles, client bindings and the
// governance switches.
//
// Every write here takes an expectedGeneration and every write ANSWERS with
// the generation the registry now stands at (api.WriteResult.Generation),
// which is the whole shape of the optimistic-concurrency contract
// (docs/flows.md#config-writes):
//
//	read  -> Generation G
//	write -> expectedGeneration G, answers Generation G'
//	next  -> expectedGeneration G'
//
// A page performing a burst of edits therefore never has to re-read between
// them, and never has to poll to learn where it now stands. Passing 0 skips
// the check — the non-interactive spelling, and NOT what a GUI form should
// send: a long-lived window's data is minutes old by the time a human hits
// save, which is exactly the case the check exists for.
//
// When the check loses, the api client raises *api.ConflictError, nothing
// was written, and MarshalError hands the page kind:"conflict" plus the
// current generation. The correct response is re-read, re-apply the user's
// intent, retry — never a blind retry with the same body.
//
// Event coverage, honestly stated (see the package report and
// docs/modules/controlplane.md): only the `servers` document has an SSE
// topic. A write to any OTHER document bumps the generation but pushes
// nothing, so the generation in the write answer is how those pages learn
// where they stand. That is a self-update from the answer, not a poll.

// ---------------------------------------------------------------------------
// Servers
// ---------------------------------------------------------------------------

// GetServer returns one server's STORED definition together with the
// generation it was read at — the read half of a read-modify-write.
//
// An edit form must open with this and not with ListServers: the list is the
// runtime view with the Health contract, and taking the entry from one call
// and the generation from another would reopen precisely the window the
// precondition closes.
func (h *Hub) GetServer(ctx context.Context, id string) (api.ServerDetail, error) {
	return call(ctx, h, func(c *api.Client) (api.ServerDetail, error) {
		return c.Servers.Get(ctx, id)
	})
}

// CreateServer adds a server definition.
func (h *Hub) CreateServer(
	ctx context.Context, spec api.ServerSpec, expectedGeneration uint64,
) (api.ServerWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ServerWrite, error) {
		return c.Servers.Create(ctx, spec, expectedGeneration)
	})
}

// UpdateServer replaces one server's definition WHOLESALE: every field of
// spec.Entry is written, so a form must submit the complete entry it opened
// with (GetServer) and not just the fields the user touched.
func (h *Hub) UpdateServer(
	ctx context.Context, spec api.ServerSpec, expectedGeneration uint64,
) (api.ServerWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ServerWrite, error) {
		return c.Servers.Update(ctx, spec, expectedGeneration)
	})
}

// DeleteServer removes a server definition.
func (h *Hub) DeleteServer(
	ctx context.Context, id string, expectedGeneration uint64,
) (api.ServerWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ServerWrite, error) {
		return c.Servers.Delete(ctx, id, expectedGeneration)
	})
}

// SetServerEnabled flips one server's enable flag without a round trip
// through a complete definition — the one entry field that is independent of
// the transport shape.
func (h *Hub) SetServerEnabled(
	ctx context.Context, id string, enabled bool, expectedGeneration uint64,
) (api.ServerWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ServerWrite, error) {
		return c.Servers.SetEnabled(ctx, id, enabled, expectedGeneration)
	})
}

// TestServer connects to a configured server and reports what it finds.
//
// This is how a credential is verified (docs/modules/controlplane.md rule 5): by making a
// REAL call. Nothing on this path can print a secret back — api's result
// type has no field one could be assigned to. It is a read, not a write, so
// it carries no precondition.
func (h *Hub) TestServer(
	ctx context.Context, id string, req api.ServerTestRequest,
) (api.ServerTestResult, error) {
	return call(ctx, h, func(c *api.Client) (api.ServerTestResult, error) {
		return c.Servers.Test(ctx, id, req)
	})
}

// ---------------------------------------------------------------------------
// Profiles
// ---------------------------------------------------------------------------

// ListProfiles returns every profile, the active marker and the generation
// the following write sends back.
func (h *Hub) ListProfiles(ctx context.Context) (api.ProfileList, error) {
	return call(ctx, h, func(c *api.Client) (api.ProfileList, error) {
		return c.Profiles.List(ctx)
	})
}

// CreateProfile creates a profile with an optional initial membership.
//
// servers is a POINTER because its three states are three different
// profiles: nil is "no narrowing" (every registered server), a pointer to an
// empty slice is block-all, and a pointer to a list is exactly those. A form
// that has not asked the question yet sends nil, never an empty slice — the
// two differ by everything.
func (h *Hub) CreateProfile(
	ctx context.Context, name string, servers *[]string, expectedGeneration uint64,
) (api.ProfileWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ProfileWrite, error) {
		return c.Profiles.Create(ctx, name, servers, expectedGeneration)
	})
}

// RenameProfile renames a profile AND repoints every client and project
// reference at it; the answer lists what was repointed. It is one operation,
// not delete-then-create: the latter would fail-close every referencing
// client to an EMPTY scope in between.
func (h *Hub) RenameProfile(
	ctx context.Context, name, newName string, expectedGeneration uint64,
) (api.ProfileWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ProfileWrite, error) {
		return c.Profiles.Rename(ctx, name, newName, expectedGeneration)
	})
}

// SetProfileServers edits a profile's member-server set (replace/add/remove).
//
// add and remove are resolved under the registry lock rather than by the
// caller computing a new list from what it last saw, so two concurrent
// "add one server" edits both land instead of one silently erasing the other.
func (h *Hub) SetProfileServers(
	ctx context.Context, name string, edit api.ServerSetEdit, expectedGeneration uint64,
) (api.ProfileWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ProfileWrite, error) {
		return c.Profiles.SetServers(ctx, name, edit, expectedGeneration)
	})
}

// SetProfileTools sets one server's three-state tool selector inside a
// profile (all / only these / none).
func (h *Hub) SetProfileTools(
	ctx context.Context, name, server string, sel api.ProfileTools, expectedGeneration uint64,
) (api.ProfileWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ProfileWrite, error) {
		return c.Profiles.SetTools(ctx, name, server, sel, expectedGeneration)
	})
}

// DeleteProfile removes a profile. Clients still pointing at it are NOT
// rewritten — they resolve to an empty scope (fail-closed) and come back in
// ProfileWrite.Dangling, which a page must surface rather than swallow.
func (h *Hub) DeleteProfile(
	ctx context.Context, name string, expectedGeneration uint64,
) (api.ProfileWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ProfileWrite, error) {
		return c.Profiles.Delete(ctx, name, expectedGeneration)
	})
}

// SetActiveProfile points the global active marker at this profile.
func (h *Hub) SetActiveProfile(
	ctx context.Context, name string, expectedGeneration uint64,
) (api.ProfileWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ProfileWrite, error) {
		return c.Profiles.SetActive(ctx, name, expectedGeneration)
	})
}

// ClearActiveProfile clears the global active marker. It is a separate
// method rather than SetActiveProfile("") because clearing is stated against
// the profile that HOLDS the marker — there is no "some other profile" for
// the daemon to guess at.
func (h *Hub) ClearActiveProfile(
	ctx context.Context, name string, expectedGeneration uint64,
) (api.ProfileWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ProfileWrite, error) {
		return c.Profiles.ClearActive(ctx, name, expectedGeneration)
	})
}

// ---------------------------------------------------------------------------
// Client scope bindings
// ---------------------------------------------------------------------------

// GetScope returns one client's stored binding plus the generation it was
// read at.
//
// ScopeDetail.Exists tells "this client has no binding" (it follows the
// active profile) from "this client has an empty binding" (a stored rule
// that grants nothing). A page that folds the two together would show a
// deliberately locked-down client as unconfigured.
func (h *Hub) GetScope(ctx context.Context, client string) (api.ScopeDetail, error) {
	return call(ctx, h, func(c *api.Client) (api.ScopeDetail, error) {
		return c.Scope.Get(ctx, client)
	})
}

// SetScope creates or amends one client's persistent binding. Absent fields
// are left untouched, so amending the profile reference cannot silently drop
// the server narrowing.
//
// This is the CONFIGURATION layer and it may widen as well as narrow: it is
// an operator surface. The agent-reachable overlay is SetSessionScope, which
// is a different surface on purpose.
func (h *Hub) SetScope(
	ctx context.Context, client string, binding api.ClientBinding, expectedGeneration uint64,
) (api.ScopeWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ScopeWrite, error) {
		return c.Scope.Set(ctx, client, binding, expectedGeneration)
	})
}

// ClearScope removes one client's binding entirely, returning it to the
// active profile. It is not "bind to nothing" — see GetScope.
func (h *Hub) ClearScope(
	ctx context.Context, client string, expectedGeneration uint64,
) (api.ScopeWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ScopeWrite, error) {
		return c.Scope.Clear(ctx, client, expectedGeneration)
	})
}

// ---------------------------------------------------------------------------
// Governance switches
// ---------------------------------------------------------------------------

// ConfigKeys returns every governance key with its current value and the
// generation the following write sends back.
//
// Values are STRINGS for every kind — the same rendering the CLI prints — so
// the two surfaces cannot disagree about what a switch currently says.
func (h *Hub) ConfigKeys(ctx context.Context) (api.GovernanceList, error) {
	return call(ctx, h, func(c *api.Client) (api.GovernanceList, error) {
		return c.Config.Keys(ctx)
	})
}

// SetConfig writes one governance key, answering with the previous value so
// a page can render the transition.
//
// These switches merge tighten-only downward: no lower layer can undo one,
// which makes this the ONLY place a safety gate can be relaxed. A page must
// therefore treat relaxing one (api.GovernanceKey.Safety) as a distinct,
// loudly-marked action rather than as another toggle. That property is
// derived client-side on purpose, so the warning still fires against a
// daemon that does not label its keys — the loud path must not be the one
// that can go quiet.
func (h *Hub) SetConfig(
	ctx context.Context, key, value string, expectedGeneration uint64,
) (api.ConfigWrite, error) {
	return call(ctx, h, func(c *api.Client) (api.ConfigWrite, error) {
		return c.Config.Set(ctx, key, value, expectedGeneration)
	})
}
