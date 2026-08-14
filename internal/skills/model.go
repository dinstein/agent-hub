package skills

import (
	"encoding/json"
	"slices"
	"time"
)

// SkillKind is the shape of the asset in the library.
type SkillKind string

const (
	// KindSkillPack is a SKILL.md directory package (the M1 default).
	KindSkillPack SkillKind = "skill"
	// KindCommand is a single-file slash command.
	KindCommand SkillKind = "command"
	// KindAgentDef is a subagent definition file.
	KindAgentDef SkillKind = "agent"
)

// SourceKind records where a library entry came from.
type SourceKind string

const (
	// SourceLocal is an import from a directory on this machine.
	SourceLocal SourceKind = "local"
	// SourceGit is an import whose provenance is a git repository. The
	// files are still read from a local checkout — this package performs no
	// git operations (see package doc; fetch is M2).
	SourceGit SourceKind = "git"
)

// Source is the provenance of a library entry. It is descriptive: nothing
// in this package re-reads Path automatically, so a moved or deleted source
// directory can never silently change the library copy.
type Source struct {
	Kind SourceKind `json:"kind"`
	// Path is the directory the files were imported from (provenance and
	// the default for `skill update` on a local source).
	Path   string `json:"path,omitempty"`
	GitURL string `json:"gitUrl,omitempty"`
	// GitRef is the requested revision (tag/branch/commit) as the user
	// spelled it in --pin.
	GitRef string `json:"gitRef,omitempty"`
	// PinnedCommit is the resolved commit the import actually came from,
	// when the caller could supply it. Recorded, never resolved here.
	PinnedCommit string `json:"pinnedCommit,omitempty"`
}

// FileEntry is one file of a skill package. Path is always a slash-separated
// package-relative path; "..", absolute paths, symlinks and non-regular
// files are rejected at import time and can therefore never appear here.
type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Skill is one library entry.
//
// Enabled (not Disabled) is the on-disk spelling on purpose: a
// hand-written or truncated record that omits the field reads as DISABLED,
// which is the fail-closed direction for "should agenthub push these bytes
// into a client's directory". Add always writes the field explicitly.
type Skill struct {
	// ID is the slugified name, deduplicated with a "-2" suffix
	// (ServerEntry's id rule).
	ID string `json:"id"`
	// Name comes from the SKILL.md frontmatter, falling back to the source
	// directory name.
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Kind        SkillKind `json:"kind"`
	// Version is the frontmatter version, or "0.0.0+<hash8>" derived from
	// ContentHash when the package does not declare one.
	Version string `json:"version"`
	Source  Source `json:"source"`
	// Path is the library location of the canonical copy, RELATIVE to the
	// skills directory ("store/<id>/<contentHash>") so the store can be
	// relocated. Use Manager.SkillPath for an absolute path. Distinct from
	// Source.Path, which is where the files came FROM.
	Path string `json:"path"`
	// ContentHash is the sha256 over the sorted file list; it is the CAS
	// directory name.
	ContentHash string `json:"contentHash"`
	// Fingerprint is "v1:<sha256>" over content AND metadata (name,
	// description, kind) — strictly wider than ContentHash. It is what the
	// pin store baselines and what Verify compares against.
	Fingerprint string      `json:"fingerprint"`
	Files       []FileEntry `json:"files"`
	Enabled     bool        `json:"enabled"`
	AddedAt     time.Time   `json:"addedAt"`
	UpdatedAt   time.Time   `json:"updatedAt"`

	// Unknown preserves fields written by a newer binary so a load-modify-
	// save cycle by an older one does not drop them (the registry envelope
	// discipline, applied per record).
	Unknown map[string]json.RawMessage `json:"-"`
}

// skillAlias avoids infinite recursion in the custom JSON methods.
type skillAlias Skill

