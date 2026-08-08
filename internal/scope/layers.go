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
// Layers that are not persisted are NOT produced here — the Resolver
// appends its Extra ones, which is how a credential narrows a session it
// owns no registry entry for. It used to append a live session Overlay;
// 0bae283 removed those, and Extra is the seam that remains. Returned
// layers never alias snapshot-owned maps or slices (Snapshot is a shared
// read-only view; FromRegistry stays pure).
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
	gl := globalLayer(snap.Governance.V)
	// Server layer: the per-server tool allow lists from servers.json. It is
	// a layer rather than a filter applied elsewhere so that the global rule
	// and a profile's rule intersect through the SAME Merge — one place
	// decides what a session sees, and adding a second narrowing mechanism
	// beside it is how the two drift apart.
	if sl, ok := ServerToolsLayer(snap); ok {
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
		if pl, ok := profileLayer(snap, profileName); ok {
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

// globalLayer builds the global layer out of governance.json. Plain bools
// become pointers only when set — see FromRegistry.
func globalLayer(g registry.GovernanceDoc) ScopeLayer {
	gl := ScopeLayer{Kind: LayerGlobal, Origin: "governance.json"}
	if g.Discovery != "" {
		d := DiscoveryMode(g.Discovery)
		gl.Discovery = &d
	}
	gl.ResultBudget = budgetsFromDocs(g.ResultBudget)
	return gl
}

// profileLayer builds the profile layer for one named profile, discovery
// included. ok=false means no such profile — the caller decides what that
// means, which for FromRegistry is a dangling reference (fail-closed).
func profileLayer(snap *registry.Snapshot, name string) (ScopeLayer, bool) {
	pdoc, found := snap.Profiles.V.Profiles[name]
	if !found {
		return ScopeLayer{}, false
	}
	p := pdoc.V
	pl := ScopeLayer{
		Kind:    LayerProfile,
		Origin:  "profiles.json#" + name,
		Servers: cloneStrings(p.Servers),
		Tools:   selectorsFromDocs(p.Tools),
	}
	if p.Discovery != "" {
		d := DiscoveryMode(p.Discovery)
		pl.Discovery = &d
	}
	return pl, true
}

// DiscoveryFor answers "which mode does this profile end up presented in",
// for a front end listing profiles without a session to resolve — `profile
// ls` printing an inherited mode rather than a dash. An empty name asks the
// same question of no profile at all: the global default.
//
// It goes through the SAME layer construction and the SAME pick rule a
// session does, because a listing that computed the answer a second way is a
// listing that eventually disagrees with the gateway it describes. ok=false
// means no layer set a mode, and the caller applies its own default
// (discovery.DefaultMode) — this package deliberately does not know it.
//
// A name that does not resolve contributes no layer: what a dangling
// reference costs is VISIBILITY, decided in Merge, and answering the
// presentation question with the global mode keeps that the only place it is
// decided.
func DiscoveryFor(snap *registry.Snapshot, profileName string) (DiscoveryMode, LayerKind, bool) {
	if snap == nil {
		return "", LayerGlobal, false
	}
	layers := []ScopeLayer{globalLayer(snap.Governance.V)}
	if profileName != "" {
		if pl, ok := profileLayer(snap, profileName); ok {
			layers = append(layers, pl)
		}
	}
	return pickDiscovery(layers)
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
//
// KNOWN DIVERGENCE from profileLayer: this layer carries the profile's
// Servers and Tools but NOT its Discovery. So the two routes to "this session
// follows profile P" — a clients.json binding and an agent token's pin —
// agree on every security field and disagree on the presentation mode, which
// docs/modules/config.md otherwise describes as taken from the most specific
// layer with no carve-out. Whether a token should inherit a profile's
// discovery mode is a product question and changing it changes what an HTTP
// agent is served, so it is recorded rather than settled here; config.md's
// internal/scope section carries the item.
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

// ServerToolsLayer builds the global per-server tool allow lists. It returns
// ok=false when no server carries one, so the common case adds no layer at
// all rather than an inert map.
//
// It is exported for the same reason DiscoveryFor is: a front end listing the
// tools this machine offers — with no client and no session to resolve — must
// reach the answer through the layer a session gets, not through a second
// filter written beside it. `server tool ls` merges exactly this layer and
// nothing else, which is what makes its answer "what every client sees before
// its own profile narrows further" rather than an approximation of it.
func ServerToolsLayer(snap *registry.Snapshot) (ScopeLayer, bool) {
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
