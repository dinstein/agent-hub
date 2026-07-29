package scope

import (
	"fmt"

	"github.com/dinstein/agent-hub/internal/registry"
)

// FromRegistry adapts a registry snapshot into the persisted scope layers
// for one session key, in ascending specificity order:
//
//	global (governance.json + active profile selection)
//	→ profile (chosen by: project binding > client binding > followActive)
//	→ client (clients.json entry for key.ClientID)
//	→ project (longest normalized-root prefix match inside the client entry)
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
	if g.HumanApproval {
		gl.Approval.HumanApproval = boolPtr(true)
	}
	if g.DenyDestructive {
		// The ONLY place DenyDestructive enters the chain: governance.json.
		gl.Approval.DenyDestructive = boolPtr(true)
	}
	layers = append(layers, gl)

	// Client entry (may be absent: unknown clients get no client layer and
	// follow the active profile).
	var ce *registry.ClientEntry
	if doc, ok := snap.Clients.V.Clients[key.ClientID]; ok {
		c := doc.V
		ce = &c
	}

	// Project match: longest normalized-root prefix within the client entry.
	root := NormalizePath(key.Root)
	var pb *registry.ProjectBinding
	var projOrigin string
	if ce != nil && root != "" {
		bestLen := -1
		bestKey := ""
		for rawRoot, pdoc := range ce.Projects {
			nr := NormalizePath(rawRoot)
			if !PathIsWithin(nr, root) {
				continue
			}
			// Longest prefix wins; among raw keys normalizing to the same
			// root, the lexicographically smallest raw key wins (determinism).
			if len(nr) > bestLen || (len(nr) == bestLen && rawRoot < bestKey) {
				bestLen = len(nr)
				bestKey = rawRoot
				v := pdoc.V
				pb = &v
			}
		}
		if pb != nil {
			projOrigin = fmt.Sprintf("clients.json#%s/projects#%s", key.ClientID, bestKey)
		}
	}

	// Profile selection chain: project binding > client binding > default
	// followActive (docs/architecture.md §7). BindingInherit falls through to the
	// enclosing layer's binding.
	binding := registry.ProfileBinding{Kind: registry.BindingFollowActive}
	bindingOrigin := "governance.json#activeProfile"
	if ce != nil {
		binding = ce.Binding()
		bindingOrigin = "clients.json#" + key.ClientID
	}
	if pb != nil {
		if b := pb.Binding(); b.Kind != registry.BindingInherit {
			binding = b
			bindingOrigin = projOrigin
		}
	}

	profileName := ""
	dangling := false
	switch binding.Kind {
	case registry.BindingNamed:
		profileName = binding.Name
		dangling = profileName == "" // named binding without a name is dangling
	case registry.BindingFollowActive, registry.BindingInherit:
		// BindingInherit cannot surface from Binding() at the client level;
		// treat defensively as followActive.
		profileName = activeProfileName(snap)
	}

	if profileName != "" {
		if pdoc, ok := snap.Profiles.V.Profiles[profileName]; ok {
			p := pdoc.V
			layers = append(layers, ScopeLayer{
				Kind:    LayerProfile,
				Origin:  "profiles.json#" + profileName,
				Servers: cloneStrings(p.Servers),
				Tools:   selectorsFromDocs(p.Tools),
			})
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

	// Client layer.
	if ce != nil {
		cl := ScopeLayer{
			Kind:         LayerClient,
			Origin:       "clients.json#" + key.ClientID,
			Servers:      cloneStrings(ce.Servers),
			Tools:        selectorsFromDocs(ce.Tools),
			ResultBudget: budgetsFromDocs(ce.ResultBudget),
		}
		if ce.Discovery != "" {
			d := DiscoveryMode(ce.Discovery)
			cl.Discovery = &d
		}
		cl.Approval.HumanApproval = cloneBool(ce.Approval.HumanApproval)
		cl.Approval.ConfirmDestructive = cloneBool(ce.Approval.ConfirmDestructive)
		layers = append(layers, cl)
	}

	// Project layer.
	if pb != nil {
		pl := ScopeLayer{
			Kind:    LayerProject,
			Origin:  projOrigin,
			Servers: cloneStrings(pb.Servers),
			Tools:   selectorsFromDocs(pb.Tools),
		}
		if pb.Discovery != "" {
			d := DiscoveryMode(pb.Discovery)
			pl.Discovery = &d
		}
		layers = append(layers, pl)
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
			Deny:  cloneStrings(d.V.Deny),
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

func cloneBool(in *bool) *bool {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func boolPtr(v bool) *bool { return &v }
