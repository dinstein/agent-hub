// Package accesslog stores the local, durable tools/call access ledger.
//
// Metadata and payloads deliberately have different shapes. Metadata is a
// bounded JSONL event stream shared by every gateway process. Complete request
// parameters and configured result bytes live in encrypted, per-process pack
// files, so an arbitrarily large accepted MCP request never breaks the atomic
// one-write-per-metadata-line rule.
//
// This package records observability; it decides no scope or permission. A
// caller may nevertheless choose strict durability and refuse to execute when
// Begin cannot persist the request. That failure direction belongs to the
// assembly, not to the frozen pipeline gate chain.
package accesslog
