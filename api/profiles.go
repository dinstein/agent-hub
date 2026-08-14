package api

import (
	"context"
	"net/http"
	"net/url"
)

// Tool selector modes (docs/model.md). The three states are keyed by
// ORIGINAL tool names — never exposed/renamed names, otherwise a rename
// would walk out from under its own narrowing rule.
//
// WHY AN EXPLICIT MODE AND NOT A BARE LIST: the three states are "every
// tool", "exactly these" and "no tool at all", and the last two differ ONLY
// by an empty vs non-empty list. Any encoding in which a missing field and
// an empty list can be confused collapses block-all into allow-all the first
// time a marshaller drops an empty slice, an HTTP layer normalizes a body or
// a hand-written client forgets the field. Spelling the state out makes that
// class of bug unrepresentable: a caller who omits Mode sends "" and is
// REFUSED, instead of silently opening a server up.
const (
	// ToolSelectAll exposes the server's full tool set. At a single layer
	// that is indistinguishable from "no rule", so the rule is dropped
	// rather than stored as an inert object.
	ToolSelectAll = "all"
	// ToolSelectOnly narrows to the named subset (at least one name).
	ToolSelectOnly = "only"
	// ToolSelectNone blocks every tool of the server: the EMPTY allow list.
	// It is stored, not dropped — dropping it would flip block-all into
	// allow-all.
	ToolSelectNone = "none"
)

// ProfileTools is one three-state tool selector EDIT.
//
// Failure direction: an empty Mode is the zero value and is REFUSED by the
// daemon. Making the zero value mean ToolSelectAll would let a caller that
// forgot the field silently widen a selector; unset must never be the loose
// case.
type ProfileTools struct {
	// Mode is one of the ToolSelect* constants. Required — no omitempty, so
	// a forgotten mode reaches the daemon as "" and is refused there rather
	// than being guessed here.
	Mode string `json:"mode"`
	// Tools are the RAW downstream tool names, required by ToolSelectOnly
	// and ignored otherwise. An empty Tools with mode "only" is NOT how
	// "none" is expressed: it is a caller bug and is refused, because
	// guessing would pick the fail-open reading.
	Tools []string `json:"tools,omitempty"`
}

// AllTools drops the tool rule for a server (its full set is exposed).
func AllTools() ProfileTools { return ProfileTools{Mode: ToolSelectAll} }

// OnlyTools narrows a server to the named tools.
func OnlyTools(tools ...string) ProfileTools {
	return ProfileTools{Mode: ToolSelectOnly, Tools: tools}
}

// NoTools blocks every tool of a server. Using this constructor rather than
// an empty subset is what keeps block-all spellable and unmistakable.
func NoTools() ProfileTools { return ProfileTools{Mode: ToolSelectNone} }

// ToolSelector is a STORED three-state selector as read back. It is the
// registry's own shape, so the same nil-vs-empty distinction the daemon
// keeps on disk survives the round trip:
//
//	Allow == nil  -> the server's full tool set (no rule)
//	Allow == []   -> block-all
//	Allow == [..] -> exactly those tools
//
// `omitzero` (not omitempty) is load-bearing: it keeps the empty list on the
// wire, and dropping it would silently turn block-all into allow-all.
type ToolSelector struct {
	Allow []string `json:"allow,omitzero"`
}

// Blocked reports the block-all state: an allow list that is present and
// empty. It exists so a frontend never has to write the nil-vs-empty test
// itself — the one test whose wrong answer opens a server up.
func (s ToolSelector) Blocked() bool { return s.Allow != nil && len(s.Allow) == 0 }

// Profile is one named tier of the scope chain as READ from the daemon.
type Profile struct {
	Name string `json:"name"`
	// Servers is the three-state member set, with the same rule as
	// ToolSelector.Allow: absent = no narrowing (every registered server),
	// [] = block-all, [...] = exactly those. `omitzero` keeps the empty
	// list distinguishable from the absent one after a decode.
	Servers []string `json:"servers,omitzero"`
	// Tools maps a server id to its stored selector.
	Tools map[string]ToolSelector `json:"tools,omitempty"`
}

