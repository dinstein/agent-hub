package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dinstein/agent-hub/internal/platform"
)

// marker is the content of MarkerFileName: the proof that agenthub owns an
// installed directory, plus enough state for Verify to take a fast path.
//
// Ownership is proven by this file and nothing else. Path conventions,
// naming patterns and receipts are all things a user could coincidentally
// reproduce; an explicit marker cannot be produced by accident.
type marker struct {
	Version     int       `json:"version"`
	ManagedBy   string    `json:"managedBy"`
	SkillID     string    `json:"skillId"`
	SkillName   string    `json:"skillName"`
	ContentHash string    `json:"contentHash"`
	Fingerprint string    `json:"fingerprint"`
	AppliedAt   time.Time `json:"appliedAt"`
	// AgentVersion records which agenthub build wrote the directory.
	AgentVersion string `json:"agentVersion,omitempty"`
	// Granularity restates the honest tier inside the artifact itself, so
	// somebody reading the directory without the docs still learns that
	// this materialization is per-client, never per-session.
	Granularity string `json:"granularity"`
}

const markerManagedBy = "agenthub"

// InstallRequest addresses one materialization.
type InstallRequest struct {
	SkillID  string
	ClientID string
	// Scope is ScopeUser (default) or ScopeProject.
	Scope string
	// ProjectRoot is required at project scope.
	ProjectRoot string
	// Dir overrides the target's directory convention; required for the
	// generic target.
	Dir string
	// AllowDrift permits overwriting a copy that was edited outside
	// agenthub. Without it a drifted target refuses the write (ErrDrifted).
	AllowDrift bool
}

func (r InstallRequest) scope() string {
	if r.Scope == "" {
		return ScopeUser
	}
	return r.Scope
}

// InstallPlan is what Apply would do, computed without writing anything.
// It is also the shape `skill install-to --dry-run` renders.
type InstallPlan struct {
	SkillID     string        `json:"skillId"`
	ClientID    string        `json:"clientId"`
	Scope       string        `json:"scope"`
	ProjectRoot string        `json:"projectRoot,omitempty"`
	Container   string        `json:"container"`
	Path        string        `json:"path"`
	Strategy    WriteStrategy `json:"strategy"`
	SourceHash  string        `json:"sourceHash"`
	// State is the target's CURRENT state (before applying).
	State ApplyState `json:"state"`
	// Detail explains State in one human-readable clause.
	Detail string `json:"detail,omitempty"`
	// Changed reports whether Apply would write anything.
	Changed bool `json:"changed"`
	// Granularity is always GranularityClient.
	Granularity string `json:"granularity"`
}

// home resolves the user home used by user-scope target conventions.
func (m *Manager) home() (string, error) {
	if m.opts.HomeDir != "" {
		return m.opts.HomeDir, nil
	}
	return os.UserHomeDir()
}

// resolve computes the container directory and the concrete path a target
// writes for one skill.
func (m *Manager) resolve(sk *Skill, t TargetDef, req InstallRequest) (container, path string, err error) {
	if !t.supports(sk.Kind) {
		return "", "", fmt.Errorf("%w: %s does not accept kind %q", ErrUnsupportedKind, t.ClientID, sk.Kind)
	}
	home := ""
	if req.Dir == "" && !t.RequiresDir && req.scope() == ScopeUser {
		if home, err = m.home(); err != nil {
			return "", "", err
		}
	}
	container, err = t.container(req.scope(), home, req.ProjectRoot, req.Dir, sk.Kind)
	if err != nil {
		return "", "", err
	}
	switch t.Strategy {
	case StrategyOwnedDir:
		return container, filepath.Join(container, sk.ID), nil
	case StrategySentinelBlock:
		if t.SentinelFile == "" {
			return "", "", fmt.Errorf("skills: target %q is sentinel-block but declares no file", t.ClientID)
		}
		return container, filepath.Join(container, t.SentinelFile), nil
	default:
		return "", "", fmt.Errorf("skills: target %q has unknown strategy %q", t.ClientID, t.Strategy)
	}
}

// receiptFor builds the receipt identity of a request.
func receiptKey(skillID, clientID, scope, container string) installKey {
	return installKey{SkillID: skillID, ClientID: clientID, Scope: scope, Container: container}
}

