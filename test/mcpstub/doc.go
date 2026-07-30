// Package mcpstub is a minimal in-process MCP 2026-07-28 server stub used by
// transport integration tests (docs/mcp-2026-07-28.md §4.1).
//
// It implements the stateless 2026-07-28 surface a conformant client
// exercises — server/discover, tools/list with resultType and freshness
// hints, tools/call — and it is strict on purpose: missing _meta, a wrong
// declared version, or an Mcp-Method / Mcp-Name header that disagrees with
// the body is rejected with the spec's error code, so a passing client test
// proves wire-level conformance.
//
// Still to come with their phases: one round of InputRequiredResult
// (Phase 2 MRTR) and a subscriptions/listen SSE stream (Phase 3).
package mcpstub