// Blocked reports that this profile's member set is present and empty, i.e.
// it exposes no server at all.
func (p Profile) Blocked() bool { return p.Servers != nil && len(p.Servers) == 0 }

// ProfileList is the answer to Profiles.List.
type ProfileList struct {
	// Generation is what the following write sends as expectedGeneration.
	Generation uint64    `json:"generation"`
	Profiles   []Profile `json:"profiles"`
	// Active is the globally active profile ("" = none).
	Active string `json:"active"`
	// ActiveKnown is false when this daemon cannot answer the question at
	// all (no state directory). A frontend must tell "there is no active
	// profile" from "this daemon does not know" — the two look identical in
	// Active alone.
	ActiveKnown bool `json:"active_known"`
}

// Server-set edit modes for ServerSetEdit.Mode.
const (
	// ServerSetReplace sets the list outright: null clears the narrowing
	// (every registered server), [] is block-all.
	ServerSetReplace = "replace"
	// ServerSetAdd adds ids. A profile with NO narrowing becomes an
	// explicit set the moment one server is named: "these and only these".
	ServerSetAdd = "add"
	// ServerSetRemove drops ids from the list.
	ServerSetRemove = "remove"
)

// ServerSetEdit is one edit of a profile's member-server set. The
// read-modify-write for add/remove happens under the registry lock, so a
// concurrent edit cannot be lost the way a caller computing the new list
// from a stale snapshot would lose it.
type ServerSetEdit struct {
	// Mode is one of the ServerSet* constants. Required; "" is refused
	// rather than defaulted.
	Mode string `json:"mode"`
	// Servers is the id list the mode applies to. Under ServerSetReplace an
	// explicit empty list is block-all and a null is "no narrowing", so this
	// field has no omitempty: the empty list has to survive the wire.
	Servers []string `json:"servers"`
}

// ProfileToolsEdit scopes one three-state selector to one server, which is
// exactly the shape of the operation behind it.
type ProfileToolsEdit struct {
	Server string `json:"server"`
	ProfileTools
}

// ProfilePatch is an edit of one profile. EXACTLY ONE field may be set: they
// are separate operations with no transaction spanning them, so a combined
// request could half-apply. The daemon refuses anything else.
type ProfilePatch struct {
	// Rename renames the profile AND repoints every client and project
	// reference to it. A rename is an operation, not a delete-then-create:
	// leaving references behind would fail-close every one of those clients
	// to an EMPTY scope.
	Rename string `json:"rename,omitempty"`
	// Servers edits the membership set.
	Servers *ServerSetEdit `json:"servers,omitempty"`
	// Tools sets one server's three-state tool selector.
	Tools *ProfileToolsEdit `json:"tools,omitempty"`
	// Active points the global active marker: true points it at THIS
	// profile, false clears it (there is no "some other profile" to guess
	// at). A nil pointer leaves the marker alone — which is why this is a
	// pointer: false must mean "clear", never "did not mention".
	Active *bool `json:"active,omitempty"`
}

// ProfileWrite is the answer to every profile mutation.
type ProfileWrite struct {
	WriteResult
	Name    string `json:"name"`
	OldName string `json:"old_name,omitempty"`
	// Profile is the entry as it now stands (absent after a delete).
	Profile *Profile `json:"profile,omitempty"`
	// Repointed lists the client ids a rename rewrote.
	Repointed []string `json:"repointed,omitempty"`
	// Dangling lists client ids left pointing at a REMOVED profile. They are
	// not rewritten (fail-closed: they now resolve to an EMPTY scope); they
	// are reported so a frontend can say so out loud.
	Dangling      []string `json:"dangling,omitempty"`
	ActiveCleared bool     `json:"active_cleared,omitempty"`
	Deleted       bool     `json:"deleted,omitempty"`
}

// profileCreateBody is the POST /v1/profiles body.
type profileCreateBody struct {
	Name string `json:"name"`
	// Servers is the initial membership. The pointer preserves the three
	// states: nil omits the key (no narrowing), &[]string{} sends [] which
	// is block-all. A plain slice with omitempty would encode those two
	// identically and collapse block-all into allow-all.
	Servers *[]string `json:"servers,omitempty"`
}