// plan computes the current state of a target without writing.
func (m *Manager) plan(st *state, sk *Skill, t TargetDef, req InstallRequest) (*InstallPlan, error) {
	container, path, err := m.resolve(sk, t, req)
	if err != nil {
		return nil, err
	}
	p := &InstallPlan{
		SkillID: sk.ID, ClientID: t.ClientID, Scope: req.scope(),
		ProjectRoot: req.ProjectRoot, Container: container, Path: path,
		Strategy: t.Strategy, SourceHash: sk.ContentHash,
		Granularity: GranularityClient,
	}
	rec, _, has := st.install(receiptKey(sk.ID, t.ClientID, req.scope(), container))
	if !has {
		// No receipt: the only question is whether the target is free.
		p.State, p.Detail = m.probeUnmanaged(sk, t, container, path)
		p.Changed = p.State != StateConflict
		return p, nil
	}
	p.State, p.Detail = m.verifyOne(rec, sk, t)
	p.Changed = p.State != StateApplied && p.State != StateConflict
	return p, nil
}

// probeUnmanaged classifies a target we hold no receipt for. It answers one
// question: may we write here at all?
func (m *Manager) probeUnmanaged(sk *Skill, t TargetDef, container, path string) (ApplyState, string) {
	if state, detail, blocked := blockedBy(t, container); blocked {
		return state, detail
	}
	switch t.Strategy {
	case StrategyOwnedDir:
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return StateMissing, "not installed"
			}
			return StateConflict, "cannot stat target: " + err.Error()
		}
		mk, err := readMarker(path)
		if err != nil {
			return StateConflict, "directory exists without an agenthub marker: " + err.Error()
		}
		if mk.SkillID != sk.ID {
			return StateConflict, fmt.Sprintf("directory is managed for a different skill (%s)", mk.SkillID)
		}
		// Ours by marker but with no receipt: adoptable, treat as missing
		// so Apply rebuilds it and re-records the receipt.
		return StateMissing, "managed directory without a receipt (will be re-recorded)"
	case StrategySentinelBlock:
		content, err := readFileString(path)
		if err != nil {
			return StateConflict, "cannot read target file: " + err.Error()
		}
		if _, _, err := findBlock(content, sk.ID, path); err != nil {
			return StateConflict, err.Error()
		}
		return StateMissing, "not installed"
	default:
		return StateConflict, "unknown write strategy"
	}
}

// blockedBy reports a shadowing file in the container (docs/modules/config.md:
// the AGENTS.override.md lesson — a file the client prefers makes our write
// invisible, and an invisible write with a healthy receipt is a lie).
func blockedBy(t TargetDef, container string) (ApplyState, string, bool) {
	for _, name := range t.BlockedIf {
		if _, err := os.Stat(filepath.Join(container, name)); err == nil {
			return StateConflict, "shadowed by " + name, true
		}
	}
	return "", "", false
}

// apply materializes one skill and returns the receipt. It never runs
// against a target whose plan says Conflict, and never overwrites a Drifted
// target unless the caller asked for it.
func (m *Manager) apply(st *state, sk *Skill, t TargetDef, req InstallRequest, p *InstallPlan) (InstallState, error) {
	switch p.State {
	case StateConflict:
		return InstallState{}, &ConflictError{Path: p.Path, Reason: p.Detail}
	case StateDrifted:
		if !req.AllowDrift {
			return InstallState{}, &DriftError{SkillID: sk.ID, Path: p.Path}
		}
	}

	var installedHash string
	var err error
	switch t.Strategy {
	case StrategyOwnedDir:
		installedHash, err = m.applyOwnedDir(sk, p.Path)
	case StrategySentinelBlock:
		installedHash, err = m.applySentinel(sk, t, p.Path)
	default:
		err = fmt.Errorf("skills: target %q has unknown strategy %q", t.ClientID, t.Strategy)
	}
	if err != nil {
		return InstallState{}, err
	}

	rec := InstallState{
		SkillID: sk.ID, ClientID: t.ClientID, Scope: req.scope(),
		Container: p.Container, ProjectRoot: req.ProjectRoot, Path: p.Path,
		Strategy: t.Strategy, SourceHash: sk.ContentHash, InstalledHash: installedHash,
		State: StateApplied, AppliedAt: m.now(), Granularity: GranularityClient,
	}
	st.putInstall(rec)
	return rec, nil
}

