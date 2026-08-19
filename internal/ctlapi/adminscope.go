package ctlapi

import (
	"fmt"
	"net/http"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// The client binding (docs/model.md#three-nouns): which profile a
// client follows. Narrowing itself lives on the profile.
//
// This is the ONLY place a client's surface is decided, and it decides it
// before the call. There was once a POST /v1/sessions/{id}/scope beside it,
// narrowing one live session's volatile overlay; it went with the rest of the
// runtime governance surface (AGENTS.md), so the persisted binding written
// here is the whole answer. The operator is allowed to loosen it — the
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

// scopeBindingWire is the PUT /v1/scope/{client} body: which profile the
// client is bound to.
//
// The retired narrowing fields are still DECLARED so a caller that sends one
// gets a 400 naming it, rather than a 200 that quietly bound the client and
// dropped the rest of the request. An old client sending servers/tools was
// asking to narrow; accepting the request while discarding that half would
// hand it a WIDER surface than it asked for and report success.
type scopeBindingWire struct {
	preconditionWire
	// Profile sets the profile reference.
	Profile *profileBindingWire `json:"profile,omitempty"`

	// Retired: narrowing moved to the profile (docs/model.md).
	Servers   *[]string                    `json:"servers,omitempty"`
	Tools     map[string]toolSelectionWire `json:"tools,omitempty"`
	Discovery *string                      `json:"discovery,omitempty"`
}

// retiredField names the first retired narrowing field the request carried,
// or "" when it carries none.
func (w scopeBindingWire) retiredField() string {
	switch {
	case w.Servers != nil:
		return "servers"
	case w.Tools != nil:
		return "tools"
	case w.Discovery != nil:
		return "discovery"
	}
	return ""
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
	if field := req.retiredField(); field != "" {
		writeErr(w, http.StatusBadRequest, confops.CodeUsage,
			fmt.Sprintf("%q is no longer part of a client binding: narrowing lives on the profile now", field),
			"put the rule in a profile ('agenthub profile server' / 'profile tool allow' / "+
				"'profile discovery') and bind this client to it",
			requestIDFrom(r.Context()))
		return
	}
	var binding confops.ClientBinding
	if req.Profile != nil {
		binding.Profile = &confops.ProfileBindingSpec{
			Kind: registry.ProfileBindingKind(req.Profile.Kind),
			Name: req.Profile.Name,
		}
	}
	res, err := confops.SetClientBinding(r.Context(), s.opts.Registry, client, binding, pre)
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
	res, err := confops.ClearClientBinding(r.Context(), s.opts.Registry, client, pre)
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	s.publishRegistryChange(registry.DocClients, res.Generation)
	writeOK(w, http.StatusOK, scopeWriteWire{
		writeResultWire: resultWire(res.Result), Client: client, Cleared: true,
	})
}
