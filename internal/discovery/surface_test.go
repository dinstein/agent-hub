package discovery

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/scope"
)

// A client that configures nothing gets lazy. The garbage case matters as
// much as the unset one: a typo in a config file must land on the default
// rather than blanking the surface or erroring.
func TestModeOfDefaultsToLazy(t *testing.T) {
	cases := []struct {
		name string
		es   *scope.EffectiveScope
		want Mode
	}{
		{"nil scope", nil, ModeLazy},
		{"unset", &scope.EffectiveScope{}, ModeLazy},
		{"garbage", &scope.EffectiveScope{Discovery: scope.DiscoveryMode("swarm")}, ModeLazy},
		{"lazy", &scope.EffectiveScope{Discovery: scope.DiscoveryLazy}, ModeLazy},
		{"grouped", &scope.EffectiveScope{Discovery: scope.DiscoveryGrouped}, ModeGrouped},
		{"full", &scope.EffectiveScope{Discovery: scope.DiscoveryFull}, ModeFull},
	}
	for _, tc := range cases {
		if got := ModeOf(tc.es); got != tc.want {
			t.Errorf("%s: ModeOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The three exposure modes are frozen: their tools/list output is the
// contract an agent's prompt is written against.
func TestGoldenFullList(t *testing.T) {
	s := New(Options{Mode: ModeFull, Tools: corpus()})
	assertGolden(t, "full_list.json", marshalDefs(t, s.List()))
}

func TestGoldenGroupedList(t *testing.T) {
	s := New(Options{Mode: ModeGrouped, Tools: corpus()})
	assertGolden(t, "grouped_list.json", marshalDefs(t, s.List()))
}

func TestGoldenLazyList(t *testing.T) {
	s := New(Options{
		Mode:  ModeLazy,
		Tools: corpus(),
		Pins:  NewStaticPins(map[string][]string{"fs": {"read_file"}}),
	})
	assertGolden(t, "lazy_list.json", marshalDefs(t, s.List()))
}

func TestGoldenGroupListing(t *testing.T) {
	s := New(Options{Mode: ModeGrouped, Tools: corpus()})
	name, ok := s.GroupName("fs")
	if !ok {
		t.Fatal("fs has no group name")
	}
	res := s.HandleGroup(name)
	if res.IsError {
		t.Fatalf("group listing errored: %s", res.Content)
	}
	assertGolden(t, "group_fs.json", indentJSON(t, res.StructuredContent))
}

// Full mode is pass-through: this package must not rewrite a downstream
// definition on its way to the client.
func TestFullModeIsVerbatim(t *testing.T) {
	in := corpus()
	byName := make(map[string]mcp.ToolDef, len(in))
	for _, tool := range in {
		byName[tool.Exposed] = tool.Def
	}
	got := New(Options{Mode: ModeFull, Tools: in}).List()
	if len(got) != len(in) {
		t.Fatalf("got %d defs, want %d", len(got), len(in))
	}
	for i, d := range got {
		want, ok := byName[d.Name]
		if !ok {
			t.Fatalf("unexpected definition %q", d.Name)
		}
		if d.Description != want.Description || string(d.InputSchema) != string(want.InputSchema) {
			t.Fatalf("definition %q was rewritten: %+v", d.Name, d)
		}
		if i > 0 && got[i-1].Name >= d.Name {
			t.Fatalf("full list is not sorted by exposed name at %d", i)
		}
	}
}

// Grouped mode's promise: the tool COUNT collapses to servers+1 while every
// callable tool name still appears in the listing (no search required).
func TestGroupedCollapsesCountWithoutHidingNames(t *testing.T) {
	tools := corpus()
	s := New(Options{Mode: ModeGrouped, Tools: tools})
	defs := s.List()
	if want := len(s.Servers()) + 1; len(defs) != want {
		t.Fatalf("grouped list has %d entries, want %d", len(defs), want)
	}
	if defs[len(defs)-1].Name != MetaCallTool {
		t.Fatalf("last grouped entry is %q, want %q", defs[len(defs)-1].Name, MetaCallTool)
	}
	joined := ""
	for _, d := range defs {
		joined += d.Description
	}
	for _, tool := range tools {
		if !contains(joined, tool.RawTool) {
			t.Errorf("grouped descriptions never name %q", tool.RawTool)
		}
	}
}

// The grouped call entry must be byte-identical to the lazy one: one
// call_tool contract across modes.
func TestGroupedCallToolMatchesLazy(t *testing.T) {
	grouped := New(Options{Mode: ModeGrouped, Tools: corpus()}).List()
	lazy := New(Options{Mode: ModeLazy, Tools: corpus()}).List()
	var g, l mcp.ToolDef
	for _, d := range grouped {
		if d.Name == MetaCallTool {
			g = d
		}
	}
	for _, d := range lazy {
		if d.Name == MetaCallTool {
			l = d
		}
	}
	if g.Description != l.Description || string(g.InputSchema) != string(l.InputSchema) {
		t.Fatal("grouped and lazy call_tool definitions diverged")
	}
}

// Group names are a pure function of the visible server set, including when
// sanitisation collides.
func TestGroupNameCollisionIsDeterministic(t *testing.T) {
	tools := []Tool{
		{Exposed: "a_b__x", ServerID: "a.b", RawTool: "x"},
		{Exposed: "a_b__y", ServerID: "a-b", RawTool: "y"},
		{Exposed: "a_b__z", ServerID: "a/b", RawTool: "z"},
	}
	var first []string
	for i := 0; i < 5; i++ {
		shuffled := []Tool{tools[i%3], tools[(i+1)%3], tools[(i+2)%3]}
		s := New(Options{Mode: ModeGrouped, Tools: shuffled})
		var names []string
		for _, d := range s.List() {
			names = append(names, d.Name)
		}
		if first == nil {
			first = names
			continue
		}
		if len(first) != len(names) {
			t.Fatalf("group naming unstable: %v vs %v", first, names)
		}
		for j := range names {
			if names[j] != first[j] {
				t.Fatalf("group naming unstable: %v vs %v", first, names)
			}
		}
	}
	// "a-b" keeps its sanitised base name; "a.b" and "a/b" take suffixes,
	// assigned in sorted serverID order.
	want := []string{"a-b_tools", "a_b_tools", "a_b_tools_2", MetaCallTool}
	for i, w := range want {
		if first[i] != w {
			t.Fatalf("group names = %v, want %v", first, want)
		}
	}
}

// Fail-closed naming: an unknown name is dropped, never promoted to a
// meta-tool, and a cold catalog makes EVERY name unknown.
func TestClassifyFailsClosed(t *testing.T) {
	lazy := New(Options{Mode: ModeLazy, Tools: corpus(),
		Pins: NewStaticPins(map[string][]string{"fs": {"read_file"}})})
	grouped := New(Options{Mode: ModeGrouped, Tools: corpus()})
	full := New(Options{Mode: ModeFull, Tools: corpus()})
	cold := New(Options{Mode: ModeLazy}) // scope resolved before any tools/list answered

	cases := []struct {
		name string
		s    *Surface
		in   string
		want NameKind
	}{
		{"lazy meta status", lazy, MetaStatus, KindMeta},
		{"lazy meta search", lazy, MetaSearchTools, KindMeta},
		{"lazy meta call", lazy, MetaCallTool, KindMeta},
		{"lazy meta fetch", lazy, MetaFetchResult, KindMeta},
		{"lazy pinned tool", lazy, "fs__read_file", KindTool},
		{"lazy unpinned but routable", lazy, "git__log", KindTool},
		{"lazy unknown bare name", lazy, "retrieve_tools", KindUnknown},
		{"lazy unknown namespaced", lazy, "fs__nope", KindUnknown},
		{"grouped aggregate", grouped, "fs_tools", KindGroup},
		{"grouped call entry", grouped, MetaCallTool, KindMeta},
		{"grouped search is NOT exposed", grouped, MetaSearchTools, KindUnknown},
		{"grouped status is NOT exposed", grouped, MetaStatus, KindUnknown},
		{"full has no meta", full, MetaCallTool, KindUnknown},
		{"full routable", full, "web__fetch_url", KindTool},
		// The cold-cache rule of docs/flows.md: nothing resolves, and the
		// bare-looking names must NOT fall back to meta-tools.
		{"cold catalog: meta still meta", cold, MetaStatus, KindMeta},
		{"cold catalog: bare unknown dropped", cold, "read_file", KindUnknown},
		{"cold catalog: routed unknown dropped", cold, "fs__read_file", KindUnknown},
	}
	for _, tc := range cases {
		if got := tc.s.Classify(tc.in); got != tc.want {
			t.Errorf("%s: Classify(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
		if drop := tc.s.ShouldDrop(tc.in); drop != (tc.want == KindUnknown) {
			t.Errorf("%s: ShouldDrop(%q) = %v", tc.name, tc.in, drop)
		}
	}
}

func TestIsBareName(t *testing.T) {
	if !IsBareName("status") || !IsBareName("read_file") {
		t.Error("bare names misclassified")
	}
	if IsBareName("fs__read_file") {
		t.Error("namespaced name reported as bare")
	}
}

// A tool whose raw name equals a meta-tool name must not shadow the meta
// surface, and vice versa.
func TestMetaNamesAreReserved(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: corpus(),
		Pins: NewStaticPins(map[string][]string{"git": {"status"}})})
	seen := map[string]int{}
	for _, d := range s.List() {
		seen[d.Name]++
	}
	for n, c := range seen {
		if c != 1 {
			t.Fatalf("duplicate entry %q in lazy list (%d times)", n, c)
		}
	}
	if seen[MetaStatus] != 1 || seen["git__status"] != 1 {
		t.Fatalf("expected both the meta status tool and the pinned git__status: %v", seen)
	}
}

// Pinned tools are exposed directly in lazy mode; pinning cannot widen
// visibility beyond the scope-filtered set.
func TestPinned(t *testing.T) {
	tools := corpus()
	pins := NewStaticPins(map[string][]string{
		"fs":      {"read_file", "write_file"},
		"git":     {PinAll},
		"absent":  {"whatever"}, // server not visible: no effect
		"web":     {"nope"},     // tool not visible: no effect
		"":        {"ignored"},
		"ignored": {""},
	})
	s := New(Options{Mode: ModeLazy, Tools: tools, Pins: pins})

	var got []string
	for _, t := range s.Pinned() {
		got = append(got, t.Exposed)
	}
	want := []string{"fs__read_file", "fs__write_file", "git__commit", "git__log", "git__status"}
	if len(got) != len(want) {
		t.Fatalf("pinned = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pinned = %v, want %v", got, want)
		}
	}

	list := s.List()
	if len(list) != len(metaDefs)+len(want) {
		t.Fatalf("lazy list has %d entries, want %d", len(list), len(metaDefs)+len(want))
	}
	// Order: the four meta-tools first, then pinned tools sorted.
	for i, d := range metaDefs {
		if list[i].Name != d.Name {
			t.Fatalf("lazy list entry %d = %q, want %q", i, list[i].Name, d.Name)
		}
	}

	// Nil PinSet pins nothing.
	if n := len(New(Options{Mode: ModeLazy, Tools: tools}).Pinned()); n != 0 {
		t.Fatalf("nil PinSet pinned %d tools", n)
	}
	if (*StaticPins)(nil).IsPinned("fs", "read_file") {
		t.Fatal("nil StaticPins must pin nothing")
	}
	if got := NewStaticPins(map[string][]string{"a": {"x"}, "b": {PinAll}}).Servers(); len(got) != 2 {
		t.Fatalf("Servers = %v", got)
	}
}

// Visible is the single visibility projection: search and call see exactly
// what tools/list sees.
func TestVisibleUsesTheSameScopePredicate(t *testing.T) {
	cached := map[string][]mcp.ToolDef{
		"fs":  {{Name: "read_file"}, {Name: "write_file"}},
		"git": {{Name: "log"}},
	}
	rt, err := router.BuildFromCache(cached)
	if err != nil {
		t.Fatal(err)
	}
	es := &scope.EffectiveScope{Servers: map[string]scope.ToolView{
		"fs": {Tools: sorted("read_file")}, // write_file hidden, git invisible
	}}
	got := New(Options{Mode: ModeLazy, Tools: Visible(rt, es)})
	if len(got.Tools()) != 1 || got.Tools()[0].Exposed != "fs__read_file" {
		t.Fatalf("visible = %+v", got.Tools())
	}
	// A hidden tool can be neither searched nor called.
	res, err := got.Search(SearchRequest{Query: "write file"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Hits {
		if h.Tool == "fs__write_file" {
			t.Fatal("search surfaced a tool the scope hides")
		}
	}
	if _, _, err := got.ResolveCall(json.RawMessage(`{"tool":"fs__write_file"}`)); err == nil {
		t.Fatal("call resolved a tool the scope hides")
	}

	// nil scope = no scope authority: everything passes (the registry-less
	// gateway mode), decided by the caller before calling.
	if n := len(Visible(rt, nil)); n != 3 {
		t.Fatalf("unscoped Visible = %d tools, want 3", n)
	}
	if Visible(nil, es) != nil {
		t.Fatal("Visible(nil router) must be empty")
	}
}

func TestCacheKeyTracksScopeHash(t *testing.T) {
	a := &scope.EffectiveScope{Hash: [32]byte{1}}
	b := &scope.EffectiveScope{Hash: [32]byte{2}}
	if CacheKeyOf(7, a) == CacheKeyOf(7, b) {
		t.Fatal("different scopes must not share an index cache slot")
	}
	if CacheKeyOf(7, a) != CacheKeyOf(7, &scope.EffectiveScope{Hash: [32]byte{1}}) {
		t.Fatal("equal content must share an index cache slot")
	}
	if CacheKeyOf(7, a) == CacheKeyOf(8, a) {
		t.Fatal("catalog generation must take part in the key")
	}
	s := New(Options{Mode: ModeLazy, Generation: 7, Scope: a})
	if s.CacheKey() != CacheKeyOf(7, a) {
		t.Fatal("surface key mismatch")
	}
}

func TestNameKindString(t *testing.T) {
	want := map[NameKind]string{KindUnknown: "unknown", KindMeta: "meta", KindGroup: "group", KindTool: "tool"}
	for k, w := range want {
		if k.String() != w {
			t.Errorf("NameKind(%d).String() = %q, want %q", k, k.String(), w)
		}
	}
}

// helpers

func sorted(in ...string) []string {
	out := append([]string(nil), in...)
	slices.Sort(out)
	return out
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// indentJSON pretty-prints raw WITHOUT reordering it: the golden then pins
// the emitted field order too.
func indentJSON(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}
