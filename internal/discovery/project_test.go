package discovery

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// The budget projection is the reason lazy mode saves tokens: every rank
// gets a compact signature, rank 1 additionally the full description, every
// other rank a bounded summary. No rank carries a schema — that is what
// describe_tool is for (docs/modules/dataplane.md).
func TestBudgetProjection(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("alpha beta gamma delta ", 40)) // ~920 bytes
	tools := []Tool{
		{Exposed: "s__one", ServerID: "s", RawTool: "widget_one", Def: mcp.ToolDef{
			Description: long,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		}},
		{Exposed: "s__two", ServerID: "s", RawTool: "widget_two", Def: mcp.ToolDef{
			Description: long,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"b":{"type":"string"}}}`),
		}},
		{Exposed: "s__three", ServerID: "s", RawTool: "widget_three", Def: mcp.ToolDef{
			Description: long,
		}},
	}
	s := New(Options{Mode: ModeLazy, Tools: tools})
	res, err := s.Search(SearchRequest{Query: "widget"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 3 {
		t.Fatalf("got %d hits", len(res.Hits))
	}

	top := res.Hits[0]
	if top.Summary != "" {
		t.Fatalf("rank 1 must carry a description, not a summary: %+v", top)
	}
	if top.Description != long {
		t.Fatal("rank 1 must carry the full description")
	}
	for _, h := range res.Hits {
		if h.Sig == "" {
			t.Fatalf("rank %d has no signature: %+v", h.Rank, h)
		}
		if !strings.HasPrefix(h.Sig, h.Tool+"(") {
			t.Fatalf("rank %d signature does not name its tool: %q", h.Rank, h.Sig)
		}
	}
	// The whole point of the two-step split: a search reply must be far
	// cheaper than the schemas it stands in for.
	if res.Savings.SavedTokens == 0 {
		t.Fatalf("the signature projection booked no savings: %+v", res.Savings)
	}
	if top.CallWith != MetaCallTool {
		t.Fatalf("call_with = %q, want %q (M1 pins the single call entry)", top.CallWith, MetaCallTool)
	}
	for _, h := range res.Hits[1:] {
		if h.Description != "" {
			t.Fatalf("rank %d leaked a full definition: %+v", h.Rank, h)
		}
		if h.Summary == "" {
			t.Fatalf("rank %d has no summary", h.Rank)
		}
		if len(h.Summary) > SummaryMaxBytes {
			t.Fatalf("rank %d summary is %d bytes, over the %d-byte budget",
				h.Rank, len(h.Summary), SummaryMaxBytes)
		}
		if h.CallWith != MetaCallTool {
			t.Fatalf("rank %d call_with = %q", h.Rank, h.CallWith)
		}
	}
	// A tool without an inputSchema still gets a callable one.
	if got := string(schemaOf(tools[2])); got != emptySchema {
		t.Fatalf("missing schema projected as %s", got)
	}
	// So does one whose schema is unparsable.
	broken := Tool{Def: mcp.ToolDef{InputSchema: json.RawMessage(`{"type":`)}}
	if got := string(schemaOf(broken)); got != emptySchema {
		t.Fatalf("invalid schema projected as %s", got)
	}
}

// The byte bound holds for multi-byte text, and truncation never splits a
// rune.
func TestSummaryByteBound(t *testing.T) {
	cases := []string{
		"",
		"short",
		strings.Repeat("a", SummaryMaxBytes-1),
		strings.Repeat("a", SummaryMaxBytes),
		strings.Repeat("a", SummaryMaxBytes+1),
		strings.Repeat("字", 200),             // 3 bytes per rune
		strings.Repeat("é", SummaryMaxBytes), // 2 bytes per rune
		strings.Repeat("🙂", SummaryMaxBytes), // 4 bytes per rune
		strings.Repeat("a字é🙂 ", 100),         // mixed widths
		"one\ttwo\n\nthree   four" + strings.Repeat(" five", 60),
	}
	for _, in := range cases {
		got := summarize(in)
		if len(got) > SummaryMaxBytes {
			t.Fatalf("summary of %d-byte input is %d bytes, over the budget", len(in), len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("summary of %d-byte input is not valid UTF-8: %q", len(in), got)
		}
		if strings.ContainsAny(got, "\n\t") {
			t.Fatalf("summary kept raw whitespace: %q", got)
		}
		if len(in) <= SummaryMaxBytes && !strings.ContainsAny(in, " \t\n") && got != in {
			t.Fatalf("a within-budget summary was rewritten: %q -> %q", in, got)
		}
	}
	// Truncation is marked.
	if got := summarize(strings.Repeat("a", 500)); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("truncation is unmarked: %q", got)
	}
	// Degenerate bound.
	if got := truncateBytes("abcdef", 2); got != ellipsis {
		t.Fatalf("truncateBytes with a tiny bound = %q", got)
	}
}

func TestWhitespaceCollapse(t *testing.T) {
	if got := collapseSpace("  a \n\t b   c  "); got != "a b c" {
		t.Fatalf("collapseSpace = %q", got)
	}
}

func TestMissingDescriptionPlaceholder(t *testing.T) {
	tools := []Tool{
		{Exposed: "s__a", ServerID: "s", RawTool: "widget", Def: mcp.ToolDef{Description: "   "}},
		{Exposed: "s__b", ServerID: "s", RawTool: "widget_two", Def: mcp.ToolDef{}},
	}
	s := New(Options{Mode: ModeLazy, Tools: tools})
	res, err := s.Search(SearchRequest{Query: "widget"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hits[0].Description != noDescription {
		t.Fatalf("rank 1 description = %q", res.Hits[0].Description)
	}
	if res.Hits[1].Summary != noDescription {
		t.Fatalf("rank 2 summary = %q", res.Hits[1].Summary)
	}
}

// The rendered search reply is frozen: agents are prompted against its
// exact shape.
func TestGoldenSearchReply(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	out, res := s.HandleSearch(json.RawMessage(`{"query":"read file","limit":3}`), nil)
	if out.IsError {
		t.Fatalf("search errored: %s", out.Content)
	}
	if res.Trace.QueryBytes != len("read file") {
		t.Fatalf("trace bytes = %d", res.Trace.QueryBytes)
	}
	assertGolden(t, "search_read_file.json", indentJSON(t, out.StructuredContent))
}

func TestGoldenStatusReply(t *testing.T) {
	for _, mode := range []Mode{ModeFull, ModeGrouped, ModeLazy} {
		s := New(Options{Mode: mode, Tools: corpus(),
			Pins: NewStaticPins(map[string][]string{"fs": {"read_file"}})})
		res := s.HandleStatus("")
		var blocks []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(res.Content, &blocks); err != nil {
			t.Fatal(err)
		}
		assertGolden(t, "status_"+string(mode)+".txt", []byte(blocks[0].Text))
	}
	// An empty surface says so instead of printing nothing.
	empty := New(Options{Mode: ModeLazy})
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(empty.HandleStatus("").Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blocks[0].Text, "no server is visible") {
		t.Fatalf("empty status = %q", blocks[0].Text)
	}
}

// A description containing quotes and newlines must not produce malformed
// JSON anywhere in the reply path.
func TestHostileDescriptionStaysWellFormed(t *testing.T) {
	nasty := "he said \"hi\"\n\t</script> \\ \x00 " + strings.Repeat("x", 300)
	s := New(Options{Mode: ModeGrouped, Tools: []Tool{
		{Exposed: "s__widget", ServerID: "s", RawTool: "widget", Def: mcp.ToolDef{Description: nasty}},
		{Exposed: "s__gadget", ServerID: "s", RawTool: "gadget", Def: mcp.ToolDef{Description: nasty}},
	}})
	name, _ := s.GroupName("s")
	if res := s.HandleGroup(name); !json.Valid(res.Content) || !json.Valid(res.StructuredContent) {
		t.Fatal("group listing produced malformed JSON")
	}
	lazy := New(Options{Mode: ModeLazy, Tools: s.Tools()})
	out, _ := lazy.HandleSearch(json.RawMessage(`{"query":"widget gadget"}`), nil)
	if !json.Valid(out.Content) || !json.Valid(out.StructuredContent) {
		t.Fatal("search reply produced malformed JSON")
	}
	// The diagnosis is caller-supplied text (downstream connect errors carry
	// remote server messages), so it goes through the same escaping as any
	// description.
	if res := lazy.HandleStatus(nasty); !json.Valid(res.Content) {
		t.Fatal("status reply produced malformed JSON")
	}
	// An unknown group entry is an anti-probing error, not a panic.
	if res := s.HandleGroup("nope_tools"); !res.IsError {
		t.Fatal("unknown group entry must report an error")
	}
}

func TestNoMatchReply(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	out, res := s.HandleSearch(json.RawMessage(`{"query":"quantum flux capacitor"}`), NewSearchGuard())
	if out.IsError {
		t.Fatal("a miss is not an error")
	}
	if len(res.Hits) != 0 || res.Matched != 0 {
		t.Fatalf("expected no hits: %+v", res)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(out.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blocks[0].Text, "no tool matches this query") {
		t.Fatalf("miss reply = %q", blocks[0].Text)
	}
}

func TestHandleSearchRejectsBadArguments(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus()})
	out, res := s.HandleSearch(json.RawMessage(`{"query":""}`), nil)
	if !out.IsError {
		t.Fatal("an empty query must be reported as an error result")
	}
	if res.Trace.Rejected != CodeQueryEmpty {
		t.Fatalf("trace.Rejected = %q, want %q", res.Trace.Rejected, CodeQueryEmpty)
	}
	out, res = s.HandleSearch(json.RawMessage(`{"qeury":"typo"}`), nil)
	if !out.IsError || res.Trace.Rejected != CodeInvalidArgs {
		t.Fatalf("unknown field not rejected: isError=%v trace=%+v", out.IsError, res.Trace)
	}
}
