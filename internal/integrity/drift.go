package integrity

// DriftKind classifies one tool's relation to its pin after a catalog
// refresh (docs/flows.md).
type DriftKind string

const (
	// DriftNew: no pin existed. The tool is pinned as baseline and is NEVER
	// quarantined — inventory growth is not a rug-pull; call-time HITL
	// already covers first use (inherited invariant).
	DriftNew DriftKind = "new"
	// DriftUnchanged: fingerprint matches the pin.
	DriftUnchanged DriftKind = "unchanged"
	// DriftChanged: description and/or schema changed versus the pin.
	// Policy (quarantine_on_drift) decides whether the tool is quarantined;
	// the pin is NOT re-baselined until an explicit user release/approve.
	DriftChanged DriftKind = "changed"
	// DriftRemoved: a pinned tool is absent from the current catalog. The
	// pin is kept (merge never deletes) so a reappearance is checked against
	// the original baseline.
	DriftRemoved DriftKind = "removed"
)

// Drift is one tool's classification result.
type Drift struct {
	Server string // server ID
	Tool   string // raw downstream tool name
	Kind   DriftKind

	PinnedHash  string // "" for DriftNew
	CurrentHash string // "" for DriftRemoved

	// For DriftChanged: which identity-bearing fields moved. Both false for
	// every other kind. (A formula migration with identical content reports
	// DriftUnchanged, never a fake change.)
	DescChanged   bool
	SchemaChanged bool

	// Pinned is the baseline snapshot (diff review); zero-valued for DriftNew.
	Pinned ToolSnapshot
	// Current is the just-observed snapshot; zero-valued for DriftRemoved.
	Current ToolSnapshot
}
