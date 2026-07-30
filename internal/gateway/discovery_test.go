package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/savings"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// metaNames is the frozen lazy-mode tools/list, in order. It is duplicated
// from internal/discovery on purpose: if the ABI order ever changes, this
// test must fail rather than follow along.
var metaNames = []string{"status", "search_tools", "describe_tool", "call_tool", "fetch_result"}

// setGovernance writes governance.json fields through an external registry
// writer (the operator/GUI path), then waits for the gateway to adopt them.
func setGovernance(t *testing.T, store *registry.Store, apply func(g *registry.GovernanceDoc)) {
	t.Helper()
	updateRegistry(t, store, func(tx *registry.Tx) {
		g := tx.Governance.V
		apply(&g)
		tx.Governance.V = g
	})
}

// callToolResult performs one tools/call and decodes the CallResult,
// retrying the transient busy error while downstreams connect.
func callToolResult(t *testing.T, c *testClient, name string, args any) mcp.CallResult {
	t.Helper()
	var out mcp.CallResult
	var lastErr *mcp.Error
	waitFor(t, fmt.Sprintf("tools/call %s (last error %v)", name, &lastErr), func() bool {
		resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{
			Name: name, Arguments: marshalParams(t, args),
		})
		lastErr = resp.Error
		if resp.Error != nil {
			if resp.Error.Code != codeRetryBusy {
				t.Fatalf("tools/call %s: %v", name, resp.Error)
			}
			return false
		}
		out = mcp.CallResult{}
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			t.Fatalf("decode %s result: %v", name, err)
		}
		return true
	})
	return out
}

