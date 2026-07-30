// Package mrtr implements the Multi Round-Trip Request (MRTR) coordinator
// introduced in MCP 2026-07-28 (docs/mcp-2026-07-28.md §2 Phase 2).
//
// When a downstream server returns an InputRequiredResult on a tools/call,
// prompts/get, or resources/read request, this package collects the requested
// inputs and retries the original request with inputResponses and the echoed
// requestState — transparently from the pipeline's perspective.
//
// Not yet implemented: this placeholder satisfies the doc-path existence
// check (test/buildrules.TestDocsCitePathsThatExist) while Phase 1 work is
// in progress.
package mrtr