// applyOwnedDir rebuilds the whole target directory from the library copy.
//
// Rebuild, not merge: the directory is ours end to end, so a stray file
// left by an older version — or by a half-finished previous write — must
// not survive. The ownership marker is checked BEFORE the removal and
// written last, so a crash mid-write leaves a directory that verifies as
// Drifted (repairable) rather than one that looks complete.
func (m *Manager) applyOwnedDir(sk *Skill, path string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		mk, err := readMarker(path)
		if err != nil {
			return "", &ConflictError{Path: path, Reason: "directory exists without an agenthub marker"}
		}
		if mk.SkillID != sk.ID {
			return "", &ConflictError{Path: path, Reason: "directory is managed for skill " + mk.SkillID}
		}
		if err := os.RemoveAll(path); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := copyTree(m.SkillPath(sk), path, sk.Files); err != nil {
		return "", err
	}
	mk := marker{
		Version: storeVersion, ManagedBy: markerManagedBy,
		SkillID: sk.ID, SkillName: sk.Name,
		ContentHash: sk.ContentHash, Fingerprint: sk.Fingerprint,
		AppliedAt: m.now(), AgentVersion: m.opts.AgentVersion,
		Granularity: GranularityClient,
	}
	b, err := json.MarshalIndent(mk, "", "  ")
	if err != nil {
		return "", err
	}
	if err := atomicWrite(filepath.Join(path, MarkerFileName), append(b, '\n')); err != nil {
		return "", err
	}
	hash, _, err := hashDir(path)
	return hash, err
}

// applySentinel writes the skill's block into a file agenthub does not own.
//
// The previous content is backed up first, the bytes outside the sentinels
// are preserved verbatim, and a rendered file over the target's CharCap is
// refused (a client that truncates the file would leave a half skill in
// place, which is worse than none).
func (m *Manager) applySentinel(sk *Skill, t TargetDef, path string) (string, error) {
	body, err := m.renderSkillBody(sk)
	if err != nil {
		return "", err
	}
	old, err := readFileString(path)
	if err != nil {
		return "", err
	}
	next, err := upsertBlock(old, sk.ID, body, path)
	if err != nil {
		return "", err
	}
	if t.CharCap > 0 && utf8.RuneCountInString(next) > t.CharCap {
		return "", &ConflictError{
			Path:   path,
			Reason: fmt.Sprintf("rendered file would be %d characters, over the %d character cap of %s", utf8.RuneCountInString(next), t.CharCap, t.ClientID),
		}
	}
	if err := platform.EnsureDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	if _, err := m.backupFile(t.ClientID, path); err != nil {
		return "", err
	}
	if err := atomicWrite(path, []byte(next)); err != nil {
		return "", err
	}
	span, found, err := findBlock(next, sk.ID, path)
	if err != nil || !found {
		return "", fmt.Errorf("skills: wrote %s but cannot find the block back (%v)", path, err)
	}
	return hashBytes([]byte(span.body)), nil
}

// renderSkillBody renders a skill for a sentinel-block target.
//
// Deterministic by construction (golden-tested): fixed heading, fixed
// preamble, the SKILL.md body verbatim, then a sorted attachment note.
// Attachments are NOT materialized by this strategy — a single shared file
// cannot hold them — and saying so in the rendered text is the honest
// alternative to pretending the install is complete.
func (m *Manager) renderSkillBody(sk *Skill) (string, error) {
	var body string
	raw, err := os.ReadFile(filepath.Join(m.SkillPath(sk), SkillFileName))
	switch {
	case err == nil:
		meta, perr := ParseSkillMD(raw)
		if perr != nil {
			return "", perr
		}
		body = strings.TrimRight(meta.Body, "\n")
	case errors.Is(err, os.ErrNotExist):
		body = ""
	default:
		return "", err
	}

	var b strings.Builder
	b.WriteString("<!-- managed by agenthub; edits inside this block are overwritten on sync -->\n")
	fmt.Fprintf(&b, "# %s (%s)\n", sk.Name, sk.Version)
	if sk.Description != "" {
		b.WriteString("\n" + sk.Description + "\n")
	}
	if body != "" {
		b.WriteString("\n" + body + "\n")
	}
	var attach []string
	for _, f := range sk.Files {
		if f.Path != SkillFileName {
			attach = append(attach, f.Path)
		}
	}
	if len(attach) > 0 {
		fmt.Fprintf(&b, "\n_Attachments kept in the agenthub library, not materialized by this strategy: %s_\n",
			strings.Join(attach, ", "))
	}
	return b.String(), nil
}

