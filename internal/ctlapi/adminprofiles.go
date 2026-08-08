package ctlapi

import (
	"cmp"
	"net/http"
	"slices"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// Profile CRUD (docs/modules/controlplane.md): membership and the
// three-state tool selectors, plus the global active marker.

// profileWire is one profile as listed.
//
// Servers keeps the three-state distinction the registry stores: absent =
// no narrowing at all (every registered server), [] = block-all, [...] =
// that set. Collapsing the first two is the fail-OPEN direction, so the
// omitzero tag of registry.Profile is preserved here.
type profileWire struct {
	Name    string                                         `json:"name"`
	Servers []string                                       `json:"servers,omitzero"`
	Tools   map[string]registry.Doc[registry.ToolSelector] `json:"tools,omitempty"`
}

// profileListWire is the GET /v1/profiles body.
type profileListWire struct {
	Generation uint64        `json:"generation"`
	Profiles   []profileWire `json:"profiles"`
	// Active is the globally active profile ("" = none). It is omitted
	// entirely when no state directory was injected, so a frontend can tell
	// "no active profile" from "this daemon cannot answer that".
	Active      string `json:"active"`
	ActiveKnown bool   `json:"active_known"`
}

// profileCreateWire is the POST /v1/profiles body.
type profileCreateWire struct {
	preconditionWire
	Name string `json:"name"`
	// Servers is the initial membership; absent/null = no narrowing,
	// [] = block-all.
	//
	// omitzero, matching profileWire above and api.Profile, rather than
	// the omitempty that stood here. Nothing marshals this struct — it is
	// only ever decoded into — so the tag was inert, which is exactly what
	// makes it worth correcting: it is a request DTO other request DTOs get
	// copied from, and omitempty on a three-state selector drops [] into
	// absent, the fail-OPEN direction the field's own comment is about.
	Servers []string `json:"servers,omitzero"`
}

// profilePatchWire is the PATCH /v1/profiles/{name} body.
//
// EXACTLY ONE operation per request. Each field below is a separate confops
// call — combining them would mean a request that half-applied when the
// second call failed, and there is no transaction spanning them.
type profilePatchWire struct {
	preconditionWire
	// Rename renames the profile AND repoints every client and project
	// reference (leaving them behind would fail-close those clients).
	Rename string `json:"rename,omitempty"`
	// Servers edits the membership set.
	Servers *serverSelectionWire `json:"servers,omitempty"`
	// Tools sets one server's three-state tool selector.
	Tools *profileToolsWire `json:"tools,omitempty"`
	// Active true points the global active marker at this profile.
	Active *bool `json:"active,omitempty"`
}

// serverSelectionWire is a membership edit. Mode is mandatory; confops
// refuses the unset one rather than guessing.
type serverSelectionWire struct {
	// Mode is "replace" (nil clears the narrowing), "add" or "remove".
	Mode    string   `json:"mode"`
	Servers []string `json:"servers,omitzero"`
}

// profileToolsWire scopes one three-state selector to one server, which is
// exactly the shape of the confops operation behind it.
type profileToolsWire struct {
	Server string `json:"server"`
	toolSelectionWire
}

// profileWriteWire is the response of every profile write.
type profileWriteWire struct {
	writeResultWire
	Name    string       `json:"name"`
	OldName string       `json:"old_name,omitempty"`
	Profile *profileWire `json:"profile,omitempty"`
	// Repointed lists the client ids a rename rewrote.
	Repointed []string `json:"repointed,omitempty"`
	// Dangling lists the client ids left pointing at a removed profile.
	// They resolve to an EMPTY scope; they are reported, never rewritten.
	Dangling      []string `json:"dangling,omitempty"`
	ActiveCleared bool     `json:"active_cleared,omitempty"`
	Deleted       bool     `json:"deleted,omitempty"`
}

func profileWireOf(name string, p registry.Profile) profileWire {
	return profileWire{Name: name, Servers: p.Servers, Tools: p.Tools}
}

// handleProfilesList implements GET /v1/profiles.
func (s *Server) handleProfilesList(w http.ResponseWriter, _ *http.Request) {
	snap := s.opts.Registry.Snapshot()
	out := profileListWire{Generation: snap.Generation, Profiles: []profileWire{}}
	for name, doc := range snap.Profiles.V.Profiles {
		out.Profiles = append(out.Profiles, profileWireOf(name, doc.V))
	}
	slices.SortFunc(out.Profiles, func(a, b profileWire) int { return cmp.Compare(a.Name, b.Name) })
	if s.opts.StateDir != "" {
		// A missing or unreadable marker reads as "no active profile" —
		// the same fail-closed direction a dangling reference takes.
		if active, err := confops.ActiveProfile(s.opts.Registry); err == nil {
			out.Active, out.ActiveKnown = active, true
		}
	}
	writeOK(w, http.StatusOK, out)
}

// handleProfileCreate implements POST /v1/profiles.
func (s *Server) handleProfileCreate(w http.ResponseWriter, r *http.Request) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req profileCreateWire
	if !decodeAdminBody(w, r, body, &req) {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	res, err := confops.CreateProfile(r.Context(), s.opts.Registry, req.Name, req.Servers, pre)
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocProfiles, res.Generation)
	writeOK(w, http.StatusOK, profileResponse(res, false))
}