// resultText flattens the text blocks of a CallResult.
func resultText(t *testing.T, res mcp.CallResult) string {
	t.Helper()
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(res.Content) == 0 {
		return ""
	}
	if err := json.Unmarshal(res.Content, &blocks); err != nil {
		t.Fatalf("decode content blocks: %v\n%s", err, res.Content)
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// TestLazyModeSurfaceAndDispatch is the end-to-end lazy-mode contract:
// switching the scope's discovery mode flips tools/list to the four
// meta-tools (announced by list_changed), search_tools finds the hidden
// tool, and call_tool executes it through the SAME pipeline as a direct
// call — proven by the gate counters, which must advance identically.
func TestLazyModeSurfaceAndDispatch(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	g, c, _ := startGateway(t, Config{
		ClientID: "lazy-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})

	// seedRegistry pins full, so the tool is listed verbatim. This test is
	// about the transition, which needs a starting mode that differs from
	// the one being switched to — inheriting the default (lazy) would make
	// the switch a no-op and the assertion vacuous.
	waitForTools(t, c, "fake__echo")

	// A direct call establishes the counter baseline.
	callToolOK(t, c, "fake__echo")
	before := g.pipe.Counters()

	// Scope change: governance switches the mode to lazy. The content hash
	// moves, so a list_changed must be pushed and the surface must flip.
	ext := externalRegistry(t, resolver)
	changed := c.notifCount(mcp.NotificationToolsListChanged)
	setGovernance(t, ext, func(gov *registry.GovernanceDoc) { gov.Discovery = "lazy" })
	waitFor(t, "list_changed after the discovery mode switch", func() bool {
		return c.notifCount(mcp.NotificationToolsListChanged) > changed
	})
	waitForTools(t, c, metaNames...)

	// search_tools ranks the hidden tool.
	res := callToolResult(t, c, discovery.MetaSearchTools, map[string]any{"query": "echo"})
	if res.IsError {
		t.Fatalf("search_tools reported an error: %s", resultText(t, res))
	}
	var payload struct {
		Results []struct {
			Tool     string `json:"tool"`
			Server   string `json:"server"`
			Rank     int    `json:"rank"`
			CallWith string `json:"call_with"`
		} `json:"results"`
		Matched int `json:"matched"`
	}
	if err := json.Unmarshal(res.StructuredContent, &payload); err != nil {
		t.Fatalf("decode search payload: %v\n%s", err, res.StructuredContent)
	}
	if len(payload.Results) != 1 || payload.Results[0].Tool != "fake__echo" {
		t.Fatalf("search results = %+v, want the single hit fake__echo", payload.Results)
	}
	if payload.Results[0].CallWith != discovery.MetaCallTool {
		t.Errorf("call_with = %q, want %q", payload.Results[0].CallWith, discovery.MetaCallTool)
	}

	// call_tool executes it — and lands in the same pipeline.
	res = callToolResult(t, c, discovery.MetaCallTool, map[string]any{
		"tool":      "fake__echo",
		"arguments": map[string]any{"marker": "lazy-call-tool"},
	})
	if res.IsError {
		t.Fatalf("call_tool reported an error: %s", resultText(t, res))
	}
	if text := resultText(t, res); !strings.Contains(text, "lazy-call-tool") {
		t.Errorf("call_tool result = %q, want the echoed marker", text)
	}

	after := g.pipe.Counters()
	if len(after) == 0 {
		t.Fatal("pipeline reports no counting stages")
	}
	for stage, n := range after {
		if n != before[stage]+1 {
			t.Errorf("stage %q counted %d, want %d: call_tool must traverse the SAME gate chain "+
				"as a direct tools/call (no fork)", stage, n, before[stage]+1)
		}
	}

	// status orients the agent over the same surface.
	status := callToolResult(t, c, discovery.MetaStatus, map[string]any{})
	if text := resultText(t, status); !strings.Contains(text, "mode=lazy") ||
		!strings.Contains(text, "fake: 1 tool(s)") {
		t.Errorf("status = %q, want the lazy surface summary", text)
	}
}

// TestLazyDropsUnknownBareNames pins the fail-closed naming rule: a bare
// name that is not one of the five meta-tools resolves to nothing and is
// DROPPED — never promoted to a meta-tool interpretation.
func TestLazyDropsUnknownBareNames(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	ext := externalRegistry(t, resolver)
	setGovernance(t, ext, func(gov *registry.GovernanceDoc) { gov.Discovery = "lazy" })

	_, c, _ := startGateway(t, Config{
		ClientID: "lazy-drop",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, metaNames...)
	// Wait for the live catalog: before it exists every name is legitimately
	// answered with the retryable busy error instead.
	callToolOK(t, c, "fake__echo")

	for _, name := range []string{"retrieve_tools", "inspect_tool", "echo"} {
		resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: name, Arguments: []byte(`{}`)})
		if resp.Error == nil {
			t.Fatalf("tools/call %q succeeded; a bare unknown name must be dropped", name)
		}
		if resp.Error.Code != mcp.CodeInvalidParams {
			t.Errorf("tools/call %q error code = %d, want %d", name, resp.Error.Code, mcp.CodeInvalidParams)
		}
	}
}

// TestGroupedModeListing covers the middle mode: one aggregate entry per
// visible server plus the shared call_tool, and the aggregate entry listing
// that server's tools.
func TestGroupedModeListing(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	ext := externalRegistry(t, resolver)
	setGovernance(t, ext, func(gov *registry.GovernanceDoc) { gov.Discovery = "grouped" })

	_, c, _ := startGateway(t, Config{
		ClientID: "grouped-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake_tools", discovery.MetaCallTool)

	res := callToolResult(t, c, "fake_tools", map[string]any{})
	if res.IsError {
		t.Fatalf("fake_tools reported an error: %s", resultText(t, res))
	}
	var payload struct {
		Server string `json:"server"`
		Tools  []struct {
			Tool    string `json:"tool"`
			RawName string `json:"raw_name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res.StructuredContent, &payload); err != nil {
		t.Fatalf("decode group payload: %v\n%s", err, res.StructuredContent)
	}
	if payload.Server != "fake" || len(payload.Tools) != 1 ||
		payload.Tools[0].Tool != "fake__echo" || payload.Tools[0].RawName != "echo" {
		t.Fatalf("group listing = %+v, want the single fake__echo entry", payload)
	}
}

// TestResultShapingAndFetchResult drives the shaping leg: a result larger
// than the session budget comes back truncated with a cursor trailer,
// fetch_result serves the remainder, an unknown cursor gets the frozen
// not-found reply, and the saving is recorded in savings.jsonl.
func TestResultShapingAndFetchResult(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	resolver := testResolver(dir)
	seedRegistry(t, resolver, "fake")
	ext := externalRegistry(t, resolver)
	setGovernance(t, ext, func(gov *registry.GovernanceDoc) {
		gov.Discovery = "lazy"
		gov.ResultBudget = map[string]registry.Doc[registry.Budget]{
			"*": {V: registry.Budget{Bytes: 512}},
		}
	})

	const payloadRunes = 4000
	big := strings.Repeat("a", payloadRunes)
	content, err := json.Marshal([]map[string]string{{"type": "text", "text": big}})
	if err != nil {
		t.Fatal(err)
	}
	script := &fakemcp.Script{Tools: []fakemcp.Tool{{
		Def: mcp.ToolDef{
			Name:        "big",
			Description: "returns a large text payload",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Result: &mcp.CallResult{Content: content},
	}}}

	_, c, _ := startGateway(t, Config{
		ClientID: "shape-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": script}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, metaNames...)

	res := callToolResult(t, c, discovery.MetaCallTool, map[string]any{
		"tool":      "fake__big",
		"arguments": map[string]any{},
	})
	page1 := resultText(t, res)
	if len(page1) >= len(big) {
		t.Fatalf("page 1 is %d bytes, want it bounded well below the %d-byte payload", len(page1), len(big))
	}
	cursor, offset := parseTrailer(t, page1)

	// Page 2 continues exactly where page 1 stopped and advances.
	res = callToolResult(t, c, discovery.MetaFetchResult, map[string]any{
		"cursor": cursor, "offset": offset,
	})
	if res.IsError {
		t.Fatalf("fetch_result reported an error: %s", resultText(t, res))
	}
	page2 := resultText(t, res)
	if page2 == "" || strings.HasPrefix(page2, "fetch_result: unknown") {
		t.Fatalf("page 2 = %q, want the continued payload", page2)
	}
	_, offset2 := parseTrailer(t, page2)
	if offset2 <= offset {
		t.Errorf("page 2 next offset = %d, want > %d (a page that cannot advance is a livelock)", offset2, offset)
	}

	// An unknown cursor gets the ONE frozen miss message — never a
	// distinguishable answer that could enumerate another session's cursors.
	res = callToolResult(t, c, discovery.MetaFetchResult, map[string]any{"cursor": "rc-999999"})
	if !res.IsError {
		t.Error("fetch_result of an unknown cursor must be an isError result")
	}
	const wantMiss = "fetch_result: unknown or expired cursor. " +
		"Re-run the original tool call to obtain a fresh result and cursor."
	if got := resultText(t, res); got != wantMiss {
		t.Errorf("miss message = %q, want the frozen %q", got, wantMiss)
	}

	// The saving is accounted for.
	savingsPath := filepath.Join(dir, "logs", savings.FileName)
	var rec savings.Record
	waitFor(t, "a savings.jsonl record", func() bool {
		data, err := os.ReadFile(savingsPath)
		if err != nil || len(data) == 0 {
			return false
		}
		line := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
		return json.Unmarshal([]byte(line), &rec) == nil && rec.Mode != ""
	})
	if rec.Mode != "shaping" || rec.Server != "fake" || rec.Client != "shape-client" {
		t.Errorf("savings record = %+v, want mode=shaping server=fake client=shape-client", rec)
	}
	if rec.SavedTokens <= 0 || rec.BaselineTokens <= rec.ActualTokens {
		t.Errorf("savings record = %+v, want a positive saving", rec)
	}
}

// parseTrailer extracts the cursor id and the next offset from a truncation
// trailer. The trailer wording is frozen by internal/shaping's golden test;
// this parser is the reader side of that contract.
func parseTrailer(t *testing.T, text string) (cursor string, offset int) {
	t.Helper()
	const marker = "Use fetch_result with cursor="
	i := strings.LastIndex(text, marker)
	if i < 0 {
		t.Fatalf("no truncation trailer in %q", tail(text))
	}
	if _, err := fmt.Sscanf(text[i:], marker+"%s offset=%d to continue.", &cursor, &offset); err != nil {
		t.Fatalf("unparsable trailer %q: %v", tail(text[i:]), err)
	}
	return cursor, offset
}

// tail keeps failure messages readable when the payload is large.
func tail(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

// TestLoadToolCacheReadsWhatTheGatewayWrote pins the offline reader the CLI
// uses against the writer: one format, one pair of functions.
func TestLoadToolCacheReadsWhatTheGatewayWrote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	resolver := testResolver(dir)

	// Empty cache is an empty map, never an error.
	got, err := LoadToolCache(resolver, nil)
	if err != nil {
		t.Fatalf("LoadToolCache on an empty data dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("LoadToolCache = %v, want empty", got)
	}

	cacheDir, err := resolver.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	cache := newToolCache(filepath.Join(cacheDir, toolCacheSubdir), slog.New(slog.DiscardHandler))
	want := []mcp.ToolDef{{Name: "echo", Description: "echoes", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	if err := cache.write("fake", want); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	got, err = LoadToolCache(resolver, nil)
	if err != nil {
		t.Fatalf("LoadToolCache: %v", err)
	}
	if len(got["fake"]) != 1 || got["fake"][0].Name != "echo" {
		t.Fatalf("LoadToolCache = %+v, want the written entry", got)
	}
}
