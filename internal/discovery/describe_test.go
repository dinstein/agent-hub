package discovery

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// describeCorpus is the search corpus plus one tool with an outputSchema and
// one whose schema is unparsable, so the reply covers both forwarding paths.
func describeCorpus() []Tool {
	tools := corpus()
	tools = append(tools,
		Tool{Exposed: "web__count", ServerID: "web", RawTool: "count", Def: mcp.ToolDef{
			Name:         "web__count",
			Description:  "Count matching documents.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"total":{"type":"integer"}}}`),
		}},
		Tool{Exposed: "web__broken", ServerID: "web", RawTool: "broken", Def: mcp.ToolDef{
			Name:         "web__broken",
			InputSchema:  json.RawMessage(`{"type":`),
			OutputSchema: json.RawMessage(`{"type":`),
		}},
	)
	return tools
}

func TestParseDescribe(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		code string
		want []string
	}{
		{name: "singular", raw: `{"tool":"fs__read_file"}`, want: []string{"fs__read_file"}},
		{name: "batch", raw: `{"tools":["a","b"]}`, want: []string{"a", "b"}},
		{name: "duplicates collapse", raw: `{"tools":["a","a","b"]}`, want: []string{"a", "b"}},
		{name: "blanks dropped", raw: `{"tools":["a"," ","b"]}`, want: []string{"a", "b"}},
		{name: "both forms", raw: `{"tool":"a","tools":["b"]}`, code: CodeInvalidArgs},
		{name: "neither form", raw: `{}`, code: CodeInvalidArgs},
		{name: "empty list", raw: `{"tools":[]}`, code: CodeInvalidArgs},
		{name: "blank name", raw: `{"tool":"   "}`, code: CodeInvalidArgs},
		{name: "unknown field", raw: `{"tool_ids":["a"]}`, code: CodeInvalidArgs},
		{name: "over the cap", raw: `{"tools":["a","b","c","d","e","f"]}`, code: CodeTooManyTools},
		{name: "exactly the cap", raw: `{"tools":["a","b","c","d","e"]}`, want: []string{"a", "b", "c", "d", "e"}},
	}
	for _, tc := range tests {
		args, err := ParseDescribe(json.RawMessage(tc.raw))
		if tc.code != "" {
			if err == nil || codeOf(err) != tc.code {
				t.Fatalf("%s: err = %v, want code %q", tc.name, err, tc.code)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := args.names()
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s: names = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The cap is an error, not a clamp: an agent must never believe it saw
// everything it asked for.
func TestDescribeCapIsNotAClamp(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: describeCorpus()})
	out, res := s.HandleDescribe(json.RawMessage(`{"tools":["a","b","c","d","e","f"]}`))
	if !out.IsError {
		t.Fatal("an over-cap call must be an error result")
	}
	if len(res.Entries) != 0 {
		t.Fatalf("an over-cap call described %d tools", len(res.Entries))
	}
}

// describe_tool can never reach a tool search cannot: the visible set is the
// same map. An invisible name and a nonexistent one are indistinguishable.
func TestDescribeVisibilityIsNotWiderThanSearch(t *testing.T) {
	all := describeCorpus()
	visible := all[:2] // pretend the scope dropped everything else
	s := New(Options{Mode: ModeLazy, Tools: visible})

	names := make([]string, 0, 3)
	names = append(names, visible[0].Exposed, all[len(all)-1].Exposed, "no__such_tool")
	raw, err := json.Marshal(DescribeArgs{Tools: names})
	if err != nil {
		t.Fatal(err)
	}
	_, res := s.HandleDescribe(raw)

	if len(res.Entries) != 1 || res.Entries[0].Tool != visible[0].Exposed {
		t.Fatalf("described %+v", res.Entries)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("errors = %+v", res.Errors)
	}
	// The hidden tool and the nonexistent one must be reported identically
	// apart from the echoed name — no oracle.
	a, b := res.Errors[0], res.Errors[1]
	if a.Error != DescribeErrNotFound || b.Error != DescribeErrNotFound {
		t.Fatalf("distinguishable error kinds: %+v %+v", a, b)
	}
	if a.Remediation != b.Remediation {
		t.Fatalf("distinguishable remediation: %q vs %q", a.Remediation, b.Remediation)
	}
	// Every describable name is in the surface's own visible set.
	for _, n := range s.DescribeNames() {
		if _, ok := s.Lookup(n); !ok {
			t.Fatalf("DescribeNames reported %q, which the surface cannot look up", n)
		}
	}
	if len(s.DescribeNames()) != len(visible) {
		t.Fatalf("DescribeNames = %v", s.DescribeNames())
	}
}

// A call in which nothing resolved is still a normal reply carrying the
// remediation — the call itself was well-formed.
func TestDescribeAllMissesIsNotAnErrorResult(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: describeCorpus()})
	out, res := s.HandleDescribe(json.RawMessage(`{"tools":["nope","also_nope"]}`))
	if out.IsError {
		t.Fatalf("all-miss reply is an error result: %s", out.Content)
	}
	if len(res.Errors) != 2 || len(res.Entries) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(string(out.Content), describeRemediation) {
		t.Fatalf("reply carries no remediation: %s", out.Content)
	}
}

