package e2e_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lazyMetaTools is the frozen lazy-mode tools/list, in order. It is written
// out here rather than imported: this suite drives the gateway from the
// OUTSIDE, and the meta-tool surface is exactly the kind of ABI an external
// client depends on.
var lazyMetaTools = []string{"status", "search_tools", "describe_tool", "call_tool", "fetch_result"}

// writeGovernance writes registry/governance.json directly — the operator
// path a GUI or a hand edit takes. The gateway reads it at startup and on
// every change notification; there is no CLI verb for scope yet.
func writeGovernance(t *testing.T, dataDir string, doc map[string]any) {
	t.Helper()
	dir := filepath.Join(dataDir, "registry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "governance.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLazyDiscoveryFullChain is the lazy-mode acceptance path, driven end to
// end through the real spawned gateway:
//
//	server add → scope set to lazy (registry write) → connect →
//	tools/list answers the five meta-tools (the downstream tool is NOT
//	listed) → search_tools finds it → call_tool runs it → an oversized
//	result comes back truncated with a cursor → fetch_result serves the
//	next page.
func TestLazyDiscoveryFullChain(t *testing.T) {
	dataDir := t.TempDir()
	out, _ := runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--json")
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("server add envelope: %s", out)
	}
	// Lazy mode plus a small result budget: both are scope inputs of the
	// global layer, so one write configures the whole run.
	writeGovernance(t, dataDir, map[string]any{
		"discovery":    "lazy",
		"resultBudget": map[string]any{"*": map[string]any{"bytes": 512}},
	})

	c := startGateway(t, dataDir, "e2e-lazy")
	c.initialize()

	// The exposed surface is the meta-tools alone — the downstream tool
	// exists and is callable, but it costs no prompt budget.
	names := c.listTools(30 * time.Second)
	if !equalStrings(names, lazyMetaTools) {
		c.fatalf("tools/list = %v, want the five meta-tools %v", names, lazyMetaTools)
	}

	// The downstream connects in the background; search is the readiness
	// probe an agent would use.
	c.waitForSearchHit("echo", "fake__echo", 30*time.Second)

	// The tool stays absent from tools/list after the catalog is live.
	if names := c.listTools(30 * time.Second); !equalStrings(names, lazyMetaTools) {
		c.fatalf("tools/list after connect = %v, want only the meta-tools", names)
	}

	// call_tool executes the searched tool.
	res := c.callTool("call_tool", map[string]any{
		"tool":      "fake__echo",
		"arguments": map[string]any{"marker": "e2e-lazy-call-tool"},
	}, 30*time.Second)
	if text := c.textContent(res); !strings.Contains(text, "e2e-lazy-call-tool") {
		c.fatalf("call_tool result does not contain the marker: %q", text)
	}

	// A result past the budget comes back as page 1 plus a cursor trailer.
	big := strings.Repeat("z", 4000)
	res = c.callTool("call_tool", map[string]any{
		"tool":      "fake__echo",
		"arguments": map[string]any{"payload": big},
	}, 30*time.Second)
	page1 := c.textContent(res)
	if len(page1) >= len(big) {
		c.fatalf("page 1 is %d bytes; the %d-byte payload was not truncated", len(page1), len(big))
	}
	cursor, offset := c.parseTrailer(page1)

	// fetch_result serves the remainder, and it advances.
	res = c.callTool("fetch_result", map[string]any{"cursor": cursor, "offset": offset}, 30*time.Second)
	page2 := c.textContent(res)
	if page2 == "" {
		c.fatalf("fetch_result returned an empty page")
	}
	if !strings.Contains(page2, "z") {
		c.fatalf("page 2 does not continue the payload: %q", page2)
	}
	if _, next := c.parseTrailer(page2); next <= offset {
		c.fatalf("page 2 next offset = %d, want > %d", next, offset)
	}

	// The savings estimate was accounted for.
	assertSavingsRecorded(t, dataDir)

	c.close()
}

// waitForSearchHit polls search_tools until the exposed name appears in the
// results. It is the lazy-mode analogue of waitForTool: in lazy mode a tool
// never shows up in tools/list, so readiness is observed through search.
func (c *gatewayClient) waitForSearchHit(query, want string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var last []string
	for time.Now().Before(deadline) {
		res := c.callTool("search_tools", map[string]any{"query": query}, 30*time.Second)
		last = nil
		for _, hit := range c.searchHits(res) {
			last = append(last, hit)
			if hit == want {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.fatalf("search_tools(%q) never returned %q within %s; last hits = %v", query, want, timeout, last)
}

// searchHits decodes the exposed tool names out of a search_tools result.
// A "no match" reply carries no structured payload and yields no hits.
func (c *gatewayClient) searchHits(res json.RawMessage) []string {
	c.t.Helper()
	var out struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		c.fatalf("search_tools result: %v\n%s", err, res)
	}
	if out.IsError {
		c.fatalf("search_tools reported an error: %s", res)
	}
	if len(out.StructuredContent) == 0 {
		return nil
	}
	var payload struct {
		Results []struct {
			Tool string `json:"tool"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out.StructuredContent, &payload); err != nil {
		c.fatalf("search payload: %v\n%s", err, out.StructuredContent)
	}
	names := make([]string, 0, len(payload.Results))
	for _, r := range payload.Results {
		names = append(names, r.Tool)
	}
	return names
}

// parseTrailer extracts the cursor id and next offset from a truncation
// trailer. The wording is frozen by internal/shaping; this is the reader
// side of that contract, written the way an agent would read it.
func (c *gatewayClient) parseTrailer(text string) (cursor string, offset int) {
	c.t.Helper()
	const marker = "Use fetch_result with cursor="
	i := strings.LastIndex(text, marker)
	if i < 0 {
		c.fatalf("no truncation trailer in the result (%d bytes)", len(text))
	}
	if _, err := fmt.Sscanf(text[i:], marker+"%s offset=%d to continue.", &cursor, &offset); err != nil {
		c.fatalf("unparsable trailer %q: %v", text[i:], err)
	}
	return cursor, offset
}

// assertSavingsRecorded checks that the shaped call produced a savings.jsonl
// line. The writer is asynchronous, so the file is polled.
func assertSavingsRecorded(t *testing.T, dataDir string) {
	t.Helper()
	path := filepath.Join(dataDir, "logs", "savings.jsonl")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			var rec struct {
				Mode        string `json:"mode"`
				Server      string `json:"server"`
				SavedTokens int64  `json:"savedTokens"`
			}
			line := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("savings line is not JSON: %v\n%s", err, line)
			}
			if rec.Mode != "shaping" || rec.Server != "fake" || rec.SavedTokens <= 0 {
				t.Fatalf("savings record = %+v, want a positive shaping saving for server fake", rec)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no savings record at %s", path)
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
