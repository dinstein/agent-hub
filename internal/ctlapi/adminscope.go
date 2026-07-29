package ctlapi

import (
	"net/http"
	"time"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// The CLIENT layer of the four-layer scope chain (docs/modules/controlplane.md
// §5): which profile a client follows, plus its own narrowing.
//
// Not to be confused with POST /v1/sessions/{id}/scope, which mutates one
// LIVE session's volatile overlay and may only tighten. This surface edits
// the PERSISTED binding, and the operator is allowed to loosen it — the
// control plane is the only place that can.

// scopeWire is the GET /v1/scope/{client} body.
//
// A client with no binding answers 200 with exists=false rather than the
// uniform 404: "this client has no binding yet" is the state a frontend
// needs in order to offer creating one, and it leaks nothing (every unbound
// id answers identically).
type scopeWire struct {
	Generation uint64               `json:"generation"`
	Client     string               `json:"client"`
	Exists     bool                 `json:"exists"`
	Entry      registry.ClientEntry `json:"entry,omitzero"`
	// Dangling reports that the binding names a profile that does not
	// exist. Such a client resolves to an EMPTY scope (fail-closed), which
	// must be shown as a fault, not as an empty list.
	Dangling        bool   `json:"dangling,omitempty"`
	DanglingProfile string `json:"dangling_profile,omitempty"`
}

// scopeBindingWire is the PUT /v1/scope/{client} body: a PATCH of the
// binding in which a nil field is left untouched.
//
// Optional-by-pointer rather than "zero means unset" because every zero
// value is meaningful here — an empty server list is block-all, an empty
// discovery string is inherit. A caller amending one field must not reset
// the rules it never mentioned.
type scopeBindingWire struct {
	preconditionWire
	// Profile sets the profile reference.
	Profile *profileBindingWire `json:"profile,omitempty"`
	// Servers replaces the three-state narrowing set. Absent (or null)
	// leaves it alone; [] is block-all.
	Servers *[]string `json:"servers,omitempty"`
	// Tools applies one three-state selector per server id.
	Tools map[string]toolSelectionWire `json:"tools,omitempty"`
	// Discovery overrides the discovery mode for this client.
	Discovery *string `json:"discovery,omitempty"`
}

// profileBindingWire is the explicit profile reference. "No profile" is
// spelled followActive, never an empty name.
type profileBindingWire struct {
	// Kind is "named", "followActive" or "inherit".
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

// scopeWriteWire is the response of a binding write.
type scopeWriteWire struct {
	writeResultWire
	Client          string               `json:"client"`
	Entry           registry.ClientEntry `json:"entry,omitzero"`
	Exists          bool                 `json:"exists"`
	Dangling        bool                 `json:"dangling,omitempty"`
	DanglingProfile string               `json:"dangling_profile,omitempty"`
	Cleared         bool                 `json:"cleared,omitempty"`
}

// handleScopeGet implements GET /v1/scope/{client}.
func (s *Server) handleScopeGet(w http.ResponseWriter, _ *http.Request, client string) {
	snap := s.opts.Registry.Snapshot()
	out := scopeWire{Generation: snap.Generation, Client: client}
	doc, ok := snap.Clients.V.Clients[client]
	if !ok {
		writeOK(w, http.StatusOK, out)
		return
	}
	out.Exists, out.Entry = true, doc.V
	if bind := doc.V.Binding(); bind.Kind == registry.BindingNamed {
		if _, found := snap.Profiles.V.Profiles[bind.Name]; !found {
			out.Dangling, out.DanglingProfile = true, bind.Name
		}
	}
	writeOK(w, http.StatusOK, out)
}

// handleScopePut implements PUT /v1/scope/{client}: create or amend.
func (s *Server) handleScopePut(w http.ResponseWriter, r *http.Request, client string) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req scopeBindingWire
	if !decodeAdminBody(w, r, body, &req) {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	binding := confops.ClientBinding{Servers: req.Servers, Discovery: req.Discovery}
	if req.Profile != nil {
		binding.Profile = &confops.ProfileBindingSpec{
			Kind: registry.ProfileBindingKind(req.Profile.Kind),
			Name: req.Profile.Name,
		}
	}
	if req.Tools != nil {
		binding.Tools = make(map[string]confops.ToolSelection, len(req.Tools))
		for id, sel := range req.Tools {
			binding.Tools[id] = sel.selection()
		}
	}
	start := time.Now()
	res, err := confops.SetClientBinding(r.Context(), s.opts.Registry, client, binding, pre)
	s.auditAdmin(r, adminAudit{
		action: "scope/set:" + client, client: client, body: body, err: err, dur: time.Since(start),
	})
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocClients, res.Generation)
	writeOK(w, http.StatusOK, scopeWriteWire{
		writeResultWire: resultWire(res.Result),
		Client:          res.Client,
		Entry:           res.Entry,
		Exists:          res.Exists,
		Dangling:        res.Dangling,
		DanglingProfile: res.DanglingProfile,
	})
}

// handleScopeDelete implements DELETE /v1/scope/{client}: drop the binding
// entirely; the client falls back to the active profile.
func (s *Server) handleScopeDelete(w http.ResponseWriter, r *http.Request, client string) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	start := time.Now()
	res, err := confops.ClearClientBinding(r.Context(), s.opts.Registry, client, pre)
	s.auditAdmin(r, adminAudit{
		action: "scope/clear:" + client, client: client, body: body, err: err, dur: time.Since(start),
	})
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocClients, res.Generation)
	writeOK(w, http.StatusOK, scopeWriteWire{
		writeResultWire: resultWire(res.Result), Client: client, Cleared: true,
	})
}
