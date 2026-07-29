package ctlapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/skills"
)

// GET /v1/skills, PATCH /v1/skills/{id}, POST /v1/skills/{id}/install.
//
// GET finally serves the route api.SkillsService.List has been calling into
// a 404 since M1-G (docs/modules/controlplane.md). The payload is api.Skill itself rather than a
// restatement of it, so the shape the client decodes and the shape the
// daemon encodes cannot drift.
//
// PATCH is the COARSE switch only (library enabled/disabled). It deliberately
// does not unmaterialize anything: disabling a skill leaves the bytes on
// disk until a sync or an explicit removal converges the target, and the
// receipts keep reporting them honestly (internal/skills, Manager.Disable).

// SkillPatchRequest is the body of PATCH /v1/skills/{id}.
//
// Enabled is a POINTER so an absent field is a bad request rather than a
// disable. A frontend that forgot to send the field must not silently turn a
// skill off — and a skills record that omits "enabled" already reads as
// disabled on disk, so the same omission would be doubly invisible.
type SkillPatchRequest struct {
	Enabled *bool `json:"enabled"`
}

// SkillInstallRequest is the body of POST /v1/skills/{id}/install: one
// materialization of one skill into one client at one scope.
type SkillInstallRequest struct {
	ClientID string `json:"client_id"`
	// Scope is "user" (default) or "project".
	Scope string `json:"scope,omitempty"`
	// ProjectRoot is required at project scope.
	ProjectRoot string `json:"project_root,omitempty"`
	// Dir overrides the target's directory convention; required for the
	// generic target.
	Dir string `json:"dir,omitempty"`
	// AllowDrift permits overwriting a copy edited outside agenthub. Without
	// it a drifted target refuses the write (409) — drift is a user telling
	// us something, and reverting it silently is how a sync tool teaches
	// people to distrust its receipts.
	AllowDrift bool `json:"allow_drift,omitempty"`
}

// handleSkillsList implements GET /v1/skills.
func (s *Server) handleSkillsList(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	views, err := s.opts.NonRegistry.Skills.List(r.Context(), skills.ListOptions{
		ClientID: r.URL.Query().Get("client"),
	})
	if err != nil {
		// A corrupt store is surfaced, never rendered as an empty library:
		// "no skills" and "the index does not parse" are opposite findings.
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"listing skills failed: "+err.Error(), "", reqID)
		return
	}
	out := make([]api.Skill, 0, len(views))
	for _, v := range views {
		out = append(out, apiSkill(v))
	}
	writeOK(w, http.StatusOK, out)
}

// handleSkillPatch implements PATCH /v1/skills/{id}: the library-level
// enable/disable switch.
func (s *Server) handleSkillPatch(w http.ResponseWriter, r *http.Request, id string) {
	reqID := requestIDFrom(r.Context())
	var req SkillPatchRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"decoding skill patch: "+err.Error(), "", reqID)
		return
	}
	if req.Enabled == nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"no change requested", `send {"enabled":true} or {"enabled":false}`, reqID)
		return
	}

	tool := "skills/disable"
	if *req.Enabled {
		tool = "skills/enable"
	}
	start := time.Now()
	var (
		sk  *skills.Skill
		err error
	)
	if *req.Enabled {
		sk, err = s.opts.NonRegistry.Skills.Enable(r.Context(), id)
	} else {
		sk, err = s.opts.NonRegistry.Skills.Disable(r.Context(), id)
	}
	s.auditNonReg(r, skills.ProviderID, tool, hashBody([]byte(id)), err == nil, time.Since(start))
	if err != nil {
		s.writeSkillsError(w, r, err)
		return
	}
	writeOK(w, http.StatusOK, apiSkill(skills.SkillView{
		Skill: *sk, Granularity: skills.GranularityClient,
	}))
}

