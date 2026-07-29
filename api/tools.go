package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Tool-level governance and the integrity quarantine
// (docs/modules/controlplane.md: GET /v1/tools, PUT
// /v1/tools/{server}/{tool}, GET /v1/quarantine, DELETE
// /v1/quarantine/{exposed}).
//
// Both stores live in <data>/state, NOT in the registry — the same files the
// gateway and the daemon consult, so a decision taken here is the decision
// the call plane enforces. A daemon assembled without a state directory
// answers the uniform 404, which a frontend renders as "unavailable on this
// daemon" (IsCode + ErrCodeNotFound), never as an empty list.

// Tool is one tool's governance state, keyed by (server, RAW tool name).
//
// Raw names are the key on purpose: a state keyed on the EXPOSED name would
// move out from under itself the moment an override renamed the tool.
type Tool struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
	// Status is the integrity approval state ("" when no record exists
	// yet). It is ORTHOGONAL to Disabled: switching a tool off and back on
	// never discards an approval, and never grants one.
	Status   string `json:"status,omitempty"`
	Disabled bool   `json:"disabled"`
	// ApprovedHash and CurrentHash are the fingerprint pin and what the
	// live definition hashes to now; a difference is drift.
	ApprovedHash string `json:"approved_hash,omitempty"`
	CurrentHash  string `json:"current_hash,omitempty"`
	// OverrideName and OverrideDescription are the stored presentation
	// overrides, empty when none is set.
	OverrideName        string `json:"override_name,omitempty"`
	OverrideDescription string `json:"override_description,omitempty"`
}

// Drifted reports that the live definition no longer matches the approved
// fingerprint. Both hashes must be known: an unknown one is "we cannot
// tell", which must not read as "unchanged".
func (t Tool) Drifted() bool {
	return t.ApprovedHash != "" && t.CurrentHash != "" && t.ApprovedHash != t.CurrentHash
}

// ToolOverride is an edit of one tool's local presentation. A nil field is
// left untouched, so blanking a description cannot silently drop a rename.
//
// Description is the NEUTRALIZATION path for a prompt-injection carrier: the
// downstream keeps its poisoned description, agenthub simply stops
// forwarding it. Name replaces the RAW name before namespacing, i.e. it
// changes what a client sees.
type ToolOverride struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	// Clear removes the override entirely. It is exclusive with the two
	// field edits (the daemon refuses the combination rather than guessing
	// an order).
	Clear bool `json:"clear,omitempty"`
}

// ToolPatch is the body of a tool-governance write. A nil field is left
// untouched, so a kill-switch flip never disturbs an override and vice
// versa. The override fields are flat rather than nested because the daemon
// applies them to a different store than Enabled, in a fixed order: a
// disable first, an enable last, so a failure between the two stores cannot
// land in the looser half-state.
type ToolPatch struct {
	// Enabled false is the kill switch.
	Enabled *bool `json:"enabled,omitempty"`
	// OverrideName replaces the raw name before namespacing.
	OverrideName *string `json:"override_name,omitempty"`
	// OverrideDescription replaces the downstream description verbatim.
	OverrideDescription *string `json:"override_description,omitempty"`
	// ClearOverride drops the override entirely; exclusive with the two
	// field edits above (the daemon refuses the combination rather than
	// guessing an order).
	ClearOverride bool `json:"clear_override,omitempty"`
}

// ToolList is the answer to Tools.List.
type ToolList struct {
	Generation uint64 `json:"generation"`
	Tools      []Tool `json:"tools"`
}

// ToolWrite is what a tool-governance write returns.
type ToolWrite struct {
	WriteResult
	Server string `json:"server"`
	Tool   string `json:"tool"`
	// Enabled is present when the write touched the kill switch.
	Enabled *bool  `json:"enabled,omitempty"`
	Status  string `json:"status,omitempty"`
	// OverrideCleared and Override report the override half of the write.
	OverrideCleared bool               `json:"override_cleared,omitempty"`
	Override        *ToolOverrideValue `json:"override,omitempty"`
}