// verifyOne classifies one receipt against disk and against the library.
//
// Precedence — most actionable first, and each level answers a different
// question: may we write here (Conflict) > are the bytes there (Missing) >
// are they ours (Drifted) > are they current (Stale) > Applied.
//
// sk may be nil, meaning the library entry behind the receipt is gone. That
// is a Conflict, not a Missing: something removed a skill without removing
// its installs, and automated writes must stop until a human looks.
func (m *Manager) verifyOne(rec InstallState, sk *Skill, t TargetDef) (ApplyState, string) {
	if state, detail, blocked := blockedBy(t, rec.Container); blocked {
		return state, detail
	}
	switch rec.Strategy {
	case StrategyOwnedDir:
		if _, err := os.Stat(rec.Path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return StateMissing, "installed directory is gone"
			}
			return StateConflict, "cannot stat installed directory: " + err.Error()
		}
		mk, err := readMarker(rec.Path)
		if err != nil {
			return StateConflict, "installed directory lost its agenthub marker: " + err.Error()
		}
		if mk.SkillID != rec.SkillID {
			return StateConflict, fmt.Sprintf("directory is now managed for %s", mk.SkillID)
		}
		hash, _, err := hashDir(rec.Path)
		if err != nil {
			return StateConflict, "cannot hash installed directory: " + err.Error()
		}
		if hash != rec.InstalledHash {
			return StateDrifted, "installed files were modified outside agenthub"
		}
	case StrategySentinelBlock:
		content, err := readFileString(rec.Path)
		if err != nil {
			return StateConflict, "cannot read target file: " + err.Error()
		}
		if content == "" {
			return StateMissing, "target file is gone"
		}
		span, found, err := findBlock(content, rec.SkillID, rec.Path)
		if err != nil {
			return StateConflict, err.Error()
		}
		if !found {
			return StateMissing, "sentinel block is gone"
		}
		if hashBytes([]byte(span.body)) != rec.InstalledHash {
			return StateDrifted, "sentinel block body was modified outside agenthub"
		}
	default:
		return StateConflict, "unknown write strategy"
	}
	if sk == nil {
		return StateConflict, "library entry for this receipt no longer exists"
	}
	if rec.SourceHash != sk.ContentHash {
		return StateStale, "library copy has been updated since this install"
	}
	return StateApplied, ""
}

// removeMaterialized deletes only what agenthub owns.
//
// Owned dir: removed only when our marker is present. Sentinel block: the
// block is cut and every other byte is written back unchanged, so a file
// that also holds a user's own rules survives intact. Anything else is a
// conflict and nothing is touched.
func (m *Manager) removeMaterialized(rec InstallState, t TargetDef) error {
	switch rec.Strategy {
	case StrategyOwnedDir:
		if _, err := os.Stat(rec.Path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		mk, err := readMarker(rec.Path)
		if err != nil {
			return &ConflictError{Path: rec.Path, Reason: "no agenthub marker; refusing to delete a directory we cannot prove is ours"}
		}
		if mk.SkillID != rec.SkillID {
			return &ConflictError{Path: rec.Path, Reason: "marker names skill " + mk.SkillID}
		}
		return os.RemoveAll(rec.Path)
	case StrategySentinelBlock:
		content, err := readFileString(rec.Path)
		if err != nil {
			return err
		}
		if content == "" {
			return nil
		}
		next, removed, err := removeBlockFrom(content, rec.SkillID, rec.Path)
		if err != nil {
			return err
		}
		if !removed {
			return nil
		}
		if _, err := m.backupFile(t.ClientID, rec.Path); err != nil {
			return err
		}
		// A file that now holds nothing but our removed block is deleted
		// rather than left as an empty artifact we created.
		if strings.TrimSpace(next) == "" {
			return os.Remove(rec.Path)
		}
		return atomicWrite(rec.Path, []byte(next))
	default:
		return fmt.Errorf("skills: unknown write strategy %q", rec.Strategy)
	}
}

// readMarker reads and validates an ownership marker.
func readMarker(dir string) (marker, error) {
	var mk marker
	b, err := os.ReadFile(filepath.Join(dir, MarkerFileName))
	if err != nil {
		return mk, err
	}
	if err := json.Unmarshal(b, &mk); err != nil {
		return mk, err
	}
	if mk.ManagedBy != markerManagedBy {
		return mk, fmt.Errorf("marker managedBy is %q, not %q", mk.ManagedBy, markerManagedBy)
	}
	if mk.SkillID == "" {
		return mk, errors.New("marker has no skillId")
	}
	return mk, nil
}

// readFileString reads a file, mapping "does not exist" to the empty string
// (an absent shared file and an empty one are the same starting point).
func readFileString(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}
