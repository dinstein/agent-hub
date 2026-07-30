// Package mcpstub is a minimal in-process MCP 2026-07-28 server stub used by
// transport integration tests (docs/mcp-2026-07-28.md §4.1).
//
// It implements exactly the new protocol surface needed by the test suite:
// server/discover, stateless tools/call with resultType, one round of
// InputRequiredResult (elicitation), and a subscriptions/listen SSE stream.
//
// Not yet implemented: this placeholder satisfies the doc-path existence
// check (test/buildrules.TestDocsCitePathsThatExist) while Phase 1 work is
// in progress.
package mcpstub
