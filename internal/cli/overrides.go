package cli

import (
	"github.com/dinstein/agent-hub/internal/confops"
)

// Tool overrides — the local rename / description neutralization of
// docs/modules/controlplane.md (`tool override --name/--desc/--clear`).
//
// The store, its fail direction (an undecodable file is an ERROR, never an
// empty override set) and the raw-name keying live in internal/confops so
// the CLI and the control plane read and write one file with one set of
// rules. What is left here is the CLI's view of them.

// toolOverridesFileName is the state file backing the override store.
const toolOverridesFileName = confops.ToolOverridesFileName

// ToolOverride is one tool's local presentation override.
type ToolOverride = confops.ToolOverride

// toolOverridesFile is the on-disk envelope: serverID -> raw tool -> override.
type toolOverridesFile = confops.ToolOverrides

// loadOverrides reads the override store.
func (a *App) loadOverrides() (toolOverridesFile, error) {
	dir, err := a.stateDir()
	if err != nil {
		return toolOverridesFile{}, err
	}
	doc, lerr := confops.LoadToolOverrides(dir)
	return doc, opsError(lerr)
}
