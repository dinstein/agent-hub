package confops

import (
	"context"
	"fmt"

	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/scope"
)

// ClientBinding is a PATCH of one client's persistent binding: which profile
// the client is on, and nothing else.
//
// It once also carried servers / tools / discovery narrowing applied on top
// of the profile. Those moved to the profile itself, so that binding a client
// is a complete answer to what that client sees rather than half of one.
type ClientBinding struct {
	// Profile sets the profile reference (named / followActive). A nil
	// pointer leaves the existing binding untouched.
	Profile *ProfileBindingSpec
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
// The same operation serves "create a binding" and "amend one": there is a
// single field, and supplying it is the whole edit.
func SetClientBinding(
	ctx context.Context, st *registry.Store, client string, b ClientBinding, pre Precondition,
) (ScopeResult, error) {
	if client == "" {
		return ScopeResult{}, usagef("a client id is required")
	}
	if b.Profile == nil {
		return ScopeResult{}, usagef("nothing to set")
	}
	if err := b.Profile.validate(); err != nil {
		return ScopeResult{}, err
	}

	var entry registry.ClientEntry
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		if tx.Clients.V.Clients == nil {
			tx.Clients.V.Clients = map[string]registry.Doc[registry.ClientEntry]{}
		}
		doc := tx.Clients.V.Clients[client]
		e := doc.V
		applyClientBinding(&e, b)
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
func applyClientBinding(entry *registry.ClientEntry, b ClientBinding) {
	ref := registry.Doc[registry.ProfileBinding]{
		V: registry.ProfileBinding{Kind: b.Profile.Kind, Name: b.Profile.Name},
	}
	entry.ProfileRef = &ref
	// The shorthand is cleared so the explicit form is the only truth;
	// keeping both would leave two spellings that can disagree.
	entry.Profile = ""
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
			e.Hint = "run 'agenthub client ls' to see current bindings"
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
