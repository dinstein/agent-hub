package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// searchToolDef is a definition with a schema worth printing: two required
// parameters, one optional with a default, one nested object.
func searchToolDef() mcp.ToolDef {
	return mcp.ToolDef{
		Name:        "search",
		Description: "Search the knowledge base.\nSecond line, folded by the human view.",
		InputSchema: json.RawMessage(`{"type":"object",` +
			`"properties":{` +
			`"query":{"type":"string"},` +
			`"index":{"type":"string"},` +
			`"limit":{"type":"integer","default":10},` +
			`"filter":{"type":"object","properties":{"after":{"type":"string"}}}},` +
			`"required":["query","index"]}`),
	}
}

// addFakeServer registers the fake endpoint and returns its id.
func addFakeServer(t *testing.T, tools []mcp.ToolDef) string {
	t.Helper()
	setDataDir(t)
	srv := mcpHTTPTestServerWithTools(t, tools)
	if code, out, _ := runCLI(t, "", "server", "add", "kb",
		"--url", srv.URL+"/mcp", "--transport", "http", "--local", "--json"); code != ExitOK {
		t.Fatalf("add exit = %d\n%s", code, out)
	}
	return "kb"
}

// lastEnvelope decodes the final line of a --json run: progress events are
// NDJSON lines that precede the envelope, and the envelope is always last.
func lastEnvelope(t *testing.T, out string) envelope {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return decodeEnvelope(t, lines[len(lines)-1])
}

// TestServerTestListsToolSignatures: the handshake already returned every
// definition in full, so `--tools` costs one extra flag and no extra round
// trip. Before this existed, the only rendering was the name list and an
// operator wiring up a new server had to guess parameter names by trial.
//
// The signature grammar is internal/discovery/toolsig's — the SAME string
// an agent sees from search_tools — so there is one format to learn, not
// two.
func TestServerTestListsToolSignatures(t *testing.T) {
	id := addFakeServer(t, []mcp.ToolDef{searchToolDef()})

	code, out, stderr := runCLI(t, "", "server", "test", id, "--tools", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s\n%s", code, out, stderr)
	}
	var res ServerTestResult
	if err := json.Unmarshal(lastEnvelope(t, out).Data, &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(res.ToolDetails) != 1 {
		t.Fatalf("toolDetails = %+v, want one entry", res.ToolDetails)
	}
	got := res.ToolDetails[0]
	if got.Name != "search" {
		t.Errorf("name = %q", got.Name)
	}
	// Required first in schema order, then optional byte-ascending; "?"
	// marks optional, "~" marks a lossy rendering.
	want := "search(query:str, index:str, filter?~:obj{after}, limit?:int=10) -> str"
	if got.Signature != want {
		t.Errorf("signature = %q, want %q", got.Signature, want)
	}
	if !got.Lossy {
		t.Error("lossy = false: the folded nested object must be admitted, " +
			"otherwise the signature reads as the whole truth")
	}
	if !strings.HasPrefix(got.Description, "Search the knowledge base.") {
		t.Errorf("description = %q", got.Description)
	}
	// The bare name list stays: a script written against the older shape
	// must keep working.
	if len(res.Tools) != 1 || res.Tools[0] != "search" || res.ToolCount != 1 {
		t.Errorf("tools = %v (count %d)", res.Tools, res.ToolCount)
	}
}

// TestServerTestPrintsToolSchema: --schema hands back the downstream's
// bytes verbatim. `server inspect --schema` cannot answer this question for
// a server that has never run through the gateway, because the persisted
// tool cache it reads is only written by a real gateway session.
func TestServerTestPrintsToolSchema(t *testing.T) {
	def := searchToolDef()
	id := addFakeServer(t, []mcp.ToolDef{def})

	code, out, stderr := runCLI(t, "", "server", "test", id, "--schema", "search", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s\n%s", code, out, stderr)
	}
	var res ServerTestResult
	if err := json.Unmarshal(lastEnvelope(t, out).Data, &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	var want, have any
	if err := json.Unmarshal(def.InputSchema, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(res.Schema, &have); err != nil {
		t.Fatalf("schema is not JSON: %v (%s)", err, res.Schema)
	}
	if a, b := mustMarshal(t, want), mustMarshal(t, have); a != b {
		t.Errorf("schema = %s, want %s", b, a)
	}
	// --schema alone does not turn on the listing.
	if len(res.ToolDetails) != 0 {
		t.Errorf("toolDetails = %+v, want none", res.ToolDetails)
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestServerTestSchemaUnknownTool: naming a tool the live server does not
// have is exit 3 with the tool-not-found code, not an empty schema. An
// empty answer would read as "this tool takes no arguments".
func TestServerTestSchemaUnknownTool(t *testing.T) {
	id := addFakeServer(t, []mcp.ToolDef{searchToolDef()})

	code, out, _ := runCLI(t, "", "server", "test", id, "--schema", "ghost", "--json")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitNotFound, out)
	}
	env := lastEnvelope(t, out)
	if env.Error == nil || env.Error.Code != CodeToolNotFound {
		t.Fatalf("envelope = %s", out)
	}
}

// TestServerTestHumanRendersSignaturesAndSchema pins the human half: the
// two output modes render from the same value, so a field visible in
// --json must be visible without it (docs/subsystems/cli.md rule 2).
func TestServerTestHumanRendersSignaturesAndSchema(t *testing.T) {
	id := addFakeServer(t, []mcp.ToolDef{searchToolDef()})

	code, out, stderr := runCLI(t, "", "server", "test", id, "--tools", "--schema", "search")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s\n%s", code, out, stderr)
	}
	if !strings.Contains(out, "search(query:str, index:str") {
		t.Errorf("human output has no signature:\n%s", out)
	}
	if !strings.Contains(out, "Search the knowledge base.") {
		t.Errorf("human output has no description:\n%s", out)
	}
	// The schema is indented for reading and says which tool it belongs to;
	// the JSON envelope keeps the raw bytes.
	if !strings.Contains(out, "schema of search:") ||
		!strings.Contains(out, `"query"`) || !strings.Contains(out, "\n      \"type\": \"object\"") {
		t.Errorf("human output has no labelled, indented schema:\n%s", out)
	}
	// A multi-line description is folded onto one line: the tool list must
	// stay readable as a list.
	if !strings.Contains(out, "Search the knowledge base. Second line") {
		t.Errorf("description was not folded onto one line:\n%s", out)
	}
}

// TestTruncateCutsOnARuneBoundary: the byte limit must not split a rune.
//
// The daemon's copy in internal/ctlapi has the same test for the same
// reason — this command renders its own result because it dials the
// downstream directly — and the two are expected to answer identically.
func TestTruncateCutsOnARuneBoundary(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("short = %q", got)
	}
	// Three bytes per rune, so a limit of 10 lands one byte inside the
	// fourth. Cutting there would leave bytes that are not UTF-8, and both
	// --json and the terminal would render them as U+FFFD — damage that
	// reads as a character the tool emitted.
	got := truncate(strings.Repeat("中", 10), 10)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
	if got != "中中中"+"… (truncated)" {
		t.Errorf("kept the wrong prefix: %q", got)
	}
	// Input that was never valid UTF-8 has no boundary to find; the trailer
	// alone is correct, because showing any of it would invent bytes.
	if got := truncate(strings.Repeat("\x80", 10), 4); got != "… (truncated)" {
		t.Errorf("invalid input = %q", got)
	}
}
