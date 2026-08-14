package skills

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LibraryState is the health of a library entry, independent of any
// installation. It answers "can we still vouch for the canonical copy".
type LibraryState string

const (
	// LibraryOK: the copy matches its pin (and, after a full Verify, the
	// bytes on disk match the index).
	LibraryOK LibraryState = "ok"
	// LibraryTampered: the copy no longer matches its pin. Fail-closed —
	// install and update refuse to propagate it (docs/subsystems/skills.md).
	LibraryTampered LibraryState = "tampered"
	// LibraryUnpinned: no baseline exists (a pre-pin entry). Not an error,
	// but not a guarantee either.
	LibraryUnpinned LibraryState = "unpinned"
	// LibraryMissing: the receipt or request names a skill the library does
	// not have.
	LibraryMissing LibraryState = "missing"
)

// InstallView is one receipt plus its freshly computed state.
type InstallView struct {
	Install InstallState `json:"install"`
	// State is computed live against disk; it can differ from
	// Install.State, which is what the last write recorded.
	State  ApplyState `json:"state"`
	Detail string     `json:"detail,omitempty"`
}

// SkillView is a library entry with its installations.
type SkillView struct {
	Skill   Skill        `json:"skill"`
	Library LibraryState `json:"library"`
	// PinnedFingerprint is the recorded baseline ("" when unpinned).
	PinnedFingerprint string        `json:"pinnedFingerprint,omitempty"`
	Installs          []InstallView `json:"installs"`
	// Granularity is always GranularityClient — file materialization
	// cannot reach per-session precision (package doc, docs/subsystems/skills.md).
	Granularity string `json:"granularity"`
}

// AddRequest imports a directory into the library.
type AddRequest struct {
	// Path is the source directory (required, and the only thing read).
	Path string
	// Name overrides the SKILL.md frontmatter name.
	Name string
	// ID overrides the derived slug. When it is already taken the request
	// fails with ErrExists instead of silently deduplicating — an explicit
	// ID is a user's assertion, not a suggestion.
	ID string
	// Kind defaults to KindSkillPack.
	Kind SkillKind
	// SourceKind defaults to SourceLocal. SourceGit records git provenance
	// for a checkout the caller already has; see the package doc on why
	// this package performs no git operations.
	SourceKind SourceKind
	GitURL     string
	// Pin is the revision the user asked for (--pin <rev>). Recorded
	// verbatim.
	Pin string
	// Commit is the resolved commit, when the caller knows it.
	Commit string
	// Disabled adds the entry disabled (it defaults to enabled).
	Disabled bool
}

