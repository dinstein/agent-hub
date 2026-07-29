package cli

import (
	"fmt"
	"strings"

	"github.com/dinstein/agent-hub/internal/registry"
)

// The visibility half of `server inspect`: who can actually reach this
// server.
//
// WHY IT BELONGS HERE. "The server is enabled, the credential is stored, the
// gateway is up — and my client still cannot see the tools" is answerable
// today only by reading `profile ls` and `client ls` and intersecting them by
// hand, per server, in the reader's head. The registry already holds both
// halves; this joins them for the one server that was asked about.
//
// It is computed from the REGISTRY ALONE. No client configuration file is
// opened (that is `client inspect`'s deliberate per-client act, and it can
// raise a macOS privacy prompt), and no daemon is required — so the answer
// stays available on exactly the machine that is broken.
//
// The scope chain narrows, never widens (docs/architecture.md §7), so what is
// computed here is an upper bound on what a session may see: a session scope
// can still take tools away below it. It is presented as what the CONFIGURED
// bindings allow, never as a claim about a live session.

// ServerVisibility is who may see one server.
type ServerVisibility struct {
	// Enabled repeats the global switch because it OUTRANKS every profile:
	// a disabled server reaches nobody, and a profile list implying
	// otherwise would be the more prominent falsehood.
	Enabled bool `json:"enabled"`
	// ActiveProfile is what an unbound client follows; "" means no active
	// profile, which narrows nothing.
	ActiveProfile string `json:"active_profile,omitempty"`
	// Profiles is every profile that includes this server, with whatever
	// tool narrowing it applies. Profiles that exclude it are named in
	// Excluded rather than listed here: "which of my profiles forgot this
	// server" is the question a bare list of the others cannot answer.
	Profiles []ProfileVisibility `json:"profiles,omitempty"`
	Excluded []string            `json:"excluded_profiles,omitempty"`
	// Clients covers the clients with a binding of their own. A client with
	// none is not listed one by one — there is no registry fact to list —
	// and ActiveProfile above says what it follows instead.
	Clients []ClientVisibility `json:"clients,omitempty"`
	// Overrides counts the local name/description overrides recorded for
	// this server's tools: the exposed surface differing from what the
	// downstream calls its own tools is a thing to know before comparing
	// this report with what a client shows.
	Overrides int `json:"tool_overrides,omitempty"`
}

// ProfileVisibility is one profile that includes the server.
type ProfileVisibility struct {
	Name string `json:"name"`
	// Tools describes the per-server selector, "" when the profile does not
	// narrow this server's tools at all.
	Tools  string `json:"tools,omitempty"`
	Active bool   `json:"active,omitempty"`
}

// ClientVisibility is one bound client's answer.
type ClientVisibility struct {
	Client string `json:"client"`
	// Profile is the profile that decides for this client, whether it names
	// one or follows the active one; Via says which of the two it was.
	Profile string `json:"profile,omitempty"`
	Via     string `json:"via"` // registry.BindingNamed | registry.BindingFollowActive
	// Dangling marks a binding that resolves nowhere. It is carried rather
	// than folded into Sees=false because the two call for different
	// repairs, and because a fail-closed empty scope looks exactly like a
	// deliberate exclusion from the outside.
	Dangling bool `json:"dangling,omitempty"`
	Sees     bool `json:"sees"`
}

// serverVisibilityOf computes the section for one server.
func serverVisibilityOf(id string, snap *registry.Snapshot, active string, overrides int) *ServerVisibility {
	profiles := snap.Profiles.V.Profiles
	out := &ServerVisibility{
		Enabled:       snap.Servers.V.Servers[id].V.Enabled,
		ActiveProfile: active,
		Overrides:     overrides,
	}
	for _, name := range sortedKeys(profiles) {
		p := profiles[name].V
		if !profileIncludesServer(p, id) {
			out.Excluded = append(out.Excluded, name)
			continue
		}
		pv := ProfileVisibility{Name: name, Active: name == active}
		if sel, ok := p.Tools[id]; ok {
			pv.Tools = describeSelector(sel.V)
		}
		out.Profiles = append(out.Profiles, pv)
	}
	for _, name := range sortedKeys(snap.Clients.V.Clients) {
		b := clientBindingOf(name, snap.Clients.V.Clients[name].V, profiles)
		cv := ClientVisibility{
			Client: name, Profile: b.Profile, Via: b.Binding, Dangling: b.Dangling,
		}
		cv.Sees = out.Enabled && profileReaches(profiles, b, active, id)
		out.Clients = append(out.Clients, cv)
	}
	return out
}

