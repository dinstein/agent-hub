// Package skills implements the skills subsystem: a two-layer model of a
// library of skill packages and their materialized installations into AI
// client directories (docs/subsystems/skills.md).
//
// # Two layers
//
// MCP servers are runtime intermediaries — agenthub sits on the call path
// and can change their visibility per session. Skills are the opposite:
// clients read them off the filesystem, agenthub is NOT on the read path.
// That difference forces two layers:
//
//   - Library (store): the canonical copy agenthub owns, content-addressed
//     under <skills>/store/<id>/<contentHash>/, indexed by skills.json.
//     This is the single source of truth.
//   - Install: a RECEIPT (installs.json) describing one materialization of
//     one skill into one client's directory at one scope. Receipts can go
//     out of date, so every one of them must be verifiable and repairable —
//     never trusted blind.
//
// # Honest tiering (docs/subsystems/skills.md)
//
// FILE MATERIALIZATION REACHES CLIENT GRANULARITY, NOT SESSION GRANULARITY.
// Once bytes are on disk every session of that client sees them; agenthub
// cannot retract a file for one session and keep it for another. Per-session
// skill visibility is only achievable over the skills-over-MCP path (5.3,
// M2), where agenthub is on the read path. Every value this package returns
// carries that limit explicitly in a Granularity field (always
// GranularityClient) so CLI and GUI output can state it instead of implying
// a precision that does not exist.
//
// # Write strategies
//
// Two, chosen per target (targets.go):
//
//   - StrategyOwnedDir: agenthub owns the whole directory and may rebuild
//     it from scratch. Ownership is proven by the marker file
//     .agenthub-managed.json. A directory WITHOUT our marker is somebody
//     else's — it is reported Conflict and never absorbed.
//   - StrategySentinelBlock: our content lives between BEGIN/END sentinels
//     inside a file somebody else owns. Bytes outside the sentinels are
//     preserved verbatim. If the sentinels are damaged (unpaired, inverted,
//     duplicated) the write is REFUSED with *SentinelError — a damaged
//     marker means we can no longer tell our bytes from the user's, and
//     guessing would destroy their content.
//
// # ApplyState
//
// Applied / Stale / Drifted / Missing / Conflict (model.go). This axis is
// ORTHOGONAL to internal/integrity's tool approval state machine (7.12 #19):
// ApplyState answers "are the bytes where we think they are", approval
// answers "is this content trusted". Neither is stored in the other's field
// and neither transition implies the other.
//
// # Storage
//
// Skills live in their own files under <data>/skills, NOT in the registry
// ("split files by change frequency"). A skill change must never
// trigger a router rebuild, and keeping the index out of the registry makes
// that structural instead of a comparison that can be got wrong. The
// on-disk discipline is the registry/integrity one — a sibling flock around
// every read-modify-write cycle, atomic writes (temp file, 0600, fsync,
// rename, fsync parent), a no-op guard, and read retries — reimplemented
// here so this package does not drag the registry document model into a
// subsystem that has no use for generations or watches.
//
// Fail directions, all fail-closed:
//   - A state file that exists but does not parse is *CorruptError, not an
//     empty store; every operation aborts and the file is left in place.
//   - A library copy whose recomputed fingerprint does not match its pin is
//     Tampered: install and update refuse to run for that skill.
//   - A missing "enabled" field reads as disabled, not enabled.
//
// # Git sources (ruling)
//
// This package NEVER shells out to git and never touches the network. A git
// source is imported from a local checkout the caller already has, and
// --pin <rev> is RECORDED (Source.GitRef / Source.PinnedCommit) so the
// revision that produced the library copy is reproducible. Fetch, clone and
// ref resolution are M2; until then Update on a git skill without a fresh
// checkout path returns ErrGitFetchUnsupported rather than pretending to be
// up to date.
package skills