// ToolOverrideValue is a STORED override as read back: plain strings, where
// the edit form (ToolOverride) uses pointers to tell "leave alone" from
// "set to empty".
type ToolOverrideValue struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// ToolsService is the tool-level governance surface: the kill switch and the
// presentation overrides.
//
// Everything here works OFFLINE from a live connection: an operator must be
// able to disable a suspicious tool without first starting the server that
// serves it. That is the whole point of a kill switch.
//
// Precondition caveat: these stores are NOT the registry, so their writes do
// not move the registry generation. expectedGeneration is still honored, but
// as an advisory check ("your view is stale"), not as a compare-and-swap on
// these files — their own cross-process locks serialize those.
type ToolsService struct{ c *Client }

// List returns the governance state of every known tool. A non-empty server
// narrows the listing to that server.
func (s *ToolsService) List(ctx context.Context, server string) (ToolList, error) {
	var q url.Values
	if server != "" {
		q = url.Values{"server": {server}}
	}
	var out ToolList
	err := s.c.do(ctx, http.MethodGet, "/tools", q, nil, &out)
	return out, err
}

// SetEnabled flips the operator kill switch for one tool.
func (s *ToolsService) SetEnabled(
	ctx context.Context, server, tool string, enabled bool, expectedGeneration uint64,
) (ToolWrite, error) {
	return s.patch(ctx, server, tool, ToolPatch{Enabled: &enabled}, expectedGeneration)
}

// SetOverride sets or clears one tool's presentation override.
func (s *ToolsService) SetOverride(
	ctx context.Context, server, tool string, ov ToolOverride, expectedGeneration uint64,
) (ToolWrite, error) {
	return s.patch(ctx, server, tool, ToolPatch{
		OverrideName:        ov.Name,
		OverrideDescription: ov.Description,
		ClearOverride:       ov.Clear,
	}, expectedGeneration)
}

func (s *ToolsService) patch(
	ctx context.Context, server, tool string, patch ToolPatch, expectedGeneration uint64,
) (ToolWrite, error) {
	var out ToolWrite
	err := s.c.doWrite(ctx, http.MethodPut,
		"/tools/"+url.PathEscape(server)+"/"+url.PathEscape(tool), nil, expectedGeneration, patch, &out)
	return out, err
}

// QuarantineEntry is one quarantined tool, keyed by the CLIENT-VISIBLE
// exposed name: that is what an agent could have called, so that is what the
// quarantine tracks.
type QuarantineEntry struct {
	Exposed     string `json:"exposed"`
	Server      string `json:"server"`
	Tool        string `json:"tool"`
	Reason      string `json:"reason,omitempty"`
	PinnedHash  string `json:"pinned_hash,omitempty"`
	CurrentHash string `json:"current_hash,omitempty"`
	// At is when the tool was isolated (UTC, RFC 3339 on the wire).
	At time.Time `json:"at"`
}

// QuarantineRelease is the outcome of releasing one entry: the write tail
// plus the entry that was released, so a frontend can report exactly what it
// just un-isolated instead of re-reading to find out.
type QuarantineRelease struct {
	WriteResult
	Exposed string          `json:"exposed"`
	Entry   QuarantineEntry `json:"entry"`
	// Released is true when an entry was actually removed. An exposed name
	// that is not in the set answers 404 rather than a cheerful success: a
	// typo must not report "released" while the real quarantine stands.
	Released bool `json:"released"`
}

// QuarantineList is the answer to Quarantine.List.
type QuarantineList struct {
	Generation uint64            `json:"generation"`
	Entries    []QuarantineEntry `json:"entries"`
}

// QuarantineService reads and releases the integrity quarantine.
type QuarantineService struct{ c *Client }

// List returns every quarantined tool.
func (s *QuarantineService) List(ctx context.Context) (QuarantineList, error) {
	var out QuarantineList
	err := s.c.do(ctx, http.MethodGet, "/quarantine", nil, nil, &out)
	return out, err
}

// Release takes one tool out of quarantine, addressed by its EXPOSED name —
// the human re-approve step.
//
// A name that is not in the set answers ErrCodeNotFound instead of
// succeeding, so a typo cannot look like a release that happened.
func (s *QuarantineService) Release(
	ctx context.Context, exposed string, expectedGeneration uint64,
) (QuarantineRelease, error) {
	var out QuarantineRelease
	err := s.c.doWrite(ctx, http.MethodDelete, "/quarantine/"+url.PathEscape(exposed), nil,
		expectedGeneration, nil, &out)
	return out, err
}