// MarshalJSON emits the known fields plus any preserved unknown ones.
// Known fields always win: an unknown entry that collides with a known key
// is dropped rather than allowed to shadow real state.
func (s Skill) MarshalJSON() ([]byte, error) {
	known, err := json.Marshal(skillAlias(s))
	if err != nil {
		return nil, err
	}
	if len(s.Unknown) == 0 {
		return known, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(known, &merged); err != nil {
		return nil, err
	}
	for k, v := range s.Unknown {
		if _, clash := merged[k]; clash {
			continue
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

// UnmarshalJSON decodes the known fields and captures every other key into
// Unknown.
func (s *Skill) UnmarshalJSON(b []byte) error {
	var a skillAlias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*s = Skill(a)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, known := range knownSkillFields {
		delete(raw, known)
	}
	if len(raw) > 0 {
		s.Unknown = raw
	}
	return nil
}

// knownSkillFields lists the JSON keys Skill itself owns. It must stay in
// sync with the struct tags above; TestKnownSkillFieldsCoverStruct enforces
// that by reflection so a new field cannot silently land in Unknown.
var knownSkillFields = []string{
	"id", "name", "description", "kind", "version", "source", "path",
	"contentHash", "fingerprint", "files", "enabled", "addedAt", "updatedAt",
}

// ApplyState is the receipt state of one installation.
//
// Orthogonal to internal/integrity's ToolState (7.12 #19): this axis is
// about bytes on disk, that one is about trust. They are stored in separate
// fields and neither transition implies the other.
//
// docs/subsystems/skills.md listed six states; this package ships five. "blocked" and
// "removed" are folded in: a target we may not write is StateConflict
// (blocked is one of its causes) and a removed install has no receipt at
// all, so it needs no state value.
type ApplyState string

const (
	// StateApplied: disk matches the receipt and the receipt matches the
	// current library copy. Nothing to do.
	StateApplied ApplyState = "applied"
	// StateStale: disk still matches the receipt, but the library moved on
	// (the skill was updated). Re-install to converge.
	StateStale ApplyState = "stale"
	// StateDrifted: disk no longer matches the receipt — a third party
	// edited the materialized copy. Never silently overwritten: a caller
	// must ask for it explicitly.
	StateDrifted ApplyState = "drifted"
	// StateMissing: the receipt exists, the files do not.
	StateMissing ApplyState = "missing"
	// StateConflict: the target is not ours to write — an owned dir without
	// our marker, damaged sentinels, a shadowing file, an over-cap render,
	// or a receipt whose library entry vanished. Writes are refused.
	StateConflict ApplyState = "conflict"
)

// Scope values of InstallState.Scope. Only these two exist: file
// materialization has no session tier (see package doc).
const (
	ScopeUser    = "user"
	ScopeProject = "project"
)

// GranularityClient is the only granularity file materialization reaches.
// It is echoed in every result value so callers cannot forget to say so.
const GranularityClient = "client"

// InstallState is the receipt for one materialization.
type InstallState struct {
	SkillID  string `json:"skillId"`
	ClientID string `json:"clientId"`
	Scope    string `json:"scope"`
	// Container is the resolved directory the target convention put us in
	// (e.g. <root>/.claude/skills). It is part of the receipt identity, so
	// the same skill installed into two projects yields two receipts.
	Container string `json:"container"`
	// ProjectRoot is the project this receipt belongs to ("" at user scope).
	ProjectRoot string `json:"projectRoot,omitempty"`
	// Path is the directory (owned-dir) or file (sentinel-block) written.
	Path     string        `json:"path"`
	Strategy WriteStrategy `json:"strategy"`
	// SourceHash is the library ContentHash at apply time; SourceHash !=
	// the library's current ContentHash means Stale.
	SourceHash string `json:"sourceHash"`
	// InstalledHash is the hash of what was actually written: the content
	// hash of the materialized files (owned-dir) or the sha256 of the
	// sentinel block body (sentinel-block). A mismatch against disk means
	// Drifted.
	InstalledHash string     `json:"installedHash"`
	State         ApplyState `json:"state"`
	AppliedAt     time.Time  `json:"appliedAt"`
	// Granularity is always GranularityClient — see package doc.
	Granularity string `json:"granularity"`
}

// key is the receipt identity used to index installs in memory.
func (s InstallState) key() installKey {
	return installKey{SkillID: s.SkillID, ClientID: s.ClientID, Scope: s.Scope, Container: s.Container}
}

type installKey struct {
	SkillID   string
	ClientID  string
	Scope     string
	Container string
}

// Pin is one library entry's fingerprint baseline (skill-pins.json).
// Merge never deletes: a pin for a removed skill is kept so a later
// re-add is compared against the original baseline instead of being
// re-pinned blind (integrity.rs' rule).
type Pin struct {
	Fingerprint       string    `json:"fingerprint"`
	HashSchemaVersion string    `json:"hashSchemaVersion"`
	FirstSeen         time.Time `json:"firstSeen"`
	LastChanged       time.Time `json:"lastChanged"`
}

// SkillSelector is the three-state skill narrowing selector, mirroring the
// tool selector semantics of docs/model.md so the skill scope chain behaves
// exactly like the server/tool one:
//
//	nil *SkillSelector -> no intervention (every enabled skill)
//	Allow == nil       -> every enabled skill
//	Allow == []        -> block-all
//	Allow == [ids...]  -> narrow to that subset
//
// A selector can only narrow. It never enables a skill whose library entry
// has Enabled=false.
type SkillSelector struct {
	Allow []string `json:"allow"`
}

// selects reports whether id passes the selector.
func (s *SkillSelector) selects(id string) bool {
	if s == nil || s.Allow == nil {
		return true
	}
	return slices.Contains(s.Allow, id)
}

// unmaterializedFiles lists the bundled files a delivery does not carry —
// everything but SKILL.md itself — in a stable order.
//
// Both skill documents name them, for the honesty reason renderSkillDocument
// gives: a reader who is not told about the rest believes it has the whole
// package. They collected the list separately, and only one of them sorted.
//
// The sort is not defensive decoration. Every builder of Skill.Files sorts by
// path today, but this list is rendered into the sentinel block that
// verifyOne compares byte for byte against disk — so an order that stopped
// being stable would report every installed skill as Drifted and stop
// automated writes, with the cause three files away in whichever builder
// dropped its sort.
func unmaterializedFiles(sk *Skill) []string {
	var out []string
	for _, f := range sk.Files {
		if f.Path != SkillFileName {
			out = append(out, f.Path)
		}
	}
	slices.Sort(out)
	return out
}