// ProfilesService manages the profile tier of the scope chain.
type ProfilesService struct{ c *Client }

// List returns every configured profile plus the active marker.
func (s *ProfilesService) List(ctx context.Context) (ProfileList, error) {
	var out ProfileList
	err := s.c.do(ctx, http.MethodGet, "/profiles", nil, nil, &out)
	return out, err
}

// Create adds a profile. servers keeps the three-state distinction: nil
// creates a profile that sees every registered server, a pointer to an empty
// slice creates a block-all one. The two are never collapsed.
func (s *ProfilesService) Create(
	ctx context.Context, name string, servers *[]string, expectedGeneration uint64,
) (ProfileWrite, error) {
	var out ProfileWrite
	err := s.c.doWrite(ctx, http.MethodPost, "/profiles", nil, expectedGeneration,
		profileCreateBody{Name: name, Servers: servers}, &out)
	return out, err
}

// Update applies one patch to one profile. Exactly one field of patch may be
// set.
func (s *ProfilesService) Update(
	ctx context.Context, name string, patch ProfilePatch, expectedGeneration uint64,
) (ProfileWrite, error) {
	var out ProfileWrite
	err := s.c.doWrite(ctx, http.MethodPatch, "/profiles/"+url.PathEscape(name), nil,
		expectedGeneration, patch, &out)
	return out, err
}

// Rename renames a profile and repoints every reference to it.
func (s *ProfilesService) Rename(
	ctx context.Context, name, newName string, expectedGeneration uint64,
) (ProfileWrite, error) {
	return s.Update(ctx, name, ProfilePatch{Rename: newName}, expectedGeneration)
}

// SetServers edits a profile's member-server set.
func (s *ProfilesService) SetServers(
	ctx context.Context, name string, edit ServerSetEdit, expectedGeneration uint64,
) (ProfileWrite, error) {
	return s.Update(ctx, name, ProfilePatch{Servers: &edit}, expectedGeneration)
}

// SetTools sets one server's three-state tool selector inside a profile.
// Both the profile and the server must exist: a selector naming a server the
// registry does not know narrows nothing, so accepting it would be a rule
// that silently does not apply.
func (s *ProfilesService) SetTools(
	ctx context.Context, name, server string, sel ProfileTools, expectedGeneration uint64,
) (ProfileWrite, error) {
	return s.Update(ctx, name,
		ProfilePatch{Tools: &ProfileToolsEdit{Server: server, ProfileTools: sel}}, expectedGeneration)
}

// Delete removes a profile.
//
// Referencing clients are deliberately NOT rewritten: a dangling reference
// resolves to an EMPTY scope, never a widened one. They come back in
// ProfileWrite.Dangling and in Warnings, because "this client just lost every
// tool" is not something an operator may learn by accident.
func (s *ProfilesService) Delete(
	ctx context.Context, name string, expectedGeneration uint64,
) (ProfileWrite, error) {
	var out ProfileWrite
	err := s.c.doWrite(ctx, http.MethodDelete, "/profiles/"+url.PathEscape(name), nil,
		expectedGeneration, nil, &out)
	return out, err
}

// SetActive points the global active marker at a profile. The profile must
// exist: the marker is the fallback for every client that does not name a
// profile itself, so pointing it at a typo would fail-close all of them at
// once.
func (s *ProfilesService) SetActive(
	ctx context.Context, name string, expectedGeneration uint64,
) (ProfileWrite, error) {
	active := true
	return s.Update(ctx, name, ProfilePatch{Active: &active}, expectedGeneration)
}

// ClearActive drops the global active marker, which currently points at
// name; every client that does not name a profile itself then sees every
// registered server again.
func (s *ProfilesService) ClearActive(
	ctx context.Context, name string, expectedGeneration uint64,
) (ProfileWrite, error) {
	active := false
	return s.Update(ctx, name, ProfilePatch{Active: &active}, expectedGeneration)
}