// handleSkillInstall implements POST /v1/skills/{id}/install.
func (s *Server) handleSkillInstall(w http.ResponseWriter, r *http.Request, id string) {
	reqID := requestIDFrom(r.Context())
	var req SkillInstallRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"decoding install request: "+err.Error(), "", reqID)
		return
	}
	if req.ClientID == "" {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"client_id is required", "name the client whose directory to materialize into", reqID)
		return
	}

	start := time.Now()
	st, err := s.opts.NonRegistry.Skills.InstallTo(r.Context(), skills.InstallRequest{
		SkillID:     id,
		ClientID:    req.ClientID,
		Scope:       req.Scope,
		ProjectRoot: req.ProjectRoot,
		Dir:         req.Dir,
		AllowDrift:  req.AllowDrift,
	})
	s.auditNonReg(r, skills.ProviderID, "skills/install",
		hashBody([]byte(id+"\x00"+req.ClientID+"\x00"+req.Scope)), err == nil, time.Since(start))
	if err != nil {
		s.writeSkillsError(w, r, err)
		return
	}
	writeOK(w, http.StatusOK, apiSkillInstall(*st, st.State, ""))
}

// writeSkillsError maps a skills failure onto the wire.
//
// A refusal (conflict, drift, tampered library) is 409 and NOT 500: the
// daemon is working correctly and the operator has a decision to make.
// An unknown skill is the uniform 404, exactly like an unknown route.
func (s *Server) writeSkillsError(w http.ResponseWriter, r *http.Request, err error) {
	reqID := requestIDFrom(r.Context())
	switch {
	case errors.Is(err, skills.ErrNotFound):
		writeNotFound(w, r)
	case errors.Is(err, skills.ErrDrifted):
		writeErr(w, http.StatusConflict, CodeConflict, err.Error(),
			"the installed copy was edited outside agenthub; re-send with allow_drift to overwrite it", reqID)
	case errors.Is(err, skills.ErrConflict):
		writeErr(w, http.StatusConflict, CodeConflict, err.Error(),
			"the target is not agenthub's to write; resolve it on disk first", reqID)
	case errors.Is(err, skills.ErrTampered):
		writeErr(w, http.StatusConflict, CodeConflict, err.Error(),
			"the library copy no longer matches its pin; re-import or verify the skill", reqID)
	case errors.Is(err, skills.ErrLockTimeout):
		writeErr(w, http.StatusConflict, CodeConflict, err.Error(),
			"another process holds the skills lock; retry", reqID)
	default:
		writeErr(w, http.StatusInternalServerError, CodeInternal, err.Error(), "", reqID)
	}
}

// apiSkill projects one library view onto the public DTO.
func apiSkill(v skills.SkillView) api.Skill {
	out := api.Skill{
		ID:          v.Skill.ID,
		Name:        v.Skill.Name,
		Description: v.Skill.Description,
		Kind:        string(v.Skill.Kind),
		Enabled:     v.Skill.Enabled,
		Fingerprint: v.Skill.Fingerprint,
		Installs:    make([]api.SkillInstall, 0, len(v.Installs)),
	}
	if !v.Skill.UpdatedAt.IsZero() {
		t := v.Skill.UpdatedAt
		out.UpdatedAt = &t
	}
	for _, iv := range v.Installs {
		out.Installs = append(out.Installs, apiSkillInstall(iv.Install, iv.State, iv.Detail))
	}
	return out
}

// apiSkillInstall projects one receipt. The LIVE state wins over the one
// recorded in the receipt: a listing must report what is on disk now, which
// is exactly the difference InstallView carries.
func apiSkillInstall(in skills.InstallState, live skills.ApplyState, detail string) api.SkillInstall {
	state := live
	if state == "" {
		state = in.State
	}
	return api.SkillInstall{
		ClientID:    in.ClientID,
		Scope:       in.Scope,
		ProjectRoot: in.ProjectRoot,
		Path:        in.Path,
		State:       apiApplyState(state),
		Detail:      detail,
	}
}

// apiApplyState maps internal/skills' ApplyState onto the frozen wire
// values. The two vocabularies differ in one word — skills calls it "stale",
// the wire calls it "outdated" — and this function is the only place that
// knows it.
//
// Fail direction: an unrecognized state renders as "conflict", the value
// that means "agenthub will not write here". A newer library state must
// never degrade to "applied", which would tell an operator everything is
// converged when the daemon does not know that.
func apiApplyState(st skills.ApplyState) string {
	switch st {
	case skills.StateApplied:
		return api.ApplyStateApplied
	case skills.StateStale:
		return api.ApplyStateOutdated
	case skills.StateDrifted:
		return api.ApplyStateDrifted
	case skills.StateMissing:
		return api.ApplyStateMissing
	default:
		return api.ApplyStateConflict
	}
}
