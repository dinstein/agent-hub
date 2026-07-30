package ctlapi

import (
	"cmp"
	"net/http"
	"slices"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/integrity"
)

// Tool-level governance (docs/modules/controlplane.md): the per-tool kill
// switch and the local presentation override.
//
// Both live in <data>/state, NOT in the registry — the same files the
// gateway and the daemon consult, so a decision taken here is the decision
// the call plane enforces. Because they have their own cross-process locks,
// the precondition on these writes is the WEAK form: it catches "the
// operator's view is stale", not "nothing changed under me".
//
// Everything here works OFFLINE from a live connection: an operator must be
// able to disable a suspicious tool without first starting the server that
// serves it. That is the whole point of a kill switch.

// toolGovWire is one tool's governance state.
type toolGovWire struct {
	Server string `json:"server"`
	// Tool is the RAW downstream name — the rename-proof key. An override
	// keyed on the exposed name would move out from under itself the moment
	// it renamed the tool.
	Tool string `json:"tool"`
	// Status is the integrity approval state ("" when no record exists yet).
	Status string `json:"status,omitempty"`
	// Disabled is the operator kill switch. It is ORTHOGONAL to Status:
	// switching a tool off and back on never discards an approval, and
	// never grants one.
	Disabled            bool   `json:"disabled"`
	ApprovedHash        string `json:"approved_hash,omitempty"`
	CurrentHash         string `json:"current_hash,omitempty"`
	OverrideName        string `json:"override_name,omitempty"`
	OverrideDescription string `json:"override_description,omitempty"`
}

// toolListWire is the GET /v1/tools body.
type toolListWire struct {
	Generation uint64        `json:"generation"`
	Tools      []toolGovWire `json:"tools"`
}

// toolSetWire is the PUT /v1/tools/{server}/{tool} body.
//
// A request may carry the kill switch, an override edit, or both. Both are
// two separate stores with no shared transaction, so the ORDER is chosen so
// that a failure between them cannot land in the looser half-state: a
// disable is applied first, an enable last.
type toolSetWire struct {
	preconditionWire
	// Enabled false is the kill switch.
	Enabled *bool `json:"enabled,omitempty"`
	// OverrideName replaces the raw name before namespacing.
	OverrideName *string `json:"override_name,omitempty"`
	// OverrideDescription replaces the downstream description verbatim —
	// the neutralization path for a poisoned description.
	OverrideDescription *string `json:"override_description,omitempty"`
	// ClearOverride drops the override entirely; exclusive with the two
	// field edits above.
	ClearOverride bool `json:"clear_override,omitempty"`
}

// toolWriteWire is the response of a tool-governance write.
type toolWriteWire struct {
	writeResultWire
	Server          string                `json:"server"`
	Tool            string                `json:"tool"`
	Enabled         *bool                 `json:"enabled,omitempty"`
	Status          string                `json:"status,omitempty"`
	OverrideCleared bool                  `json:"override_cleared,omitempty"`
	Override        *confops.ToolOverride `json:"override,omitempty"`
}

// handleToolsList implements GET /v1/tools: every approval record of every
// registered server, merged with the local overrides.
//
// Overrides for servers or tools with no approval record are listed too: an
// override that neutralizes a description is a governance fact even when the
// server it belongs to was removed, and hiding it would hide a rule that is
// still being applied.
func (s *Server) handleToolsList(w http.ResponseWriter, r *http.Request) {
	opt, ok := s.stateOptions()
	if !ok {
		writeNotFound(w, r)
		return
	}
	store, err := integrity.OpenApprovalStore(opt.Dir, integrity.Options{LockTimeout: opt.LockTimeout})
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	overrides, err := confops.LoadToolOverrides(opt.Dir)
	if err != nil {
		s.writeOpsError(w, r, err)
		return
	}
	snap := s.opts.Registry.Snapshot()
	seen := map[string]map[string]bool{}
	out := toolListWire{Generation: snap.Generation, Tools: []toolGovWire{}}
	for id := range snap.Servers.V.Servers {
		recs, lerr := store.ListServer(r.Context(), id)
		if lerr != nil {
			s.writeOpsError(w, r, lerr)
			return
		}
		for _, rec := range recs {
			ov := overrides.Overrides[id][rec.Tool]
			out.Tools = append(out.Tools, toolGovWire{
				Server: id, Tool: rec.Tool, Status: string(rec.Status), Disabled: rec.Disabled,
				ApprovedHash: rec.ApprovedHash, CurrentHash: rec.CurrentHash,
				OverrideName: ov.Name, OverrideDescription: ov.Description,
			})
			if seen[id] == nil {
				seen[id] = map[string]bool{}
			}
			seen[id][rec.Tool] = true
		}
	}
	for server, tools := range overrides.Overrides {
		for tool, ov := range tools {
			if seen[server][tool] {
				continue
			}
			out.Tools = append(out.Tools, toolGovWire{
				Server: server, Tool: tool,
				OverrideName: ov.Name, OverrideDescription: ov.Description,
			})
		}
	}
	slices.SortFunc(out.Tools, func(a, b toolGovWire) int {
		return cmp.Or(cmp.Compare(a.Server, b.Server), cmp.Compare(a.Tool, b.Tool))
	})
	writeOK(w, http.StatusOK, out)
}

// handleToolSet implements PUT /v1/tools/{server}/{tool}.
func (s *Server) handleToolSet(w http.ResponseWriter, r *http.Request, server, tool string) {
	opt, ok := s.stateOptions()
	if !ok {
		writeNotFound(w, r)
		return
	}
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req toolSetWire
	if !decodeAdminBody(w, r, body, &req) {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	reqID := requestIDFrom(r.Context())
	overriding := req.OverrideName != nil || req.OverrideDescription != nil || req.ClearOverride
	if req.Enabled == nil && !overriding {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"nothing to set: send enabled, an override field, or clear_override", "", reqID)
		return
	}
	out := toolWriteWire{Server: server, Tool: tool}
	out.Generation = s.generation()

	// Ordering invariant: the TIGHTENING half runs first. A disable that
	// lands followed by a failed override leaves the tool off, which is the
	// safe residue; the reverse order could leave it renamed and callable.
	applyEnabled := func() bool {
		res, err := confops.SetToolEnabled(r.Context(), s.opts.Registry, opt,
			server, tool, *req.Enabled, s.opts.ToolLookup, pre)
		if err != nil {
			s.writeOpsError(w, r, err)
			return false
		}
		enabled := !res.Record.Disabled
		out.Enabled, out.Status = &enabled, string(res.Record.Status)
		out.Generation, out.Changed = res.Generation, out.Changed || res.Changed
		return true
	}
	applyOverride := func() bool {
		res, err := confops.SetToolOverride(r.Context(), s.opts.Registry, opt.Dir, server, tool,
			confops.ToolOverrideEdit{
				Name: req.OverrideName, Description: req.OverrideDescription, Clear: req.ClearOverride,
			}, pre)
		if err != nil {
			s.writeOpsError(w, r, err)
			return false
		}
		out.OverrideCleared = res.Cleared
		if !res.Cleared {
			ov := res.Override
			out.Override = &ov
		}
		out.Generation, out.Changed = res.Generation, out.Changed || res.Changed
		return true
	}

	disabling := req.Enabled != nil && !*req.Enabled
	if disabling && !applyEnabled() {
		return
	}
	if overriding && !applyOverride() {
		return
	}
	if req.Enabled != nil && !disabling && !applyEnabled() {
		return
	}
	writeOK(w, http.StatusOK, out)
}
