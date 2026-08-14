package skills

import (
	"fmt"
	"path/filepath"
	"slices"
)

// WriteStrategy is how a target's bytes are managed.
type WriteStrategy string

const (
	// StrategyOwnedDir: agenthub owns a whole directory and may rebuild it.
	// Ownership is proven by MarkerFileName; a directory without the marker
	// is somebody else's and is reported Conflict.
	StrategyOwnedDir WriteStrategy = "owned-dir"
	// StrategySentinelBlock: agenthub owns a BEGIN/END delimited region of
	// a file somebody else owns. Bytes outside the region are preserved
	// verbatim, and damaged markers refuse the write.
	StrategySentinelBlock WriteStrategy = "sentinel-block"
)

// TargetDef declares one materialization target: where a client reads
// skills from, and how agenthub is allowed to write there.
//
// This is the skills counterpart of internal/clients' Format table — same
// library, different table (docs/subsystems/skills.md). Adding a client is a row,
// never a new code path.
type TargetDef struct {
	// ClientID matches the client identifier used everywhere else
	// ("claude-code", "cursor", ...). "generic" is the escape hatch.
	ClientID string
	// DisplayName is the human-facing product name.
	DisplayName string
	// Supports lists the skill kinds this target accepts. A skill of any
	// other kind is refused with ErrUnsupportedKind rather than written
	// somewhere the client will not read it.
	Supports []SkillKind
	Strategy WriteStrategy
	// UserDir resolves the container directory at user scope, given the
	// user's home. Nil means the target has no user-scope convention.
	UserDir func(home string, kind SkillKind) (string, error)
	// ProjectDir resolves the container directory at project scope, given
	// the project root. Nil means no project-scope convention.
	ProjectDir func(root string, kind SkillKind) (string, error)
	// RequiresDir marks a target with no built-in convention: the caller
	// must pass an explicit directory. The generic target is the only
	// built-in one, and it exists so a user can point agenthub at a client
	// agenthub does not know about without waiting for a table row.
	RequiresDir bool
	// SentinelFile is the basename written inside the container by
	// sentinel-block targets.
	SentinelFile string
	// CharCap bounds the WHOLE rendered file, not just our block (the
	// Windsurf 6000-character lesson, docs/subsystems/skills.md: clients truncate the
	// file, so a per-block budget measures the wrong thing). Zero means
	// uncapped. Exceeding it is a Conflict: silently writing a file the
	// client will truncate produces a skill that is present and broken,
	// which is worse than one that is absent and reported.
	CharCap int
	// BlockedIf lists basenames inside the container whose presence shadows
	// what we would write (the AGENTS.override.md lesson). Their presence
	// is a Conflict — writing under a shadow yields a receipt that claims
	// an effect the client never sees.
	BlockedIf []string
}

// supports reports whether the target accepts a skill kind.
func (t TargetDef) supports(k SkillKind) bool {
	return slices.Contains(t.Supports, k)
}

// container resolves the directory this target writes into, for one scope.
func (t TargetDef) container(scope, home, root, override string, kind SkillKind) (string, error) {
	if override != "" {
		return filepath.Clean(override), nil
	}
	if t.RequiresDir {
		return "", fmt.Errorf("skills: target %q has no directory convention: pass an explicit directory", t.ClientID)
	}
	switch scope {
	case ScopeUser:
		if t.UserDir == nil {
			return "", fmt.Errorf("skills: target %q has no user-scope directory", t.ClientID)
		}
		if home == "" {
			return "", fmt.Errorf("skills: target %q at user scope needs a home directory", t.ClientID)
		}
		return t.UserDir(home, kind)
	case ScopeProject:
		if t.ProjectDir == nil {
			return "", fmt.Errorf("skills: target %q has no project-scope directory", t.ClientID)
		}
		if root == "" {
			return "", fmt.Errorf("skills: target %q at project scope needs a project root", t.ClientID)
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		return t.ProjectDir(abs, kind)
	default:
		return "", fmt.Errorf("skills: unknown scope %q (want %q or %q)", scope, ScopeUser, ScopeProject)
	}
}

// GenericTargetID is the target for clients that have no table row yet: the
// caller supplies the directory and gets owned-dir semantics.
const GenericTargetID = "generic"

// builtinTargets is the target table.
//
// Only three rows in M1, on purpose: claude-code is the reference owned-dir
// consumer, cursor the reference sentinel-block one, and generic proves the
// table generalizes without a code change. Growing the table is table data,
// not new logic.
func builtinTargets() []TargetDef {
	return []TargetDef{
		{
			ClientID:    "claude-code",
			DisplayName: "Claude Code",
			Supports:    []SkillKind{KindSkillPack},
			Strategy:    StrategyOwnedDir,
			UserDir: func(home string, _ SkillKind) (string, error) {
				return filepath.Join(home, ".claude", "skills"), nil
			},
			ProjectDir: func(root string, _ SkillKind) (string, error) {
				return filepath.Join(root, ".claude", "skills"), nil
			},
		},
		{
			ClientID:    "cursor",
			DisplayName: "Cursor",
			Supports:    []SkillKind{KindSkillPack, KindCommand},
			Strategy:    StrategySentinelBlock,
			UserDir: func(home string, _ SkillKind) (string, error) {
				return filepath.Join(home, ".cursor", "rules"), nil
			},
			ProjectDir: func(root string, _ SkillKind) (string, error) {
				return filepath.Join(root, ".cursor", "rules"), nil
			},
			SentinelFile: "agenthub.mdc",
		},
		{
			ClientID:    GenericTargetID,
			DisplayName: "Generic directory",
			Supports:    []SkillKind{KindSkillPack, KindCommand, KindAgentDef},
			Strategy:    StrategyOwnedDir,
			RequiresDir: true,
		},
	}
}

// targetTable builds the effective table: built-ins plus extras, extras
// winning on ClientID collision.
func targetTable(extra []TargetDef) map[string]TargetDef {
	out := map[string]TargetDef{}
	for _, t := range builtinTargets() {
		out[t.ClientID] = t
	}
	for _, t := range extra {
		out[t.ClientID] = t
	}
	return out
}

// Target returns the target definition for a client ID.
func (m *Manager) Target(clientID string) (TargetDef, bool) {
	t, ok := m.targets[clientID]
	return t, ok
}