// handleProfilePatch implements PATCH /v1/profiles/{name}.
func (s *Server) handleProfilePatch(w http.ResponseWriter, r *http.Request, name string) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req profilePatchWire
	if !decodeAdminBody(w, r, body, &req) {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	reqID := requestIDFrom(r.Context())
	ops := 0
	for _, present := range []bool{req.Rename != "", req.Servers != nil, req.Tools != nil, req.Active != nil} {
		if present {
			ops++
		}
	}
	if ops != 1 {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"a profile patch carries exactly one of rename, servers, tools or active",
			"they are separate operations; send them as separate requests so a failure cannot half-apply", reqID)
		return
	}

	var (
		res confops.ProfileResult
		err error
	)
	switch {
	case req.Rename != "":
		res, err = confops.RenameProfile(r.Context(), s.opts.Registry, name, req.Rename, pre)
	case req.Servers != nil:
		res, err = confops.SetProfileServers(r.Context(), s.opts.Registry, name,
			confops.ServerSelection{Mode: serverSetMode(req.Servers.Mode), Servers: req.Servers.Servers}, pre)
	case req.Tools != nil:
		res, err = confops.SetProfileTools(r.Context(), s.opts.Registry, name, req.Tools.Server,
			req.Tools.selection(), pre)
	default:
		target := name
		if !*req.Active {
			// active:false clears the marker rather than pointing it
			// elsewhere — there is no "some other profile" to guess at.
			target = ""
		}
		if s.opts.StateDir == "" {
			writeNotFound(w, r) // no state directory: the marker is not served here
			return
		}
		res, err = confops.SetActiveProfile(r.Context(), s.opts.Registry, target, pre)
	}
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	if req.Active == nil {
		// The active marker is a state file, not a registry document: it
		// bumps no generation and there is nothing to announce.
		s.publishRegistryChange(registry.DocProfiles, res.Generation)
	}
	writeOK(w, http.StatusOK, profileResponse(res, false))
}

// handleProfileDelete implements DELETE /v1/profiles/{name}.
func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request, name string) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	res, err := confops.RemoveProfile(r.Context(), s.opts.Registry, name, pre)
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocProfiles, res.Generation)
	writeOK(w, http.StatusOK, profileResponse(res, true))
}

// profileResponse renders a confops profile result. The warnings confops
// attached (a client left pointing at a removed profile, a cleared active
// marker) travel with it: "your client just lost every tool" is not
// something an operator may learn by accident.
func profileResponse(res confops.ProfileResult, deleted bool) profileWriteWire {
	out := profileWriteWire{
		writeResultWire: resultWire(res.Result),
		Name:            res.Name,
		OldName:         res.OldName,
		Repointed:       res.Repointed,
		Dangling:        res.Dangling,
		ActiveCleared:   res.ActiveCleared,
		Deleted:         deleted,
	}
	if res.Exists {
		p := profileWireOf(res.Name, res.Profile)
		out.Profile = &p
	}
	return out
}

// serverSetMode maps the wire spelling onto the confops mode. An unknown
// spelling maps to the UNSET mode, which confops refuses — never to a
// default that would edit a membership set the caller did not describe.
func serverSetMode(mode string) confops.ServerSetMode {
	switch mode {
	case "replace":
		return confops.ServerSetReplace
	case "add":
		return confops.ServerSetAdd
	case "remove":
		return confops.ServerSetRemove
	default:
		return confops.ServerSetUnset
	}
}
