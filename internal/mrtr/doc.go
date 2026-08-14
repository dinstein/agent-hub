// Package mrtr implements the Multi Round-Trip Request (MRTR) input
// resolution introduced in MCP 2026-07-28 (docs/status/mcp-2026-07-28.md §2
// Phase 2).
//
// When a downstream server returns an InputRequiredResult, Resolve answers
// each requested input through the Handler seam — internal/downstream fills
// it with the same peer-handler adapter that serves legacy server-initiated
// reverse RPCs — and returns the inputResponses map for the retry. The
// retry loop itself (re-issuing the original request with a new JSON-RPC
// id, the echoed requestState, and the collected responses) lives in
// internal/downstream: requestState deliberately never enters this package,
// so "the coordinator cannot inspect or modify it" is enforced by the
// package boundary.
package mrtr
