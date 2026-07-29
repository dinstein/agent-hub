package integrity

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ToolState is the approval state of one tool (docs/modules/security.md).
type ToolState string

const (
	// StatePending: newly discovered, awaiting review. Calls are blocked.
	StatePending ToolState = "pending"
	// StateApproved: reviewed and trusted at ApprovedHash.
	StateApproved ToolState = "approved"
	// StateChanged: fingerprint drifted away from ApprovedHash after
	// approval (rug-pull suspect). Calls are blocked until a user reviews
	// the diff.
	StateChanged ToolState = "changed"

	// stateNone is the internal zero state used for first-sight transitions.
	stateNone ToolState = ""
)

// TransitionReason is the typed cause attached to every state change. The
// transition table is keyed on it — encoding the security property once,
// enforced everywhere.
type TransitionReason string

const (
	// ReasonFirstSeen: tool discovered under manual approval mode.
	ReasonFirstSeen TransitionReason = "first_seen"
	// ReasonAutoApprove: tool discovered under auto approval mode
	// (provenance-driven, e.g. locally hand-configured server).
	ReasonAutoApprove TransitionReason = "auto_approve"
	// ReasonUserApprove: explicit user review/approval — the ONLY reasons
	// (with ReasonUserBlock) that may clear a Changed mark.
	ReasonUserApprove TransitionReason = "user_approve"
	// ReasonUserBlock: explicit user block — approves the current content
	// and disables it in one atomic record write.
	ReasonUserBlock TransitionReason = "user_block"
	// ReasonBaselineTrust: server-level "trust current snapshot" (e.g. on
	// quarantine release of the server). Promotes Pending only — NEVER
	// Changed (rug-pull marks survive re-trusting the server).
	ReasonBaselineTrust TransitionReason = "baseline_trust"
	// ReasonDriftDetected: fingerprint no longer matches ApprovedHash.
	ReasonDriftDetected TransitionReason = "drift_detected"
	// ReasonFormulaMigration: hash formula version changed but content
	// matched under the new formula; hashes rewritten in place.
	ReasonFormulaMigration TransitionReason = "formula_migration"
)

// ApprovalMode decides the initial state of a newly discovered tool.
type ApprovalMode string

const (
	ModeAuto   ApprovalMode = "auto"   // first sight -> Approved (baseline trust by provenance)
	ModeManual ApprovalMode = "manual" // first sight -> Pending
)

// Provenance describes where a server's configuration came from; it drives
// the default approval mode.
type Provenance string

const (
	ProvenanceLocal   Provenance = "local"   // hand-configured by the user
	ProvenanceCurated Provenance = "curated" // installed from the curated catalog
	ProvenanceRemote  Provenance = "remote"  // remote/imported/unknown origin
)

// DefaultModeFor maps provenance to an approval mode. Only locally
// hand-configured servers get auto-approval — configuring a server by hand
// is itself the trust decision. Fail direction: any unknown provenance is
// ModeManual (fail-closed: unrecognized origin must not self-approve).
func DefaultModeFor(p Provenance) ApprovalMode {
	if p == ProvenanceLocal {
		return ModeAuto
	}
	return ModeManual
}

