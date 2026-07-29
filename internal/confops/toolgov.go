package confops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dinstein/agent-hub/internal/integrity"
	"github.com/dinstein/agent-hub/internal/registry"
)

// Tool-level governance writes the <state> stores the gateway and the daemon
// also consult, so a decision taken here is the decision the call plane
// enforces.
//
// Everything here works OFFLINE from the gateway's tool cache: the operator
// must be able to disable a suspicious tool without first starting the
// server that serves it. That is the whole point of a kill switch.
//
// Precondition caveat: these stores are NOT the registry, so their writes do
// not move the registry generation and a precondition on them is checked
// against a snapshot (see Precondition.checkSnapshot) — it catches a stale
// operator view, not a concurrent write to these files, which their own
// cross-process locks serialize.

// StateOptions locates the state directory and how long to wait for its
// cross-process locks.
type StateOptions struct {
	Dir         string
	LockTimeout time.Duration
}

// ToolSnapshotFunc resolves a tool the operator names but that no live
// connection has ever recorded, typically out of the gateway's persisted
// catalog cache. Returning ok == false means "not in the cache either".
type ToolSnapshotFunc func(server, tool string) (integrity.ToolSnapshot, bool, error)

// ToolStateResult is what SetToolEnabled returns.
type ToolStateResult struct {
	Result
	Record integrity.ApprovalRecord
}

// SetToolEnabled is the operator's kill switch. It is orthogonal to the
// approval state (Disabled is a flag, not a state), so switching a tool off
// and back on never discards an approval — and never grants one either.
//
// When no approval record exists yet, one is materialized from lookup's
// cached definition in ModeManual, i.e. Pending: creating a record through
// an operator command must not become a way to grant trust as a side effect.
// Without that fallback the kill switch would require starting the
// suspicious server first, which is exactly backwards.
func SetToolEnabled(
	ctx context.Context, st *registry.Store, opt StateOptions,
	server, tool string, enabled bool, lookup ToolSnapshotFunc, pre Precondition,
) (ToolStateResult, error) {
	if opt.Dir == "" {
		return ToolStateResult{}, usagef("no state directory")
	}
	if err := pre.checkSnapshot(st); err != nil {
		return ToolStateResult{}, err
	}
	store, err := integrity.OpenApprovalStore(opt.Dir, integrity.Options{LockTimeout: opt.LockTimeout})
	if err != nil {
		return ToolStateResult{}, err
	}
	rec, err := store.SetDisabled(ctx, server, tool, !enabled)
	if errors.Is(err, integrity.ErrNotFound) && lookup != nil {
		rec, err = observeThenSetDisabled(ctx, store, server, tool, !enabled, lookup)
	}
	out := ToolStateResult{}
	if st != nil {
		out.Generation = st.Snapshot().Generation
	}
	if err != nil {
		return out, err
	}
	out.Record = rec
	out.Changed = rec.Disabled == !enabled
	return out, nil
}

// observeThenSetDisabled creates the missing approval record from the cached
// catalog and then applies the flag.
func observeThenSetDisabled(
	ctx context.Context, store *integrity.ApprovalStore,
	server, tool string, disable bool, lookup ToolSnapshotFunc,
) (integrity.ApprovalRecord, error) {
	snap, ok, err := lookup(server, tool)
	if err != nil {
		return integrity.ApprovalRecord{}, err
	}
	if !ok {
		return integrity.ApprovalRecord{}, integrity.ErrNotFound
	}
	if _, err := store.Observe(ctx, server, snap, integrity.ModeManual); err != nil {
		return integrity.ApprovalRecord{}, err
	}
	return store.SetDisabled(ctx, server, tool, disable)
}

// ToolOverride is one tool's local presentation override. An empty field
// means "no override for this aspect"; the pair is cleared as a unit.
type ToolOverride struct {
	// Name replaces the RAW tool name before namespacing, i.e. it changes
	// the exposed name a client sees.
	Name string `json:"name,omitempty"`
	// Description replaces the downstream description verbatim. This is the
	// neutralization path for a poisoned description: the original stays on
	// the downstream, agenthub simply stops forwarding it.
	Description string `json:"description,omitempty"`
}

// Empty reports whether the override carries nothing and should be dropped.
func (o ToolOverride) Empty() bool { return o.Name == "" && o.Description == "" }

// ToolOverrides is the on-disk envelope of <state>/tool-overrides.json:
// serverID -> RAW tool name -> override.
//
// Raw names are the key on purpose: an override keyed on the exposed name
// would move out from under itself the moment it renamed a tool.
type ToolOverrides struct {
	Version   int                                `json:"version"`
	Overrides map[string]map[string]ToolOverride `json:"overrides"`
}

// ToolOverridesFileName is the state file backing ToolOverrides.
const ToolOverridesFileName = "tool-overrides.json"

// ToolOverridesPath resolves the override store inside a state directory.
func ToolOverridesPath(stateDir string) string {
	return filepath.Join(stateDir, ToolOverridesFileName)
}

