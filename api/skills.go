package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// ApplyState values for one skill installation (docs/subsystems/skills.md). A frontend
// renders them verbatim; it never re-derives state from paths or hashes.
const (
	// ApplyStateApplied: the installed copy matches the library skill.
	ApplyStateApplied = "applied"
	// ApplyStateDrifted: the installed copy was edited outside agenthub.
	ApplyStateDrifted = "drifted"
	// ApplyStateOutdated: the library skill moved on; reinstall to update.
	ApplyStateOutdated = "outdated"
	// ApplyStateMissing: the install target is gone from disk.
	ApplyStateMissing = "missing"
	// ApplyStateConflict: a foreign file occupies the install path.
	ApplyStateConflict = "conflict"
)

// SkillInstall is one (skill x client x scope) installation cell of the
// Skills matrix.
type SkillInstall struct {
	ClientID string `json:"client_id"`
	// Scope is the install scope ("user", "project", ...).
	Scope       string `json:"scope"`
	ProjectRoot string `json:"project_root,omitempty"`
	Path        string `json:"path"`
	// State is one of the ApplyState* constants.
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Skill is one library skill plus its installation matrix row.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Kind is the skill source kind ("dir", "file", ...).
	Kind    string `json:"kind,omitempty"`
	Enabled bool   `json:"enabled"`
	// Fingerprint is the pinned content fingerprint; a change to it is what
	// makes a remembered approval or an install go stale.
	Fingerprint string         `json:"fingerprint,omitempty"`
	UpdatedAt   *time.Time     `json:"updated_at,omitempty"`
	Installs    []SkillInstall `json:"installs,omitempty"`
}

// SkillPatch is the body of a library-skill edit.
//
// Enabled is a POINTER so an absent field is a bad request rather than a
// disable: a frontend that forgot to send it must not silently turn a skill
// off. It is also the ONLY editable field — a skill's content, fingerprint
// and install matrix are derived state, and writing them here would create a
// second source of truth next to the library on disk.
//
// The switch is COARSE: disabling does not unmaterialize anything. The bytes
// stay on disk until a sync or an explicit removal converges the target, and
// the install receipts keep reporting them honestly.
type SkillPatch struct {
	Enabled *bool `json:"enabled"`
}

// SkillInstallRequest materializes one skill into one client at one scope.
//
// Installation granularity is CLIENT-level, not per-session: the files live
// outside agenthub's read path, so the scope chain cannot narrow them. A
// frontend must not present it as a per-session control.
type SkillInstallRequest struct {
	ClientID string `json:"client_id"`
	// Scope is "user" (default) or "project".
	Scope string `json:"scope,omitempty"`
	// ProjectRoot is required at project scope.
	ProjectRoot string `json:"project_root,omitempty"`
	// Dir overrides the target's directory convention; required for the
	// generic target.
	Dir string `json:"dir,omitempty"`
	// AllowDrift permits overwriting a copy edited outside agenthub.
	// Without it a drifted target refuses the write with a 409 — drift is a
	// user telling us something, and reverting it silently is how a sync
	// tool teaches people to distrust its receipts.
	AllowDrift bool `json:"allow_drift,omitempty"`
}

// SkillsService reads the skills library and drives its install matrix.
//
// These calls carry no expectedGeneration: the library is not the registry,
// so there is no shared document to lose a compare-and-swap against.
type SkillsService struct{ c *Client }

// List returns the skills library with each skill's install matrix.
func (s *SkillsService) List(ctx context.Context) ([]Skill, error) {
	return s.ListForClient(ctx, "")
}

// ListForClient is List with the install rows narrowed to one client.
func (s *SkillsService) ListForClient(ctx context.Context, client string) ([]Skill, error) {
	var q url.Values
	if client != "" {
		q = url.Values{"client": {client}}
	}
	var out []Skill
	if err := s.c.do(ctx, http.MethodGet, "/skills", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetEnabled flips one library skill's coarse enable switch.
func (s *SkillsService) SetEnabled(ctx context.Context, id string, enabled bool) (Skill, error) {
	return s.Patch(ctx, id, SkillPatch{Enabled: &enabled})
}

// Patch edits one library skill and returns it as it now stands.
func (s *SkillsService) Patch(ctx context.Context, id string, patch SkillPatch) (Skill, error) {
	var out Skill
	err := s.c.do(ctx, http.MethodPatch, "/skills/"+url.PathEscape(id), nil, patch, &out)
	return out, err
}

// Install materializes one skill and returns the resulting install cell. The
// ApplyState on it is what a frontend renders — verbatim, never re-derived
// from paths or hashes.
//
// A refusal (drift, a foreign file at the target, a library copy that no
// longer matches its pin) is a 409 with E_CONFLICT: the daemon is working
// correctly and the operator has a decision to make. It is NOT a stale
// precondition, so IsConflict is false for it and a blind retry would fail
// identically; re-send with AllowDrift only if the drift is expendable.
func (s *SkillsService) Install(
	ctx context.Context, id string, req SkillInstallRequest,
) (SkillInstall, error) {
	var out SkillInstall
	err := s.c.do(ctx, http.MethodPost, "/skills/"+url.PathEscape(id)+"/install", nil, req, &out)
	return out, err
}