// profileIncludesServer applies the three-state server set: nil is "no
// narrowing" (every registered server), empty is none, a list is that list.
// The nil case is the one that must not be read as "empty".
func profileIncludesServer(p registry.Profile, id string) bool {
	if p.Servers == nil {
		return true
	}
	for _, s := range p.Servers {
		if s == id {
			return true
		}
	}
	return false
}

// profileReaches resolves one binding to a yes/no for this server, in the
// same fail-closed direction the scope chain takes: a named profile that does
// not exist sees NOTHING (docs/architecture.md §7), and only the absence of
// an active profile — a state that narrows nothing at all — reads as "yes"
// without a profile behind it.
func profileReaches(
	profiles map[string]registry.Doc[registry.Profile], b ClientBindingView, active, id string,
) bool {
	name := b.Profile
	if b.Binding != string(registry.BindingNamed) {
		if active == "" {
			return true
		}
		name = active
	}
	p, ok := profiles[name]
	if !ok {
		return false
	}
	return profileIncludesServer(p.V, id)
}

// writeVisibility renders the section. It leads with the answer people came
// for — who sees it — and puts the profile machinery underneath, because the
// profile list is the EXPLANATION and is only interesting once the answer is
// surprising.
func (i ServerInspect) writeVisibility(d *detailWriter) {
	v := i.Visibility
	if v == nil {
		return
	}
	d.section("visibility")
	switch {
	case !v.Enabled:
		d.field("seen by", "nobody — the server is disabled for every client")
		d.cont("'agenthub server enable %s' puts it back into service", i.Server.ID)
	default:
		i.writeClientVisibility(d, v)
	}
	for n, p := range v.Profiles {
		text := p.Name
		if p.Active {
			text += " (active)"
		}
		if p.Tools != "" {
			text += ": " + p.Tools
		}
		d.at(n, "profiles", "%s", text)
	}
	if len(v.Excluded) > 0 {
		d.at(len(v.Profiles), "profiles", "not in: %s", strings.Join(v.Excluded, ", "))
	}
	if v.Overrides > 0 {
		d.field("overrides", "%d tool(s) are exposed under a local name or description", v.Overrides)
		d.cont("'agenthub tool override ls %s' shows them", i.Server.ID)
	}
}

// writeClientVisibility renders the two lists that answer the question, plus
// what a client with no binding of its own gets — the case that covers most
// machines, and the one a list of explicit bindings silently omits.
func (i ServerInspect) writeClientVisibility(d *detailWriter, v *ServerVisibility) {
	var seen, hidden []string
	for _, c := range v.Clients {
		if c.Sees {
			seen = append(seen, clientVisibilityText(c))
			continue
		}
		hidden = append(hidden, clientVisibilityText(c))
	}
	if len(seen) > 0 {
		d.field("seen by", "%s", strings.Join(seen, ", "))
	}
	if len(hidden) > 0 {
		d.field("hidden", "%s", strings.Join(hidden, ", "))
	}
	d.field("default", "%s", i.unboundText(v))
}

// unboundText describes what a client that is not bound to anything sees. It
// is stated on every report rather than only when it changes the answer:
// "which of my clients is bound" is precisely what the person reading this
// does not know, and an unstated default reads as "does not apply here".
func (i ServerInspect) unboundText(v *ServerVisibility) string {
	if v.ActiveProfile == "" {
		return "no active profile — an unbound client sees every enabled server, this one included"
	}
	return fmt.Sprintf("an unbound client follows the active profile %q, %s",
		v.ActiveProfile, activeVerdict(v))
}

// activeVerdict reads the active profile's answer off the lists already
// computed rather than resolving it a second time. The third case is the one
// worth spelling out: an active profile that does not exist is not a wider
// scope, it is an empty one, and every unbound client is currently seeing
// nothing at all.
func activeVerdict(v *ServerVisibility) string {
	for _, p := range v.Profiles {
		if p.Active {
			return "which includes it"
		}
	}
	for _, name := range v.Excluded {
		if name == v.ActiveProfile {
			return "which does NOT include it"
		}
	}
	return "which does not exist — those clients see nothing at all"
}

// clientVisibilityText names the client and the profile that decided, in the
// spelling `client ls` uses for the same two states.
func clientVisibilityText(c ClientVisibility) string {
	switch {
	case c.Dangling:
		return fmt.Sprintf("%s (%s MISSING -> empty scope)", c.Client, c.Profile)
	case c.Via == string(registry.BindingNamed):
		return fmt.Sprintf("%s (%s)", c.Client, c.Profile)
	default:
		return c.Client + " (follows active)"
	}
}
