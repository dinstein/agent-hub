package scope

import (
	"fmt"

	"github.com/dinstein/agent-hub/internal/registry"
)

// FromRegistry adapts a registry snapshot into the persisted scope layers
// for one session key, in ascending specificity order:
//
//	global (governance.json + active profile selection)
//	→ profile (chosen by: client binding > followActive)
//
// The client contributes no layer: clients.json says WHICH profile applies,
// never what it contains.
//
// The session layer is NOT produced here — the Resolver appends the live
// Overlay. Returned layers never alias snapshot-owned maps or slices
// (Snapshot is a shared read-only view; FromRegistry stays pure).
//
// Dangling profile references resolve FAIL-CLOSED to an empty scope
// (a block-all profile layer) and are reported as a Diagnostic — never
// silently (docs/architecture.md §7).
func FromRegistry(snap *registry.Snapshot, key SessionKey) ([]ScopeLayer, []Diagnostic) {
	var layers []ScopeLayer
	var diags []Diagnostic

	// Global layer: governance.json. Plain bools become pointers only when
	// set — an unset switch is "no intervention", and since merge is OR a
	// false pointer would be inert anyway.
	g := snap.Governance.V
	gl := ScopeLayer{Kind: LayerGlobal, Origin: "governance.json"}
	if g.Discovery != "" {
		d := DiscoveryMode(g.Discovery)
		gl.Discovery = &d
	}
	gl.ResultBudget = budgetsFromDocs(g.ResultBudget)
	// Server layer: the per-server tool allow lists from servers.json. It is
	// a layer rather than a filter applied elsewhere so that the global rule
	// and a profile's rule intersect through the SAME Merge — one place
	// decides what a session sees, and adding a second narrowing mechanism
	// beside it is how the two drift apart.
	if sl, ok := serverToolsLayer(snap); ok {
		layers = append(layers, sl)
	}

	layers = append(layers, gl)

	// Client entry (may be absent: an unbound client follows the active
	// profile). It contributes no layer of its own — a client SELECTS a
	// profile, it does not narrow on top of one.
	var ce *registry.ClientEntry
	if doc, ok := snap.Clients.V.Clients[key.ClientID]; ok {
		c := doc.V
		ce = &c
	}

	// Profile selection chain: client binding > default followActive
	// (docs/architecture.md §7).
	binding := registry.ProfileBinding{Kind: registry.BindingFollowActive}
	bindingOrigin := "governance.json#activeProfile"
	if ce != nil {
		binding = ce.Binding()
		bindingOrigin = "clients.json#" + key.ClientID
	}

	profileName := ""
	dangling := false
	switch binding.Kind {
	case registry.BindingNamed:
		profileName = binding.Name
		dangling = profileName == "" // named binding without a name is dangling
	case registry.BindingFollowActive:
		profileName = activeProfileName(snap)
	}

	if profileName != "" {
		if pdoc, ok := snap.Profiles.V.Profiles[profileName]; ok {
			p := pdoc.V
			pl := ScopeLayer{
				Kind:    LayerProfile,
				Origin:  "profiles.json#" + profileName,
				Servers: cloneStrings(p.Servers),
				Tools:   selectorsFromDocs(p.Tools),
			}
			if p.Discovery != "" {
				d := DiscoveryMode(p.Discovery)
				pl.Discovery = &d
			}
			layers = append(layers, pl)
		} else {
			dangling = true
		}
	}
	if dangling {
		// FAIL-CLOSED: the referenced profile does not exist → empty scope
		// (block-all), never a silent widening to activeProfile; the
		// Diagnostic makes `session show` / `doctor` print the warning.
		layers = append(layers, ScopeLayer{
			Kind:    LayerProfile,
			Origin:  bindingOrigin,
			Servers: []string{},
		})
		diags = append(diags, Diagnostic{
			Layer:   LayerProfile,
			Origin:  bindingOrigin,
			Message: fmt.Sprintf("dangling profile %q → empty scope", profileName),
		})
	}

	return layers, diags
}

// PinnedProfileLayer builds the profile layer for an explicitly pinned
// profile name — the shape a credential (an agent token's Profile field) uses
// to join the intersection without owning a clients.json entry.
//
// Failure direction: a name that does not resolve yields a BLOCK-ALL layer
// (Servers: []) and ok=false, mirroring the dangling-reference handling of
// FromRegistry. The caller gets a layer it can merge either way, so a typo in
// a token's profile pin costs visibility rather than granting it; ok is
// returned separately so the caller can also say so out loud.
func PinnedProfileLayer(snap *registry.Snapshot, name string) (ScopeLayer, bool) {
	origin := "profiles.json#" + name
	if snap != nil {
		if pdoc, found := snap.Profiles.V.Profiles[name]; found {
			p := pdoc.V
			return ScopeLayer{
				Kind:    LayerProfile,
				Origin:  origin,
				Servers: cloneStrings(p.Servers),
				Tools:   selectorsFromDocs(p.Tools),
			}, true
		}
	}
	return ScopeLayer{Kind: LayerProfile, Origin: origin, Servers: []string{}}, false
}

// activeProfileName returns the globally active profile name, or "" when
// none is set — followActive then applies no profile narrowing, the full
// registered server set, matching `agenthub profile use -` (clear).
//
// This used to be hardcoded to "" while `agenthub profile use` wrote the
// name to a state file: the marker could be set and listed, but no session
// ever applied it. Reading it off the snapshot is what makes followActive
// actually follow, and keeps FromRegistry pure — the value arrives with the
// registry document rather than from a file read during resolution.
//
// A name that does not resolve is handled by the caller as a DANGLING
// reference (fail-closed, block-all), which is the whole point of routing it
// through the same path a named binding takes.
func activeProfileName(snap *registry.Snapshot) string {
	if snap == nil {
		return ""
	}
	return snap.Governance.V.ActiveProfile
}

// --- registry → scope conversions (always deep-copied: the snapshot is a
// shared read-only view and layers must not alias it) ---

func budgetsFromDocs(in map[string]registry.Doc[registry.Budget]) map[string]*Budget {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*Budget, len(in))
	for k, d := range in {
		b := d.V
		out[k] = &b
	}
	return out
}

func selectorsFromDocs(in map[string]registry.Doc[registry.ToolSelector]) map[string]*ToolSelector {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*ToolSelector, len(in))
	for k, d := range in {
		sel := ToolSelector{
			Allow: cloneStrings(d.V.Allow),
		}
		out[k] = &sel
	}
	return out
}

// cloneStrings preserves the load-bearing nil-vs-empty distinction:
// nil = no intervention, [] = block-all (see registry.ToolSelector).
func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// serverToolsLayer builds the global per-server tool allow lists. It returns
// ok=false when no server carries one, so the common case adds no layer at
// all rather than an inert map.
func serverToolsLayer(snap *registry.Snapshot) (ScopeLayer, bool) {
	if snap == nil {
		return ScopeLayer{}, false
	}
	var sel map[string]*ToolSelector
	for id, doc := range snap.Servers.V.Servers {
		if doc.V.Tools == nil {
			continue // no rule: the server's full tool set
		}
		if sel == nil {
			sel = map[string]*ToolSelector{}
		}
		sel[id] = &ToolSelector{Allow: cloneStrings(doc.V.Tools)}
	}
	if sel == nil {
		return ScopeLayer{}, false
	}
	return ScopeLayer{Kind: LayerGlobal, Origin: "servers.json", Tools: sel}, true
}