// ApprovalRecord is one tool's approval state (7.5 record shape). Previous
// and Current snapshots directly power the GUI/CLI diff-review view.
type ApprovalRecord struct {
	Server string `json:"server"` // server ID
	Tool   string `json:"tool"`   // RAW downstream tool name (rename-proof key)

	Status ToolState `json:"status"`
	// Disabled is orthogonal to Status: a user switch-off that preserves the
	// approval. Block sets Status=Approved AND Disabled=true in ONE record
	// write — there is no crash window in which a rug-pull tool exists as
	// "approved and enabled".
	Disabled bool `json:"disabled,omitempty"`

	ApprovedHash      string `json:"approvedHash,omitempty"` // "" until first approval
	CurrentHash       string `json:"currentHash"`
	HashSchemaVersion string `json:"hashSchemaVersion"`

	// Previous is the approved-time snapshot, kept while Status==Changed for
	// diff review; cleared on (re-)approval.
	Previous *ToolSnapshot `json:"previous,omitempty"`
	// Current is the last-observed snapshot.
	Current ToolSnapshot `json:"current"`

	Reason    TransitionReason `json:"reason"` // cause of the last transition
	FirstSeen time.Time        `json:"firstSeen"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

// CallAllowed is the single call-gating predicate: only an enabled, approved
// record admits calls. Fail direction: every other combination — Pending,
// Changed, Disabled, or a zero record — is blocked (fail-closed). The
// index/search plane and the call plane must both consult this same store
// state (7.5 "double gate consistency").
func (r ApprovalRecord) CallAllowed() bool {
	return r.Status == StateApproved && !r.Disabled
}

// transitionKey identifies one edge of the state machine.
type transitionKey struct {
	from, to ToolState
}

// allowedTransitions is THE transition table (docs/modules/security.md). Self-loops that rewrite
// hashes (drift refresh, formula migration) are modeled as transitions so
// they pass through the same single enforcement point.
var allowedTransitions = map[transitionKey][]TransitionReason{
	{stateNone, StatePending}:      {ReasonFirstSeen},
	{stateNone, StateApproved}:     {ReasonAutoApprove},
	{StatePending, StateApproved}:  {ReasonUserApprove, ReasonBaselineTrust, ReasonUserBlock},
	{StatePending, StatePending}:   {ReasonDriftDetected}, // never approved: further drift is not a rug-pull
	{StateApproved, StateChanged}:  {ReasonDriftDetected},
	{StateApproved, StateApproved}: {ReasonFormulaMigration},
	{StateChanged, StateChanged}:   {ReasonDriftDetected},
	// The security property: Changed -> Approved by explicit user action ONLY.
	{StateChanged, StateApproved}: {ReasonUserApprove, ReasonUserBlock},
}

// isUserAction reports whether reason is an explicit human decision.
func isUserAction(reason TransitionReason) bool {
	return reason == ReasonUserApprove || reason == ReasonUserBlock
}

// assertTransition is the single enforcement point of the state machine.
// It re-checks the "Changed -> Approved requires a user action" property
// independently of the table, so a future table edit cannot silently weaken
// it. Fail direction: any violation returns *TransitionError; the caller
// must leave the record untouched, keeping the tool blocked (fail-closed),
// and log loudly.
func assertTransition(from, to ToolState, reason TransitionReason) error {
	// Belt-and-suspenders: hard-coded security property, independent of the table.
	if from == StateChanged && to == StateApproved && !isUserAction(reason) {
		return &TransitionError{From: from, To: to, Reason: reason}
	}
	for _, r := range allowedTransitions[transitionKey{from, to}] {
		if r == reason {
			return nil
		}
	}
	return &TransitionError{From: from, To: to, Reason: reason}
}

// approvalsFile is the on-disk envelope of state/tool-approvals.json:
// server ID -> raw tool name -> record.
type approvalsFile struct {
	Version int                                  `json:"version"`
	Servers map[string]map[string]ApprovalRecord `json:"servers"`
}

// ApprovalStore persists approval records in <state>/tool-approvals.json,
// guarded by a sibling cross-process lock (same discipline as PinStore).
//
// Approval is ORTHOGONAL to quarantine (7.5 / 7.12 #19): this store answers
// "is this tool trusted for calls", quarantine answers "is this tool hidden
// by drift policy". Neither store writes the other.
type ApprovalStore struct {
	f *lockedFile
}

// OpenApprovalStore opens the approval store under stateDir — normally
// platform.StateDir().
func OpenApprovalStore(stateDir string, opts Options) (*ApprovalStore, error) {
	f, err := newLockedFile(stateDir, approvalsFileName, opts)
	if err != nil {
		return nil, err
	}
	return &ApprovalStore{f: f}, nil
}

// mutate runs fn on the decoded file under the lock and saves when fn
// reports dirty. A corrupt store aborts before fn runs — a transient decode
// error must never be treated as "records missing" and overwritten.
func (s *ApprovalStore) mutate(ctx context.Context, fn func(file *approvalsFile) (dirty bool, err error)) error {
	return s.f.withLock(ctx, func() error {
		file, _, err := loadStore[approvalsFile](s.f.path, func(v *approvalsFile) int { return v.Version })
		if err != nil {
			return err
		}
		if file.Servers == nil {
			file.Servers = map[string]map[string]ApprovalRecord{}
		}
		dirty, err := fn(&file)
		if err != nil {
			return err
		}
		if !dirty {
			return nil
		}
		file.Version = storeVersion
		return s.f.save(file)
	})
}

// ForgetServer deletes every approval record of one server — the cleanup
// half of `agenthub server rm`.
//
// This is the ONLY path that discards approval state without a per-tool
// transition, and it is exempt from assertTransition by construction: the
// records are not moving to another state, they cease to exist along with
// the server that earned them. Keeping them would mean a server re-added
// under the same id inherits Approved records for tools it never presented
// — a rug-pull with the drift check disarmed.
//
// A server with no records is a no-op (StateForgetter contract). Fail
// direction is inherited from mutate: a corrupt store aborts and is never
// overwritten.
func (s *ApprovalStore) ForgetServer(ctx context.Context, server string) error {
	return s.mutate(ctx, func(file *approvalsFile) (bool, error) {
		if _, ok := file.Servers[server]; !ok {
			return false, nil
		}
		delete(file.Servers, server)
		return true, nil
	})
}

// StateName implements confops.StateForgetter.
func (s *ApprovalStore) StateName() string { return "tool approvals" }

// Observe records the latest snapshot of one tool and advances its state:
//
//   - unknown tool: Pending (manual mode) or Approved (auto mode).
//   - fingerprint == ApprovedHash: no state change, CurrentHash refreshed.
//   - fingerprint drifted while Approved: -> Changed (automatic rug-pull
//     mark), Previous kept for diff review. Auto mode does NOT bypass this:
//     drift after approval is always Changed regardless of provenance.
//   - drifted while Pending/Changed: state kept, Current refreshed.
//   - stored under an older hash formula with identical content: hashes are
//     migrated in place, state preserved (never a fake rug-pull).
//
// Returns the post-observation record.
func (s *ApprovalStore) Observe(ctx context.Context, server string, snap ToolSnapshot, mode ApprovalMode) (ApprovalRecord, error) {
	fp, err := Fingerprint(snap)
	if err != nil {
		return ApprovalRecord{}, err
	}
	var out ApprovalRecord
	err = s.mutate(ctx, func(file *approvalsFile) (bool, error) {
		now := s.f.now().UTC()
		rec, ok := file.Servers[server][snap.Name]
		if !ok {
			rec, err = newRecord(server, snap, fp, mode, now)
			if err != nil {
				return false, err
			}
			setRecord(file, rec)
			out = rec
			return true, nil
		}

		approvedRef := rec.ApprovedHash
		if rec.HashSchemaVersion != HashSchemaVersion {
			// Formula migration: recompute the stored current snapshot under
			// the active formula; matching content bridges the version bump.
			refp, rerr := Fingerprint(rec.Current)
			if rerr == nil && refp == fp {
				if rec.Status == StateApproved {
					if err := assertTransition(StateApproved, StateApproved, ReasonFormulaMigration); err != nil {
						return false, err
					}
					rec.Reason = ReasonFormulaMigration
					rec.ApprovedHash = fp
				}
				rec.CurrentHash = fp
				rec.HashSchemaVersion = HashSchemaVersion
				rec.Current = snap
				rec.UpdatedAt = now
				setRecord(file, rec)
				out = rec
				return true, nil
			}
			// Content changed across the formula bump: compare against the
			// approved snapshot recomputed under the active formula so real
			// drift is still caught (fail-closed: recompute failure leaves
			// approvedRef stale and thus mismatching -> drift path).
			if rec.Previous != nil {
				if pfp, perr := Fingerprint(*rec.Previous); perr == nil {
					approvedRef = pfp
				}
			} else if rec.Status == StateApproved {
				if afp, aerr := Fingerprint(rec.Current); aerr == nil {
					approvedRef = afp
				}
			}
		}

		statusChanged := false
		if rec.Status == StateApproved && fp != approvedRef {
			if err := assertTransition(StateApproved, StateChanged, ReasonDriftDetected); err != nil {
				return false, err
			}
			prev := rec.Current
			rec.Previous = &prev
			rec.Status = StateChanged
			rec.Reason = ReasonDriftDetected
			statusChanged = true
		}

		// No-op guard: identical content under the current formula and no
		// state movement — skip the write (avoids write churn from N
		// gateways observing the same catalog).
		if !statusChanged && rec.CurrentHash == fp && rec.HashSchemaVersion == HashSchemaVersion {
			out = rec
			return false, nil
		}

		if !statusChanged && rec.Status != StateApproved && rec.CurrentHash != fp {
			// Drift while Pending/Changed: state kept, content refreshed —
			// still a table-checked (self-loop) transition.
			if err := assertTransition(rec.Status, rec.Status, ReasonDriftDetected); err != nil {
				return false, err
			}
			rec.Reason = ReasonDriftDetected
		}

		rec.CurrentHash = fp
		rec.HashSchemaVersion = HashSchemaVersion
		rec.Current = snap
		rec.UpdatedAt = now
		setRecord(file, rec)
		out = rec
		return true, nil
	})
	if err != nil {
		return ApprovalRecord{}, err
	}
	return out, nil
}

// newRecord builds the first-sight record for a tool under the given mode.
func newRecord(server string, snap ToolSnapshot, fp string, mode ApprovalMode, now time.Time) (ApprovalRecord, error) {
	rec := ApprovalRecord{
		Server:            server,
		Tool:              snap.Name,
		CurrentHash:       fp,
		HashSchemaVersion: HashSchemaVersion,
		Current:           snap,
		FirstSeen:         now,
		UpdatedAt:         now,
	}
	// Fail direction: only an explicit ModeAuto self-approves; anything else
	// (ModeManual, zero value, garbage) lands in Pending (fail-closed).
	if mode == ModeAuto {
		if err := assertTransition(stateNone, StateApproved, ReasonAutoApprove); err != nil {
			return ApprovalRecord{}, err
		}
		rec.Status = StateApproved
		rec.Reason = ReasonAutoApprove
		rec.ApprovedHash = fp
	} else {
		if err := assertTransition(stateNone, StatePending, ReasonFirstSeen); err != nil {
			return ApprovalRecord{}, err
		}
		rec.Status = StatePending
		rec.Reason = ReasonFirstSeen
	}
	return rec, nil
}

func setRecord(file *approvalsFile, rec ApprovalRecord) {
	if file.Servers[rec.Server] == nil {
		file.Servers[rec.Server] = map[string]ApprovalRecord{}
	}
	file.Servers[rec.Server][rec.Tool] = rec
}

// Approve is the explicit user approval: Pending|Changed -> Approved at the
// CURRENT hash ("what you reviewed is what you approved"). Previous is
// cleared — the diff is resolved. Returns ErrNotFound for unknown tools
// (approving a tool never observed would approve nothing reviewable).
func (s *ApprovalStore) Approve(ctx context.Context, server, tool string) (ApprovalRecord, error) {
	return s.userDecision(ctx, server, tool, ReasonUserApprove, false)
}

// Block is the atomic blacklist: ONE record write sets Status=Approved (at
// the current hash) AND Disabled=true, eliminating the crash window between
// two writes that could leave an "approved and enabled" rug-pull tool.
func (s *ApprovalStore) Block(ctx context.Context, server, tool string) (ApprovalRecord, error) {
	return s.userDecision(ctx, server, tool, ReasonUserBlock, true)
}

func (s *ApprovalStore) userDecision(ctx context.Context, server, tool string, reason TransitionReason, disable bool) (ApprovalRecord, error) {
	var out ApprovalRecord
	err := s.mutate(ctx, func(file *approvalsFile) (bool, error) {
		rec, ok := file.Servers[server][tool]
		if !ok {
			return false, fmt.Errorf("integrity: approve %s/%s: %w", server, tool, ErrNotFound)
		}
		if rec.Status != StateApproved {
			if err := assertTransition(rec.Status, StateApproved, reason); err != nil {
				return false, err
			}
		}
		rec.Status = StateApproved
		rec.Reason = reason
		rec.ApprovedHash = rec.CurrentHash
		rec.Previous = nil
		rec.Disabled = disable
		rec.UpdatedAt = s.f.now().UTC()
		setRecord(file, rec)
		out = rec
		return true, nil
	})
	if err != nil {
		return ApprovalRecord{}, err
	}
	return out, nil
}

// BaselineTrust promotes every Pending tool of server to Approved at its
// current hash — the "trust the server's current snapshot" bulk action
// (e.g. after releasing a server from quarantine). Changed records are
// DELIBERATELY untouched: a rug-pull mark survives re-trusting the server
// and can only be cleared per-tool by Approve/Block (7.5 baseline trust).
// Returns the promoted records sorted by tool name.
func (s *ApprovalStore) BaselineTrust(ctx context.Context, server string) ([]ApprovalRecord, error) {
	var out []ApprovalRecord
	err := s.mutate(ctx, func(file *approvalsFile) (bool, error) {
		now := s.f.now().UTC()
		dirty := false
		for tool, rec := range file.Servers[server] {
			if rec.Status != StatePending {
				continue
			}
			if err := assertTransition(StatePending, StateApproved, ReasonBaselineTrust); err != nil {
				return false, err
			}
			rec.Status = StateApproved
			rec.Reason = ReasonBaselineTrust
			rec.ApprovedHash = rec.CurrentHash
			rec.Previous = nil
			rec.UpdatedAt = now
			file.Servers[server][tool] = rec
			out = append(out, rec)
			dirty = true
		}
		return dirty, nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out, nil
}

// SetDisabled toggles the Disabled flag without touching the approval state
// (Approved <-> Disabled in the 7.5 diagram is this flag, not a state).
func (s *ApprovalStore) SetDisabled(ctx context.Context, server, tool string, disabled bool) (ApprovalRecord, error) {
	var out ApprovalRecord
	err := s.mutate(ctx, func(file *approvalsFile) (bool, error) {
		rec, ok := file.Servers[server][tool]
		if !ok {
			return false, fmt.Errorf("integrity: set-disabled %s/%s: %w", server, tool, ErrNotFound)
		}
		if rec.Disabled == disabled {
			out = rec
			return false, nil
		}
		rec.Disabled = disabled
		rec.UpdatedAt = s.f.now().UTC()
		setRecord(file, rec)
		out = rec
		return true, nil
	})
	if err != nil {
		return ApprovalRecord{}, err
	}
	return out, nil
}

// Get returns one record. Fail direction: ErrNotFound for a missing record
// in a HEALTHY store; ErrStoreCorrupt when the store cannot be read — the
// two must never be conflated (a corrupt read treated as missing would let
// auto-approval overwrite a Pending record).
func (s *ApprovalStore) Get(ctx context.Context, server, tool string) (ApprovalRecord, error) {
	var out ApprovalRecord
	err := s.mutate(ctx, func(file *approvalsFile) (bool, error) {
		rec, ok := file.Servers[server][tool]
		if !ok {
			return false, fmt.Errorf("integrity: get %s/%s: %w", server, tool, ErrNotFound)
		}
		out = rec
		return false, nil
	})
	if err != nil {
		return ApprovalRecord{}, err
	}
	return out, nil
}

// DisabledTools returns the operator kill switch as a set:
// serverID -> RAW tool name -> true for every record whose Disabled flag is
// set. It is the projection the data plane consumes (router.Policy.Disabled)
// — one locked read for the whole store instead of one per server.
//
// Deliberately NOT the same predicate as CallAllowed: this reports only the
// explicit switch-off, not the approval Status. Folding Status in here would
// make every never-reviewed tool vanish from the catalog under ModeManual,
// which is a product decision, not a store detail.
//
// Fail direction: FAIL-CLOSED — a corrupt or locked store returns an error
// with a nil map, and the caller must keep (never clear) the deny set it is
// already enforcing. An unreadable approval file is exactly what erasing a
// disable looks like, so "cannot read" must never mean "nothing disabled".
func (s *ApprovalStore) DisabledTools(ctx context.Context) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	err := s.mutate(ctx, func(file *approvalsFile) (bool, error) {
		for server, tools := range file.Servers {
			for tool, rec := range tools {
				if !rec.Disabled {
					continue
				}
				if out[server] == nil {
					out[server] = map[string]bool{}
				}
				out[server][tool] = true
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListServer returns all records of one server, sorted by tool name.
func (s *ApprovalStore) ListServer(ctx context.Context, server string) ([]ApprovalRecord, error) {
	var out []ApprovalRecord
	err := s.mutate(ctx, func(file *approvalsFile) (bool, error) {
		for _, rec := range file.Servers[server] {
			out = append(out, rec)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out, nil
}
