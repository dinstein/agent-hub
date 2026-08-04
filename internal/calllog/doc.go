// Package calllog stores the local, durable tools/call access ledger.
//
// Metadata and payloads deliberately have different shapes. Metadata is a
// bounded JSONL event stream shared by every gateway process. Complete request
// parameters and configured result bytes live in encrypted, per-process pack
// files, so an arbitrarily large accepted MCP request never breaks the atomic
// one-write-per-metadata-line rule.
//
// Events are HMAC-authenticated and pack entries use XChaCha20-Poly1305 with
// call/kind binding. Retention, total bytes and filesystem free-space reserve
// are enforced under one cross-process inspect-prune-write lock. These checks
// fail closed: a configured bound is never silently weakened on a platform
// that cannot provide the required locking or disk-space observation.
//
// This package records observability; it decides no scope or permission, and
// no assembly on top of it does either. A write that fails costs the history
// a line and is reported; it never costs a call. The gateway used to refuse a
// tools/call it could not record, which put an availability failure — a full
// disk, an unreadable vault — in the path of every call, in exchange for a
// record that was already lost by the time the refusal happened.
package calllog