// LoadToolOverrides reads the override store.
//
// Failure direction: a missing file is an EMPTY store, but an undecodable
// one is an ERROR. Reading "no overrides" out of a corrupt file would
// silently restore a poisoned description the operator had neutralized.
func LoadToolOverrides(stateDir string) (ToolOverrides, error) {
	path := ToolOverridesPath(stateDir)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ToolOverrides{Version: 1, Overrides: map[string]map[string]ToolOverride{}}, nil
		}
		return ToolOverrides{}, err
	}
	var doc ToolOverrides
	if uerr := json.Unmarshal(b, &doc); uerr != nil {
		return ToolOverrides{}, &Error{
			Kind: KindState, Code: CodeStateCorrupt,
			Message: fmt.Sprintf("%s is unreadable: %v", path, uerr),
			Hint:    "inspect the file; agenthub refuses to treat a corrupt override store as 'no overrides'",
		}
	}
	if doc.Overrides == nil {
		doc.Overrides = map[string]map[string]ToolOverride{}
	}
	return doc, nil
}

// SaveToolOverrides writes the store atomically, pruning emptied entries so
// the file stays a faithful listing of what is actually overridden.
func SaveToolOverrides(stateDir string, doc ToolOverrides) error {
	for server, tools := range doc.Overrides {
		for tool, ov := range tools {
			if ov.Empty() {
				delete(tools, tool)
			}
		}
		if len(tools) == 0 {
			delete(doc.Overrides, server)
		}
	}
	doc.Version = 1
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return atomicWriteJSON(ToolOverridesPath(stateDir), doc)
}

// ForgetServerOverrides drops every tool override of one server — the
// cleanup half of RemoveServer. stateDir may be empty (no state directory
// resolved), which is a no-op: there is no store to clean.
//
// Failure direction differs from the other cleanups on purpose: a corrupt
// store propagates the error from LoadToolOverrides rather than being
// rewritten, because overriding is how a poisoned description is
// neutralized and blindly rewriting that file could restore one.
// RemoveServer turns the error into a warning; the server is already gone.
func ForgetServerOverrides(stateDir, serverID string) error {
	if stateDir == "" {
		return nil
	}
	doc, err := LoadToolOverrides(stateDir)
	if err != nil {
		return err
	}
	if _, ok := doc.Overrides[serverID]; !ok {
		return nil
	}
	delete(doc.Overrides, serverID)
	return SaveToolOverrides(stateDir, doc)
}

// ToolOverrideEdit is one override edit. A nil field is left untouched, so
// blanking a description cannot silently drop a rename.
type ToolOverrideEdit struct {
	Name        *string
	Description *string
	// Clear removes the override entirely. It is exclusive with the two
	// field edits.
	Clear bool
}

// ToolOverrideResult is what SetToolOverride returns.
type ToolOverrideResult struct {
	Result
	Server   string
	Tool     string
	Override ToolOverride
	Cleared  bool
}

// SetToolOverride renames a tool locally or replaces a poisoned description.
//
// --desc is the neutralization path for a prompt-injection carrier: the
// downstream keeps its description, agenthub simply stops forwarding it.
func SetToolOverride(
	ctx context.Context, st *registry.Store, stateDir, server, tool string,
	edit ToolOverrideEdit, pre Precondition,
) (ToolOverrideResult, error) {
	_ = ctx
	if stateDir == "" {
		return ToolOverrideResult{}, usagef("no state directory")
	}
	if server == "" || tool == "" {
		return ToolOverrideResult{}, usagef("a server id and a tool name are required")
	}
	if edit.Clear && (edit.Name != nil || edit.Description != nil) {
		return ToolOverrideResult{}, usagef("clearing an override cannot be combined with a name or description edit")
	}
	if !edit.Clear && edit.Name == nil && edit.Description == nil {
		return ToolOverrideResult{}, usagef("nothing to override: set a name, a description, or clear")
	}
	if err := pre.checkSnapshot(st); err != nil {
		return ToolOverrideResult{}, err
	}
	out := ToolOverrideResult{Server: server, Tool: tool}
	if st != nil {
		out.Generation = st.Snapshot().Generation
	}
	doc, err := LoadToolOverrides(stateDir)
	if err != nil {
		return out, err
	}
	if edit.Clear {
		if _, ok := doc.Overrides[server][tool]; !ok {
			return out, notFoundf(CodeToolNotFound, "no override for %s/%s", server, tool)
		}
		delete(doc.Overrides[server], tool)
		if err := SaveToolOverrides(stateDir, doc); err != nil {
			return out, err
		}
		out.Cleared, out.Changed = true, true
		return out, nil
	}
	if doc.Overrides[server] == nil {
		doc.Overrides[server] = map[string]ToolOverride{}
	}
	before := doc.Overrides[server][tool]
	ov := before
	if edit.Name != nil {
		ov.Name = *edit.Name
	}
	if edit.Description != nil {
		ov.Description = *edit.Description
	}
	doc.Overrides[server][tool] = ov
	if err := SaveToolOverrides(stateDir, doc); err != nil {
		return out, err
	}
	out.Override, out.Changed = ov, ov != before
	return out, nil
}