// An unparsable input schema is replaced by the permissive default and an
// unparsable output schema is dropped: the agent must never be handed a
// schema it cannot reason about.
func TestDescribeRepairsBrokenSchemas(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: describeCorpus()})
	_, res := s.HandleDescribe(json.RawMessage(`{"tool":"web__broken"}`))
	if len(res.Entries) != 1 {
		t.Fatalf("entries = %+v", res.Entries)
	}
	e := res.Entries[0]
	if string(e.Schema) != emptySchema {
		t.Fatalf("schema = %s", e.Schema)
	}
	if e.OutputSchema != nil {
		t.Fatalf("output_schema = %s", e.OutputSchema)
	}
	if e.Description != noDescription {
		t.Fatalf("description = %q", e.Description)
	}
	if !e.Lossy {
		t.Fatal("an unparsable schema must be reported lossy")
	}
}

// Entry order is the CALLER's order, so an agent can zip the reply against
// its own request list.
func TestDescribePreservesCallerOrder(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: describeCorpus()})
	_, res := s.HandleDescribe(json.RawMessage(`{"tools":["web__count","fs__read_file","git__log"]}`))
	want := []string{"web__count", "fs__read_file", "git__log"}
	for i, e := range res.Entries {
		if e.Tool != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, e.Tool, want[i])
		}
	}
}

// The rendered describe_tool reply is frozen: agents are prompted against
// its exact shape (canonical.md §6).
func TestGoldenDescribeReply(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: describeCorpus()})
	out, _ := s.HandleDescribe(json.RawMessage(`{"tools":["fs__read_file","web__count","ghost"]}`))
	if out.IsError {
		t.Fatalf("describe errored: %s", out.Content)
	}
	assertGolden(t, "describe_reply.json", indentJSON(t, out.StructuredContent))
}

// describe_tool is exposed in lazy mode only, and is reserved everywhere.
func TestDescribeToolExposure(t *testing.T) {
	tools := describeCorpus()
	lazy := New(Options{Mode: ModeLazy, Tools: tools})
	if lazy.Classify(MetaDescribeTool) != KindMeta {
		t.Fatal("lazy mode must classify describe_tool as a meta-tool")
	}
	var found bool
	for _, d := range lazy.List() {
		if d.Name == MetaDescribeTool {
			found = true
		}
	}
	if !found {
		t.Fatal("lazy tools/list omits describe_tool")
	}
	for _, mode := range []Mode{ModeFull, ModeGrouped} {
		s := New(Options{Mode: mode, Tools: tools})
		if s.Classify(MetaDescribeTool) != KindUnknown {
			t.Fatalf("%s mode exposes describe_tool", mode)
		}
	}
	if !IsMetaName(MetaDescribeTool) {
		t.Fatal("describe_tool must be a reserved name in every mode")
	}
}
