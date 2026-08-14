// Package mcpstub is a minimal in-process MCP 2026-07-28 server stub used by
// transport integration tests (docs/status/mcp-2026-07-28.md §4.1).
//
// It implements the stateless 2026-07-28 surface a conformant client
// exercises — server/discover, tools/list with resultType and freshness
// hints, tools/call, and one MRTR round (the confirm tool answers
// input_required asking for roots/list, and redeems each issued
// requestState exactly once) — and it is strict on purpose: missing _meta,
// a wrong declared version, an Mcp-Method / Mcp-Name header that disagrees
// with the body, or a requestState not echoed verbatim is rejected with the
// spec's error code, so a passing client test proves wire-level
// conformance.
//
// subscriptions/listen is deliberately not offered here: the stub answers
// it method-not-found like any unknown method, and clients only open the
// stream when configured to (HTTPConfig.NotificationStream). The stream
// mechanics are pinned at the transport level
// (TestStreamableHTTP2026SubscriptionsListen).
package mcpstub
