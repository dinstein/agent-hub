package confops

import (
	"context"
	"fmt"

	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/scope"
)

// ClientBinding is a PATCH of one client's persistent scope binding (the
// CLIENT layer of the four-layer chain): every field is optional, and a nil
// field is left untouched.
//
// Optional-by-pointer rather than "zero means unset" because the zero values
// are all meaningful here — an empty server list is block-all, an empty
// discovery string is "inherit". A caller amending one field must not reset
// the rules it never mentioned.
type ClientBinding struct {
	// Profile sets the profile reference (named / followActive / inherit).
	Profile *ProfileBindingSpec
	// Servers replaces the three-state narrowing set: nil pointer = leave
	// alone, pointer to nil or empty slice = block-all, otherwise that set.
	Servers *[]string
	// Tools applies one three-state selector per server id.
	Tools map[string]ToolSelection
	// Discovery overrides the discovery mode for this client.
	Discovery *string
}

// ProfileBindingSpec is the explicit profile reference. It replaces
// toolport's `"profile": ""` magic value: "no profile" is spelled
// followActive, never an empty name.
type ProfileBindingSpec struct {
	Kind registry.ProfileBindingKind
	Name string
}

func (s ProfileBindingSpec) validate() error {
	switch s.Kind {
	case registry.BindingNamed:
		if s.Name == "" {
			return usagef("a named profile binding needs a profile name")
		}
		return nil
	case registry.BindingFollowActive:
		return nil
	default:
		return usagef("unknown profile binding kind %q (want named or followActive)", s.Kind)
	}
}

// ScopeResult is what the client-layer operations return.
type ScopeResult struct {
	Result
	Client string
	// Entry is the binding as it now stands; Exists is false after a clear.
	Entry  registry.ClientEntry
	Exists bool
	// Dangling reports that the resulting binding names a profile that does
	// not exist. The resolver fail-closes such a client to an EMPTY scope,
	// so this is reported loudly rather than shown as a silent empty set.
	Dangling bool
	// DanglingProfile is the missing profile name when Dangling is set.
	DanglingProfile string
}

// SetClientBinding creates or amends one client's persistent binding.
//
// Only the fields the caller filled in are applied, so the same operation
// serves "create a binding" and "amend one". Every server id named — in the
// narrowing set or in a tool selector — must exist in the registry: a rule
// about a server nobody registered narrows nothing, and accepting it would
// be a rule that silently does not apply.
func SetClientBinding(
	ctx context.Context, st *registry.Store, client string, b ClientBinding, pre Precondition,
) (ScopeResult, error) {
	if client == "" {
		return ScopeResult{}, usagef("a client id is required")
	}
	if b.Profile == nil && b.Servers == nil && b.Tools == nil && b.Discovery == nil {
		return ScopeResult{}, usagef("nothing to set")
	}
	if b.Profile != nil {
		if err := b.Profile.validate(); err != nil {
			return ScopeResult{}, err
		}
	}
	if b.Discovery != nil && *b.Discovery != "" {
		if err := ValidateDiscovery(*b.Discovery); err != nil {
			return ScopeResult{}, err
		}
	}
	for _, id := range sortedKeys(b.Tools) {
		if err := b.Tools[id].validate(); err != nil {
			return ScopeResult{}, err
		}
	}

	var entry registry.ClientEntry
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		if tx.Clients.V.Clients == nil {
			tx.Clients.V.Clients = map[string]registry.Doc[registry.ClientEntry]{}
		}
		doc := tx.Clients.V.Clients[client]
		e := doc.V
		if err := applyClientBinding(tx, &e, b); err != nil {
			return err
		}
		doc.V = e
		tx.Clients.V.Clients[client] = doc
		entry = e
		return nil
	})
	if err != nil {
		return ScopeResult{Result: res}, err
	}
	out := ScopeResult{Result: res, Client: client, Entry: entry, Exists: true}
	if bind := entry.Binding(); bind.Kind == registry.BindingNamed {
		if _, ok := st.Snapshot().Profiles.V.Profiles[bind.Name]; !ok {
			out.Dangling, out.DanglingProfile = true, bind.Name
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"profile %q does not exist; this client now resolves to an EMPTY scope (fail-closed)", bind.Name))
		}
	}
	return out, nil
}

// applyClientBinding folds the patch into an entry, under the lock.
func applyClientBinding(tx *registry.Tx, entry *registry.ClientEntry, b ClientBinding) error {
	if b.Profile != nil {
		ref := registry.Doc[registry.ProfileBinding]{
			V: registry.ProfileBinding{Kind: b.Profile.Kind, Name: b.Profile.Name},
		}
		entry.ProfileRef = &ref
		// The shorthand is cleared so the explicit form is the only truth;
		// keeping both would leave two spellings that can disagree.
		entry.Profile = ""
	}
	if b.Servers != nil {
		list := dedupSorted(*b.Servers)
		if list == nil {
			list = []string{}
		}
		for _, id := range list {
			if _, ok := tx.Servers.V.Servers[id]; !ok {
				return serverNotFound(id)
			}
		}
		entry.Servers = list
	}
	if b.Tools != nil {
		if entry.Tools == nil {
			entry.Tools = map[string]registry.Doc[registry.ToolSelector]{}
		}
		for _, id := range sortedKeys(b.Tools) {
			if _, ok := tx.Servers.V.Servers[id]; !ok {
				return serverNotFound(id)
			}
			applySelector(entry.Tools, id, b.Tools[id])
		}
		if len(entry.Tools) == 0 {
			entry.Tools = nil
		}
	}
	if b.Discovery != nil {
		entry.Discovery = *b.Discovery
	}
	return nil
}

// ClearClientBinding removes a client's persistent binding entirely; the
// client falls back to the active profile.
func ClearClientBinding(
	ctx context.Context, st *registry.Store, client string, pre Precondition,
) (ScopeResult, error) {
	if client == "" {
		return ScopeResult{}, usagef("a client id is required")
	}
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		if _, ok := tx.Clients.V.Clients[client]; !ok {
			e := notFoundf(CodeNotFound, "no scope binding for client %q", client)
			e.Hint = "run 'agenthub scope ls' to see current bindings"
			return e
		}
		delete(tx.Clients.V.Clients, client)
		return nil
	})
	if err != nil {
		return ScopeResult{Result: res}, err
	}
	return ScopeResult{Result: res, Client: client}, nil
}

// ValidateDiscovery rejects an unknown discovery mode at the moment the
// operator can still fix it, instead of letting the resolver silently fall
// back to a default nobody asked for.
func ValidateDiscovery(mode string) error {
	switch scope.DiscoveryMode(mode) {
	case scope.DiscoveryLazy, scope.DiscoveryGrouped, scope.DiscoveryFull:
		return nil
	}
	return usagef("unknown discovery mode %q (want lazy, grouped or full)", mode)
}
