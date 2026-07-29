// Package mcp is the sole MCP protocol facade of the repository
// (canonical.md §2 rule 2, ruling #32).
//
// It implements, on the standard library only:
//
//   - JSON-RPC 2.0 wire types (Request / Response / Notification / Error,
//     IDs as string or number with raw-text fidelity),
//   - newline-delimited JSON framing with a bounded read
//     (MaxFrameSize = 16 MiB; an oversized frame poisons the reader),
//   - the minimal MCP domain types needed by M0 (initialize, tools/list,
//     tools/call, ping, cancelled / list_changed notifications),
//   - protocol version negotiation (client declares ProtocolVersion and
//     accepts a downgrade to any entry of SupportedVersions).
//
// Invariants:
//
//   - No package outside internal/mcp (including its transport subpackage)
//     may touch the MCP protocol; no third-party MCP library may be
//     imported anywhere in the repository (enforced by depguard).
//   - Tool input schemas and call results are passed through verbatim as
//     json.RawMessage — this package never re-shapes downstream JSON.
//   - Malformed frames and oversized frames yield decidable, typed errors
//     (ErrMalformedFrame, ErrFrameTooLarge); they must never panic.
package mcp
