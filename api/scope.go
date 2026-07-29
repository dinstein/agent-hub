package api

import (
	"context"
	"net/http"
	"net/url"
)

// Profile binding kinds (docs/architecture.md §7). They replace the `"profile": ""`
// magic value some proxies use: "no profile" is spelled followActive, never
// an empty name — an empty name is a typo, and a typo must not resolve.
const (
	// BindingNamed references a profile by name.
	BindingNamed = "named"
	// BindingFollowActive follows the global active profile.
	BindingFollowActive = "followActive"
)

// ProfileBinding is the explicit profile reference of a client binding.
type ProfileBinding struct {
	// Kind is one of the Binding* constants.
	Kind string `json:"kind"`
	// Name is required iff Kind is BindingNamed.
	Name string `json:"name,omitempty"`
}

// ClientEntry is one client's STORED binding — the CLIENT layer of the
// three-layer scope chain, in the registry document's own shape.
//
// CONTRACT: field names mirror internal/registry.ClientEntry (camelCase).
// Profile is the shorthand for {kind:"named"}; an explicit ProfileRef wins
// over it when both are present, which is why an edit sets one and clears
// the other rather than leaving two spellings that can disagree.
type ClientEntry struct {
	Profile    string          `json:"profile,omitempty"`
	ProfileRef *ProfileBinding `json:"profileRef,omitempty"`
	Discovery  string          `json:"discovery,omitempty"`
	// Servers is the three-state narrowing set: absent = no narrowing,
	// [] = block-all, [...] = exactly those. `omitzero` keeps the empty
	// list distinguishable from the absent one after a decode.
	Servers []string `json:"servers,omitzero"`
	// Tools maps a server id to its stored selector.
	Tools map[string]ToolSelector `json:"tools,omitempty"`
}

// Binding resolves the effective profile reference, applying the rule that
// an explicit ProfileRef wins over the Profile shorthand and that an absent
// binding means "follow the active profile".
func (e ClientEntry) Binding() ProfileBinding {
	if e.ProfileRef != nil {
		return *e.ProfileRef
	}
	if e.Profile != "" {
		return ProfileBinding{Kind: BindingNamed, Name: e.Profile}
	}
	return ProfileBinding{Kind: BindingFollowActive}
}

// ClientBinding is an EDIT of one client's persistent binding: a nil/absent
// field is left untouched.
//
// Optional-by-pointer rather than "zero means unset" because every zero
// value here is meaningful: an empty server list is block-all and an empty
// discovery string is "inherit". A caller amending one field must not reset
// the rules it never mentioned.
type ClientBinding struct {
	// Profile sets the profile reference (named / followActive / inherit).
	Profile *ProfileBinding `json:"profile,omitempty"`
	// Servers is the three-state narrowing set, and the pointer is
	// load-bearing:
	//
	//	nil pointer  -> leave the rule alone
	//	&[]string{}  -> block-all (encodes as "servers": [])
	//	&[...]       -> exactly those servers
	//
	// A plain []string with omitempty would encode "leave alone" and
	// "block-all" identically — the fail-open collapse this API refuses to
	// make representable.
	Servers *[]string `json:"servers,omitempty"`
	// Tools applies one three-state selector per server id.
	Tools map[string]ProfileTools `json:"tools,omitempty"`
	// Discovery overrides the discovery mode ("lazy", "grouped", "full").
	// A pointer to "" clears the override; a nil pointer leaves it.
	Discovery *string `json:"discovery,omitempty"`
}

// ScopeDetail is one client's stored binding plus the generation it was read
// at — the read half of a read-modify-write, for the same reason
// ServerDetail carries one.
type ScopeDetail struct {
	Generation uint64 `json:"generation"`
	Client     string `json:"client"`
	// Exists is false when this client has no binding at all. Entry is then
	// the zero value, which is NOT the same thing as an empty binding: the
	// former follows the active profile, the latter is a stored rule.
	Exists bool        `json:"exists"`
	Entry  ClientEntry `json:"entry,omitzero"`
	// Dangling reports that the binding names a profile that does not
	// exist. Such a client resolves to an EMPTY scope (fail-closed), which
	// must be shown as a fault, not as an empty tool list.
	Dangling bool `json:"dangling,omitempty"`
	// DanglingProfile is the missing profile name when Dangling is set.
	DanglingProfile string `json:"dangling_profile,omitempty"`
}

// ScopeWrite is the answer to a client-binding mutation.
type ScopeWrite struct {
	WriteResult
	Client string      `json:"client"`
	Entry  ClientEntry `json:"entry,omitzero"`
	// Exists is false after a clear.
	Exists bool `json:"exists"`
	// Dangling reports that the RESULTING binding names a profile that does
	// not exist — reported loudly rather than shown as a silently empty
	// tool list.
	Dangling        bool   `json:"dangling,omitempty"`
	DanglingProfile string `json:"dangling_profile,omitempty"`
	// Cleared reports that the binding was removed.
	Cleared bool `json:"cleared,omitempty"`
}

// ScopeService manages the persistent per-client scope binding.
//
// This is the CONFIGURATION layer, not the session overlay: it is persisted,
// it may widen as well as narrow (it is an operator surface), and it takes
// effect for every future session of that client. The agent-reachable,
// narrow-only, volatile overlay is Sessions.SetScope — a different surface
// on purpose (ruling #8).
type ScopeService struct{ c *Client }

// Get returns one client's persistent binding.
func (s *ScopeService) Get(ctx context.Context, client string) (ScopeDetail, error) {
	var out ScopeDetail
	err := s.c.do(ctx, http.MethodGet, "/scope/"+url.PathEscape(client), nil, nil, &out)
	return out, err
}

// Set creates or amends one client's binding. Every server id named — in the
// narrowing set or in a tool selector — must exist: a rule about a server
// nobody registered narrows nothing, and accepting it would be a rule that
// silently does not apply.
func (s *ScopeService) Set(
	ctx context.Context, client string, b ClientBinding, expectedGeneration uint64,
) (ScopeWrite, error) {
	var out ScopeWrite
	err := s.c.doWrite(ctx, http.MethodPut, "/scope/"+url.PathEscape(client), nil, expectedGeneration, b, &out)
	return out, err
}

// Clear removes a client's binding entirely; the client falls back to the
// active profile.
func (s *ScopeService) Clear(ctx context.Context, client string, expectedGeneration uint64) (ScopeWrite, error) {
	var out ScopeWrite
	err := s.c.doWrite(ctx, http.MethodDelete, "/scope/"+url.PathEscape(client), nil, expectedGeneration, nil, &out)
	return out, err
}
