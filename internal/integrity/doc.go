// Package integrity implements tool fingerprint pinning, drift
// classification, quarantine and the tool-approval state machine
// (docs/modules/security.md).
//
// Inherited, accident-driven invariants (toolport integrity.rs — each one
// paid for by a real incident, do not "simplify" them away):
//
//   - Corrupt != Fresh: a state file that exists but cannot be parsed is
//     ErrStoreCorrupt and every operation fails loudly (fail-closed). It is
//     NEVER treated as an empty set and NEVER renamed aside (a ".corrupt"
//     rename would make the next read look like a legitimate fresh store —
//     silent re-baseline is exactly what a tampering attacker wants).
//     A missing file, by contrast, IS fresh: first run has no pins.
//   - Added tools are never quarantined: inventory growth is not a
//     rug-pull; call-time HITL/confirmation already covers new tools.
//   - Merge never deletes: a pin for a tool that disappeared from the
//     catalog is kept (Removed drift) so a later reappearance is checked
//     against the original baseline, not re-pinned blind.
//   - Quarantine keys the CLIENT-VISIBLE exposed name, computed after
//     per-scope overrides are applied (#423: override precedes policy), so
//     a rename can never move a tool out from under its quarantine entry.
//   - Changed → Approved requires an explicit user action; baseline trust
//     never clears a rug-pull mark.
//
// Orthogonality (docs/modules/security.md): quarantine and approval are
// independent axes — quarantine manages visibility under drift policy,
// approval manages call permission. Releasing quarantine does not approve;
// approving does not release. (For skills the same machine is orthogonal to
// ApplyState: trust vs materialization.)
//
// Dependency budget: standard library + internal/platform only. The
// cross-process file lock and atomic-write ladder are deliberate
// independent reimplementations of internal/registry's (same style, no
// import) — integrity must not pull registry's document model into the
// data plane.
//
// Concurrency model: N gateways + the daemon run integrity checks against
// the same state files. Every read-modify-write cycle holds the sibling
// flock; no single-writer assumption is made anywhere.
package integrity