// Add imports a source directory into the library.
func (m *Manager) Add(ctx context.Context, req AddRequest) (*Skill, error) {
	if req.Path == "" {
		return nil, errors.New("skills: add needs a source path")
	}
	sc, err := m.scanTree(req.Path)
	if err != nil {
		return nil, err
	}
	kind := req.Kind
	if kind == "" {
		kind = KindSkillPack
	}
	if sc.meta.Kind != "" && req.Kind == "" {
		kind = sc.meta.Kind
	}
	name := firstNonEmpty(req.Name, sc.meta.Name, sc.dirName)
	if err := m.scanContent(sc, name); err != nil {
		return nil, err
	}
	// Content carrying our sentinel strings could truncate its own block on
	// a sentinel-block target and orphan everything after the fake END, so
	// it is refused at the door rather than at install time. The check covers
	// every field renderSkillBody embeds — including the version and file
	// paths — not just name/description/body.
	if scanCarriesSentinelMarker(name, sc) {
		return nil, &ImportError{Path: req.Path, Reason: "content contains an agenthub sentinel marker"}
	}

	contentHash := ContentHash(sc.files)
	srcKind := req.SourceKind
	if srcKind == "" {
		srcKind = SourceLocal
	}
	absSrc, err := filepath.Abs(req.Path)
	if err != nil {
		return nil, err
	}

	var out Skill
	err = m.withState(ctx, func(st *state) error {
		taken := func(id string) bool { _, ok := st.skills.Skills[id]; return ok }
		id := req.ID
		if id == "" {
			id = uniqueID(slugify(name), taken)
		} else {
			// Shape before collision: an ID this store would never mint is
			// answered with what is wrong with it, not with "already
			// exists". req.ID reaches here verbatim from an operator's
			// --id today and from whatever calls AddRequest tomorrow.
			if err := checkID(id); err != nil {
				return err
			}
			if taken(id) {
				return fmt.Errorf("%w: skill %q", ErrExists, id)
			}
		}
		now := m.now()
		sk := Skill{
			ID: id, Name: name, Description: sc.meta.Description, Kind: kind,
			Version: deriveVersion(sc.meta.Version, contentHash),
			Source: Source{
				Kind: srcKind, Path: absSrc, GitURL: req.GitURL,
				GitRef: req.Pin, PinnedCommit: req.Commit,
			},
			Path:        storeRel(id, contentHash),
			ContentHash: contentHash,
			Files:       sc.files,
			Enabled:     !req.Disabled,
			AddedAt:     now, UpdatedAt: now,
		}
		fp, err := Fingerprint(SnapshotOf(&sk))
		if err != nil {
			return err
		}
		sk.Fingerprint = fp
		if err := copyTree(absSrc, m.SkillPath(&sk), sk.Files); err != nil {
			return err
		}
		st.putSkill(sk)
		st.pin(id, fp, now)
		if err := m.pruneVersions(id, contentHash); err != nil {
			return err
		}
		out = sk
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListOptions filters List.
type ListOptions struct {
	// ClientID, when set, keeps only that client's install points.
	ClientID string
}

// List returns the library with each entry's live install states.
//
// Read-only: the freshly computed states are reported, never written back.
// A listing must not mutate state — that is Verify's job, and conflating
// them would make "look at the skills" a write that can fail.
func (m *Manager) List(ctx context.Context, opts ListOptions) ([]SkillView, error) {
	var out []SkillView
	err := m.withState(ctx, func(st *state) error {
		for _, sk := range st.sortedSkills() {
			out = append(out, m.viewOf(st, sk, opts.ClientID))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Inspect returns one library entry with its install states.
func (m *Manager) Inspect(ctx context.Context, id string) (*SkillView, error) {
	var out SkillView
	err := m.withState(ctx, func(st *state) error {
		sk, ok := st.skill(id)
		if !ok {
			return fmt.Errorf("%w: skill %q", ErrNotFound, id)
		}
		out = m.viewOf(st, sk, "")
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// viewOf builds a SkillView. The library check here is the cheap one (index
// fingerprint against the pin); Verify does the full recompute from disk.
func (m *Manager) viewOf(st *state, sk Skill, clientID string) SkillView {
	v := SkillView{Skill: sk, Granularity: GranularityClient}
	if p, ok := st.pins.Pins[sk.ID]; ok {
		v.PinnedFingerprint = p.Fingerprint
		if p.Fingerprint == sk.Fingerprint {
			v.Library = LibraryOK
		} else {
			v.Library = LibraryTampered
		}
	} else {
		v.Library = LibraryUnpinned
	}
	for _, rec := range st.installsOf(sk.ID) {
		if clientID != "" && rec.ClientID != clientID {
			continue
		}
		t, ok := m.Target(rec.ClientID)
		if !ok {
			v.Installs = append(v.Installs, InstallView{
				Install: rec, State: StateConflict,
				Detail: "no target definition for client " + rec.ClientID,
			})
			continue
		}
		s, detail := m.verifyOne(rec, &sk, t)
		v.Installs = append(v.Installs, InstallView{Install: rec, State: s, Detail: detail})
	}
	return v
}

// Enable and Disable flip the library entry's Enabled flag. They are the
// coarse switch; the fine one is the SkillSelector a Sync carries from the
// scope chain. A disabled skill is never materialized by Sync, whatever the
// selector says — the two narrow, they never widen.
func (m *Manager) Enable(ctx context.Context, id string) (*Skill, error) {
	return m.setEnabled(ctx, id, true)
}

// Disable marks a library entry disabled. Note it does NOT unmaterialize
// anything on its own: the bytes stay until a Sync (or an explicit Remove)
// converges the target, and until then the receipt honestly reports them.
func (m *Manager) Disable(ctx context.Context, id string) (*Skill, error) {
	return m.setEnabled(ctx, id, false)
}

func (m *Manager) setEnabled(ctx context.Context, id string, enabled bool) (*Skill, error) {
	var out Skill
	err := m.withState(ctx, func(st *state) error {
		sk, ok := st.skill(id)
		if !ok {
			return fmt.Errorf("%w: skill %q", ErrNotFound, id)
		}
		if sk.Enabled != enabled {
			sk.Enabled = enabled
			sk.UpdatedAt = m.now()
			st.putSkill(sk)
		}
		out = sk
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveRequest removes a library entry and everything it materialized.
type RemoveRequest struct {
	ID string
	// Force drops receipts whose materialized copy could not be removed
	// (a conflicted target). The files stay where they are — Force means
	// "stop tracking", never "delete something we cannot prove is ours".
	Force bool
	// KeepInstalled leaves materialized copies in place on purpose.
	KeepInstalled bool
}

// RemoveResult reports what Remove did.
type RemoveResult struct {
	SkillID string `json:"skillId"`
	// RemovedInstalls lists the install paths that were unmaterialized.
	RemovedInstalls []string `json:"removedInstalls,omitempty"`
	// Conflicts lists install points that refused to be removed.
	Conflicts   []string `json:"conflicts,omitempty"`
	Granularity string   `json:"granularity"`
}

// Remove unmaterializes every install point and then deletes the library
// entry and its stored copies.
//
// The pin is deliberately KEPT (integrity.rs' "merge never deletes"): if
// the same skill is added again later, its baseline history is still there
// to compare against instead of being re-pinned blind.
func (m *Manager) Remove(ctx context.Context, req RemoveRequest) (*RemoveResult, error) {
	res := &RemoveResult{SkillID: req.ID, Granularity: GranularityClient}
	err := m.withState(ctx, func(st *state) error {
		sk, ok := st.skill(req.ID)
		if !ok {
			return fmt.Errorf("%w: skill %q", ErrNotFound, req.ID)
		}
		for _, rec := range st.installsOf(sk.ID) {
			if req.KeepInstalled {
				st.deleteInstall(rec.key())
				continue
			}
			t, known := m.Target(rec.ClientID)
			var rerr error
			if !known {
				rerr = &ConflictError{Path: rec.Path, Reason: "no target definition for client " + rec.ClientID}
			} else {
				rerr = m.removeMaterialized(rec, t)
			}
			if rerr != nil {
				if !req.Force {
					res.Conflicts = append(res.Conflicts, rec.Path)
					continue
				}
				res.Conflicts = append(res.Conflicts, rec.Path)
			} else {
				res.RemovedInstalls = append(res.RemovedInstalls, rec.Path)
			}
			st.deleteInstall(rec.key())
		}
		if len(res.Conflicts) > 0 && !req.Force {
			return &ConflictError{
				Path:   strings.Join(res.Conflicts, ", "),
				Reason: "install points could not be removed; re-run with force to stop tracking them",
			}
		}
		delete(st.skills.Skills, sk.ID)
		// The ID was shape-checked when it was added, but this is the one
		// place where believing it costs a directory outside the store, and
		// the index it came from is a file on disk. Re-check rather than
		// join: dropping the library entry while leaving the files is a
		// recoverable state, deleting the wrong tree is not.
		if err := checkID(sk.ID); err != nil {
			return err
		}
		return os.RemoveAll(filepath.Join(m.dir, storeDirName, sk.ID))
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// Plan reports what InstallTo would do, writing nothing.
func (m *Manager) Plan(ctx context.Context, req InstallRequest) (*InstallPlan, error) {
	var out *InstallPlan
	err := m.withState(ctx, func(st *state) error {
		sk, t, err := m.lookupForInstall(st, req.SkillID, req.ClientID)
		if err != nil {
			return err
		}
		out, err = m.plan(st, sk, t, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// InstallTo materializes one skill into one client at one scope.
//
// Granularity: the receipt returned says GranularityClient, because that is
// the truth — every session of that client now sees these bytes.
func (m *Manager) InstallTo(ctx context.Context, req InstallRequest) (*InstallState, error) {
	var out InstallState
	err := m.withState(ctx, func(st *state) error {
		sk, t, err := m.lookupForInstall(st, req.SkillID, req.ClientID)
		if err != nil {
			return err
		}
		if err := m.requireTrusted(st, sk); err != nil {
			return err
		}
		p, err := m.plan(st, sk, t, req)
		if err != nil {
			return err
		}
		out, err = m.apply(st, sk, t, req, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// lookupForInstall resolves a skill and a target, or explains which is
// missing.
func (m *Manager) lookupForInstall(st *state, skillID, clientID string) (*Skill, TargetDef, error) {
	sk, ok := st.skill(skillID)
	if !ok {
		return nil, TargetDef{}, fmt.Errorf("%w: skill %q", ErrNotFound, skillID)
	}
	t, ok := m.Target(clientID)
	if !ok {
		return nil, TargetDef{}, fmt.Errorf("%w: install target %q", ErrNotFound, clientID)
	}
	return &sk, t, nil
}

// requireTrusted refuses to propagate a library copy that is not what the
// index says it is. It runs before every materialization, and it is the
// check standing between a modified `SKILL.md` and a client's rule file —
// where the bytes are read by that client's model as its own instructions.
//
// **It re-reads the library copy rather than comparing the index to
// itself.** Comparing `pins.Pins[id].Fingerprint` against `sk.Fingerprint`
// is two values out of the same two files, so a stored file edited after
// pinning — leaving `skills.json` and `skill-pins.json` untouched — passed
// it, and `applySentinel` then read that modified file directly. This
// package already says why elsewhere: an index a tamperer has edited cannot
// vouch for itself. The install path was the one taking it at its word.
//
// So the recomputation is `verifyLibrary`'s, the same one `Verify` reports:
// hash the files on disk, rebuild the fingerprint from what is actually
// there, and compare both against the recorded values.
//
// Fail-closed in three directions. Tampered refuses. A library copy that
// cannot be read or hashed refuses too, rather than being read as "nothing
// to compare" — that is precisely the state an attacker can arrange. Only
// LibraryUnpinned passes without a baseline, and it has still had its
// content hash checked; it means an entry predating pinning, not an
// unverified one.
//
// What it cannot answer: a tamperer who rewrites the library copy, the
// index AND the pin file consistently. All three live on the same disk, and
// no recomputation can outrank a baseline the attacker also wrote. The
// remaining window is between this check and the read at materialization.
func (m *Manager) requireTrusted(st *state, sk *Skill) error {
	lib, onDisk, detail := m.verifyLibrary(st, *sk)
	switch lib {
	case LibraryOK, LibraryUnpinned:
		return nil
	case LibraryTampered:
		pinned := ""
		if p, ok := st.pins.Pins[sk.ID]; ok {
			pinned = p.Fingerprint
		}
		return &TamperError{SkillID: sk.ID, Pinned: pinned, Current: onDisk, Detail: detail}
	default:
		return fmt.Errorf("%w: %q: %s", ErrUnverifiable, sk.ID, detail)
	}
}

// SyncAction is what Sync did to one skill at one target.
type SyncAction string

const (
	// ActionInstalled: materialized for the first time.
	ActionInstalled SyncAction = "installed"
	// ActionUpdated: re-materialized because the library moved on or the
	// files were missing.
	ActionUpdated SyncAction = "updated"
	// ActionUnchanged: already converged; nothing was written.
	ActionUnchanged SyncAction = "unchanged"
	// ActionRemoved: unmaterialized because the skill is no longer
	// selected or is disabled.
	ActionRemoved SyncAction = "removed"
	// ActionSkipped: deliberately left alone (drift without AllowDrift,
	// unsupported kind).
	ActionSkipped SyncAction = "skipped"
	// ActionFailed: refused or errored; Error says why.
	ActionFailed SyncAction = "failed"
)

// SyncRequest materializes a whole scope's worth of skills into one client.
type SyncRequest struct {
	ClientID    string
	Scope       string
	ProjectRoot string
	Dir         string
	// Selector is the three-state narrowing from the scope chain
	// (nil = every enabled skill).
	Selector *SkillSelector
	// AllowDrift lets Sync overwrite locally modified copies.
	AllowDrift bool
	// NoPrune keeps receipts whose skill is no longer selected. Pruning is
	// the default because "sync" means converge: leaving a deselected skill
	// materialized would make the target disagree with the scope that
	// governs it.
	NoPrune bool
}

// SyncItem is one skill's outcome.
type SyncItem struct {
	SkillID string     `json:"skillId"`
	Action  SyncAction `json:"action"`
	State   ApplyState `json:"state"`
	Path    string     `json:"path,omitempty"`
	Detail  string     `json:"detail,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// SyncResult reports a whole Sync.
type SyncResult struct {
	ClientID    string     `json:"clientId"`
	Scope       string     `json:"scope"`
	ProjectRoot string     `json:"projectRoot,omitempty"`
	Items       []SyncItem `json:"items"`
	// Changed is false when the target was already converged (Sync is
	// idempotent: a second run writes nothing).
	Changed bool `json:"changed"`
	// Granularity is always GranularityClient. Sync CANNOT give a session
	// its own skill set — the bytes are shared by every session of this
	// client (docs/subsystems/skills.md, A.3 #5).
	Granularity string `json:"granularity"`
}

// Sync converges one client target on the selected set of enabled skills.
//
// One skill's conflict never aborts the batch: a shadowed file or a
// hand-edited copy is recorded as a failed item and the other skills still
// converge. Only store-level failures return an error.
func (m *Manager) Sync(ctx context.Context, req SyncRequest) (*SyncResult, error) {
	res := &SyncResult{
		ClientID: req.ClientID, Scope: req.Scope, ProjectRoot: req.ProjectRoot,
		Granularity: GranularityClient,
	}
	if res.Scope == "" {
		res.Scope = ScopeUser
	}
	err := m.withState(ctx, func(st *state) error {
		t, ok := m.Target(req.ClientID)
		if !ok {
			return fmt.Errorf("%w: install target %q", ErrNotFound, req.ClientID)
		}
		wanted := map[string]bool{}
		for _, sk := range st.sortedSkills() {
			if !sk.Enabled || !req.Selector.selects(sk.ID) || !t.supports(sk.Kind) {
				continue
			}
			wanted[sk.ID] = true
			ireq := InstallRequest{
				SkillID: sk.ID, ClientID: req.ClientID, Scope: res.Scope,
				ProjectRoot: req.ProjectRoot, Dir: req.Dir, AllowDrift: req.AllowDrift,
			}
			res.Items = append(res.Items, m.syncOne(st, sk, t, ireq))
		}
		if req.NoPrune {
			return nil
		}
		// Prune only inside the container(s) THIS request converges. A sync
		// of project A must never unmaterialize project B, and a generic
		// target pointed at one directory must not touch another — the
		// receipt's container is what makes those distinct.
		mine := m.containersOf(t, res.Scope, req.ProjectRoot, req.Dir)
		for _, rec := range append([]InstallState(nil), st.installs.Installs...) {
			if rec.ClientID != req.ClientID || rec.Scope != res.Scope || wanted[rec.SkillID] || !mine[rec.Container] {
				continue
			}
			item := SyncItem{SkillID: rec.SkillID, Action: ActionRemoved, State: StateMissing, Path: rec.Path}
			if err := m.removeMaterialized(rec, t); err != nil {
				item.Action, item.State, item.Error = ActionFailed, StateConflict, err.Error()
				res.Items = append(res.Items, item)
				continue
			}
			st.deleteInstall(rec.key())
			res.Items = append(res.Items, item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(res.Items, func(a, b SyncItem) int { return cmp.Compare(a.SkillID, b.SkillID) })
	for _, it := range res.Items {
		if it.Action != ActionUnchanged {
			res.Changed = true
			break
		}
	}
	return res, nil
}

// containersOf resolves every directory a target could write for one scope
// — one per supported kind, since a target may split kinds across
// directories. Unresolvable combinations are simply absent, which makes the
// prune pass skip receipts it cannot prove belong to this request.
func (m *Manager) containersOf(t TargetDef, scope, projectRoot, dir string) map[string]bool {
	home := ""
	if dir == "" && !t.RequiresDir && scope == ScopeUser {
		home, _ = m.home()
	}
	out := map[string]bool{}
	for _, k := range t.Supports {
		if c, err := t.container(scope, home, projectRoot, dir, k); err == nil {
			out[c] = true
		}
	}
	return out
}

// syncOne converges a single skill at a target, translating every failure
// into an item instead of an aborted batch.
func (m *Manager) syncOne(st *state, sk Skill, t TargetDef, req InstallRequest) SyncItem {
	item := SyncItem{SkillID: sk.ID}
	if err := m.requireTrusted(st, &sk); err != nil {
		item.Action, item.State, item.Error = ActionFailed, StateConflict, err.Error()
		return item
	}
	p, err := m.plan(st, &sk, t, req)
	if err != nil {
		item.Action, item.State, item.Error = ActionFailed, StateConflict, err.Error()
		return item
	}
	item.Path, item.Detail = p.Path, p.Detail
	_, _, had := st.install(receiptKey(sk.ID, t.ClientID, req.scope(), p.Container))
	switch p.State {
	case StateApplied:
		item.Action, item.State = ActionUnchanged, StateApplied
		return item
	case StateConflict:
		item.Action, item.State, item.Error = ActionFailed, StateConflict, p.Detail
		return item
	case StateDrifted:
		if !req.AllowDrift {
			item.Action, item.State = ActionSkipped, StateDrifted
			return item
		}
	}
	rec, err := m.apply(st, &sk, t, req, p)
	if err != nil {
		item.Action, item.State, item.Error = ActionFailed, p.State, err.Error()
		return item
	}
	item.State, item.Path = rec.State, rec.Path
	if had {
		item.Action = ActionUpdated
	} else {
		item.Action = ActionInstalled
	}
	return item
}

// UpdateRequest refreshes a library entry from its source.
type UpdateRequest struct {
	ID string
	// Path is the directory to re-import from. Defaults to Source.Path for
	// local sources; REQUIRED for git sources (this package never fetches,
	// see the package doc — M2).
	Path string
	// Pin records a new revision for a git source. Passing only Pin
	// records provenance without touching content, which is exactly what
	// `skill update --pin <rev>` can honestly do today.
	Pin    string
	Commit string
	// Check reports what would change and writes nothing (--check).
	Check bool
	// Reapply re-materializes every install point afterwards.
	Reapply bool
	// AllowDrift lets the reapply pass overwrite locally modified copies.
	AllowDrift bool
}

// UpdateResult reports an update.
type UpdateResult struct {
	SkillID string `json:"skillId"`
	// Changed is false when the source produced identical content.
	Changed         bool   `json:"changed"`
	Check           bool   `json:"check"`
	FromVersion     string `json:"fromVersion"`
	ToVersion       string `json:"toVersion"`
	FromContentHash string `json:"fromContentHash"`
	ToContentHash   string `json:"toContentHash"`
	FromFingerprint string `json:"fromFingerprint"`
	ToFingerprint   string `json:"toFingerprint"`
	// Reapplied lists the install points re-materialized afterwards.
	Reapplied []SyncItem `json:"reapplied,omitempty"`
	Detail    string     `json:"detail,omitempty"`
	// Granularity is always GranularityClient.
	Granularity string `json:"granularity"`
}

// Update re-imports a library entry from its source.
//
// Git sources: this package performs NO git operations (package doc). A
// request carrying only --pin records the revision; a request without a
// local checkout path returns ErrGitFetchUnsupported rather than reporting
// "up to date" without having looked.
func (m *Manager) Update(ctx context.Context, req UpdateRequest) (*UpdateResult, error) {
	res := &UpdateResult{SkillID: req.ID, Granularity: GranularityClient, Check: req.Check}
	err := m.withState(ctx, func(st *state) error {
		sk, ok := st.skill(req.ID)
		if !ok {
			return fmt.Errorf("%w: skill %q", ErrNotFound, req.ID)
		}
		res.FromVersion, res.ToVersion = sk.Version, sk.Version
		res.FromContentHash, res.ToContentHash = sk.ContentHash, sk.ContentHash
		res.FromFingerprint, res.ToFingerprint = sk.Fingerprint, sk.Fingerprint

		src := req.Path
		if src == "" {
			if sk.Source.Kind == SourceGit {
				if req.Pin == "" {
					return ErrGitFetchUnsupported
				}
				// Pin-only update: provenance changes, content does not.
				if req.Check {
					res.Detail = "would record revision " + req.Pin
					res.Changed = sk.Source.GitRef != req.Pin || (req.Commit != "" && sk.Source.PinnedCommit != req.Commit)
					return nil
				}
				sk.Source.GitRef = req.Pin
				if req.Commit != "" {
					sk.Source.PinnedCommit = req.Commit
				}
				sk.UpdatedAt = m.now()
				st.putSkill(sk)
				res.Changed = true
				res.Detail = "recorded revision " + req.Pin + " (content unchanged; git fetch is M2)"
				return nil
			}
			src = sk.Source.Path
		}
		if src == "" {
			return errors.New("skills: update needs a source path (the original source is unknown)")
		}

		sc, err := m.scanTree(src)
		if err != nil {
			return err
		}
		name := firstNonEmpty(sc.meta.Name, sk.Name)
		if err := m.scanContent(sc, name); err != nil {
			return err
		}
		if scanCarriesSentinelMarker(name, sc) {
			return &ImportError{Path: src, Reason: "content contains an agenthub sentinel marker"}
		}
		next := sk
		next.Name = name
		next.Description = sc.meta.Description
		next.Files = sc.files
		next.ContentHash = ContentHash(sc.files)
		next.Version = deriveVersion(sc.meta.Version, next.ContentHash)
		next.Path = storeRel(sk.ID, next.ContentHash)
		if abs, err := filepath.Abs(src); err == nil {
			next.Source.Path = abs
		}
		if req.Pin != "" {
			next.Source.GitRef = req.Pin
		}
		if req.Commit != "" {
			next.Source.PinnedCommit = req.Commit
		}
		fp, err := Fingerprint(SnapshotOf(&next))
		if err != nil {
			return err
		}
		next.Fingerprint = fp
		res.ToVersion, res.ToContentHash, res.ToFingerprint = next.Version, next.ContentHash, next.Fingerprint
		res.Changed = next.Fingerprint != sk.Fingerprint
		if !res.Changed {
			res.Detail = "already up to date"
			return nil
		}
		if req.Check {
			res.Detail = "update available"
			return nil
		}
		next.UpdatedAt = m.now()
		if err := copyTree(next.Source.Path, m.SkillPath(&next), next.Files); err != nil {
			return err
		}
		st.putSkill(next)
		st.pin(next.ID, next.Fingerprint, next.UpdatedAt)
		if err := m.pruneVersions(next.ID, next.ContentHash); err != nil {
			return err
		}
		if !req.Reapply {
			return nil
		}
		for _, rec := range st.installsOf(next.ID) {
			t, ok := m.Target(rec.ClientID)
			if !ok {
				res.Reapplied = append(res.Reapplied, SyncItem{
					SkillID: next.ID, Action: ActionFailed, State: StateConflict,
					Path: rec.Path, Error: "no target definition for client " + rec.ClientID,
				})
				continue
			}
			res.Reapplied = append(res.Reapplied, m.syncOne(st, next, t, InstallRequest{
				SkillID: next.ID, ClientID: rec.ClientID, Scope: rec.Scope,
				ProjectRoot: rec.ProjectRoot, Dir: rec.Container, AllowDrift: req.AllowDrift,
			}))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// VerifyRequest selects what to verify.
type VerifyRequest struct {
	// ID verifies one library entry; empty verifies all (`verify --all`).
	ID string
	// ClientID narrows the install points that are checked.
	ClientID string
}

// SkillVerification is one library entry's full three-way comparison:
// upstream provenance, library copy, materialized copies (docs/subsystems/skills.md).
type SkillVerification struct {
	SkillID string       `json:"skillId"`
	Library LibraryState `json:"library"`
	Detail  string       `json:"detail,omitempty"`
	// Fingerprint is recomputed from the bytes on disk, not read from the
	// index — an index a tamperer edited must not be able to vouch for
	// itself.
	Fingerprint       string        `json:"fingerprint,omitempty"`
	PinnedFingerprint string        `json:"pinnedFingerprint,omitempty"`
	Installs          []InstallView `json:"installs"`
}

// VerifyReport is the whole verification run.
type VerifyReport struct {
	Skills []SkillVerification `json:"skills"`
	// OK is false when anything is tampered, drifted, missing or
	// conflicted.
	OK bool `json:"ok"`
	// Granularity is always GranularityClient.
	Granularity string `json:"granularity"`
}

// Verify recomputes the library copies from disk, compares them against the
// pins, and re-classifies every install point.
//
// Unlike List this DOES persist the refreshed receipt states: verify is an
// explicit command, and the point of running it is to leave the receipts
// telling the truth.
func (m *Manager) Verify(ctx context.Context, req VerifyRequest) (*VerifyReport, error) {
	rep := &VerifyReport{OK: true, Granularity: GranularityClient}
	err := m.withState(ctx, func(st *state) error {
		skills := st.sortedSkills()
		if req.ID != "" {
			sk, ok := st.skill(req.ID)
			if !ok {
				return fmt.Errorf("%w: skill %q", ErrNotFound, req.ID)
			}
			skills = []Skill{sk}
		}
		for _, sk := range skills {
			sv := SkillVerification{SkillID: sk.ID}
			sv.Library, sv.Fingerprint, sv.Detail = m.verifyLibrary(st, sk)
			if p, ok := st.pins.Pins[sk.ID]; ok {
				sv.PinnedFingerprint = p.Fingerprint
			}
			if sv.Library != LibraryOK {
				rep.OK = false
			}
			for _, rec := range st.installsOf(sk.ID) {
				if req.ClientID != "" && rec.ClientID != req.ClientID {
					continue
				}
				view := m.verifyReceipt(st, rec, &sk)
				if view.State != StateApplied {
					rep.OK = false
				}
				sv.Installs = append(sv.Installs, view)
			}
			rep.Skills = append(rep.Skills, sv)
		}
		if req.ID != "" {
			return nil
		}
		// Orphan receipts: a skill vanished from the index without its
		// installs being removed. Never silently dropped — the files are
		// still on a user's disk.
		for _, rec := range st.installs.Installs {
			if _, ok := st.skill(rec.SkillID); ok {
				continue
			}
			if req.ClientID != "" && rec.ClientID != req.ClientID {
				continue
			}
			rep.OK = false
			rep.Skills = append(rep.Skills, SkillVerification{
				SkillID: rec.SkillID, Library: LibraryMissing,
				Detail:   "receipt without a library entry",
				Installs: []InstallView{m.verifyReceipt(st, rec, nil)},
			})
		}
		slices.SortStableFunc(rep.Skills, func(a, b SkillVerification) int { return cmp.Compare(a.SkillID, b.SkillID) })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// verifyReceipt classifies one receipt and writes the result back into the
// stored state.
func (m *Manager) verifyReceipt(st *state, rec InstallState, sk *Skill) InstallView {
	t, ok := m.Target(rec.ClientID)
	if !ok {
		rec.State = StateConflict
		st.putInstall(rec)
		return InstallView{Install: rec, State: StateConflict, Detail: "no target definition for client " + rec.ClientID}
	}
	s, detail := m.verifyOne(rec, sk, t)
	rec.State = s
	st.putInstall(rec)
	return InstallView{Install: rec, State: s, Detail: detail}
}

// verifyLibrary recomputes a library entry's fingerprint from the bytes in
// the store and compares it with the index and the pin.
func (m *Manager) verifyLibrary(st *state, sk Skill) (LibraryState, string, string) {
	path := m.SkillPath(&sk)
	if _, err := os.Stat(path); err != nil {
		return LibraryMissing, "", "library copy is gone: " + err.Error()
	}
	hash, files, err := hashDir(path)
	if err != nil {
		return LibraryMissing, "", "cannot hash library copy: " + err.Error()
	}
	onDisk := sk
	onDisk.Files = files
	fp, err := Fingerprint(SnapshotOf(&onDisk))
	if err != nil {
		return LibraryTampered, "", err.Error()
	}
	p, pinned := st.pins.Pins[sk.ID]
	switch {
	case hash != sk.ContentHash:
		return LibraryTampered, fp, "library files changed outside agenthub"
	case !pinned:
		return LibraryUnpinned, fp, "no fingerprint baseline recorded"
	case p.Fingerprint != fp:
		return LibraryTampered, fp, "library copy does not match its pin"
	default:
		return LibraryOK, fp, ""
	}
}

// pruneVersions keeps the current content-addressed version plus the most
// recent keptVersions-1 older ones (docs/subsystems/skills.md). Old versions are what
// a rollback and a drift diff read from, so pruning to exactly one would
// save space by deleting the evidence.
func (m *Manager) pruneVersions(id, keep string) error {
	// Same reasoning as the removal path: this function deletes, and the ID
	// reaching it came out of a file.
	if err := checkID(id); err != nil {
		return err
	}
	dir := filepath.Join(m.dir, storeDirName, id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	type versionDir struct {
		name string
		mod  int64
	}
	var versions []versionDir
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		versions = append(versions, versionDir{name: e.Name(), mod: info.ModTime().UnixNano()})
	}
	slices.SortFunc(versions, func(a, b versionDir) int {
		return cmp.Or(cmp.Compare(b.mod, a.mod), cmp.Compare(a.name, b.name))
	})
	for i, v := range versions {
		if i < keptVersions-1 {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, v.name)); err != nil {
			return err
		}
	}
	return nil
}

// storeRel is the library-relative path of one content-addressed version.
func storeRel(id, contentHash string) string {
	return storeDirName + "/" + id + "/" + contentHash
}

// maxIDLen bounds an ID so that the deepest path it appears in stays well
// inside every platform's limit.
const maxIDLen = 64

// validID reports whether id is a legal skill ID.
//
// The shape is exactly what slugify produces: 1..64 lowercase ASCII letters
// and digits, single inner dashes, no leading or trailing dash. Everything
// outside that is refused rather than sanitized — a sanitizer has to be
// right about every escaping form, while a shape check only has to be right
// about one.
//
// This is a PATH SAFETY check wearing a shape check's clothes, which is why
// it is this strict:
//
//   - `.` and `..` and every separator are excluded by the character set, so
//     an ID cannot climb out of the store on a copy or take a directory
//     outside it with a removal.
//   - Uppercase is excluded because two IDs differing only in case share one
//     directory on a case-insensitive filesystem while the index counts them
//     as two skills — one skill's files would answer for the other's.
//   - The empty string is excluded because it collapses a join: the store
//     directory itself becomes the version directory, and a removal of it
//     takes every skill.
//
// (internal/shaping's validID is the same discipline for a different id
// space.)
func validID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	if id[0] == '-' || id[len(id)-1] == '-' {
		return false
	}
	for i := range len(id) {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if id[i-1] == '-' {
				return false // no doubled dash; slugify never emits one
			}
		default:
			return false
		}
	}
	return true
}

// checkID is validID as a typed error, for the paths that report one.
func checkID(id string) error {
	if !validID(id) {
		return fmt.Errorf("%w: %q (lowercase letters, digits and single dashes, up to %d characters)",
			ErrInvalidID, id, maxIDLen)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
