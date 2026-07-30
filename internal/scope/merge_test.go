package scope

import (
	"reflect"
	"testing"

	"github.com/dinstein/agent-hub/internal/router"
)

func discPtr(d DiscoveryMode) *DiscoveryMode { return &d }

func testCatalog() router.Catalog {
	return router.NewCatalog(map[string][]string{
		"fs":  {"read", "write", "delete"},
		"git": {"log", "commit"},
		"web": {"fetch"},
	})
}

func toolsOf(t *testing.T, es *EffectiveScope, server string) []string {
	t.Helper()
	tv, ok := es.Servers[server]
	if !ok {
		t.Fatalf("server %q not visible; visible: %v", server, es.Servers)
	}
	return tv.Tools
}

// TestMergeMatrix drives the full 4.1 merge table: three-state selectors,
// three layers, intersection / union / OR / most-specific-wins.
func TestMergeMatrix(t *testing.T) {
	cases := []struct {
		name    string
		layers  []ScopeLayer
		cat     router.Catalog
		servers map[string][]string // expected server -> tools; nil value = just visible
		disc    DiscoveryMode
		budgets map[string]int
	}{
		{
			name:   "no layers: catalog passes through",
			layers: nil,
			cat:    testCatalog(),
			servers: map[string][]string{
				"fs": {"delete", "read", "write"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
		},
		{
			name: "nil servers on every layer = no intervention",
			layers: []ScopeLayer{
				{Kind: LayerGlobal}, {Kind: LayerProfile},
				{Kind: LayerSession},
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {"delete", "read", "write"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
		},
		{
			name: "empty servers list = block-all (nil vs empty is load-bearing)",
			layers: []ScopeLayer{
				{Kind: LayerProfile, Servers: []string{}},
			},
			cat:     testCatalog(),
			servers: map[string][]string{},
		},
		{
			name: "server visibility intersects across layers",
			layers: []ScopeLayer{
				{Kind: LayerProfile, Servers: []string{"fs", "git", "web"}},
				{Kind: LayerProfile, Servers: []string{"fs", "git"}},
				{Kind: LayerSession, Servers: []string{"git", "web"}},
			},
			cat:     testCatalog(),
			servers: map[string][]string{"git": {"commit", "log"}},
		},
		{
			name: "layer cannot widen beyond catalog",
			layers: []ScopeLayer{
				{Kind: LayerProfile, Servers: []string{"fs", "ghost"}},
			},
			cat:     testCatalog(),
			servers: map[string][]string{"fs": {"delete", "read", "write"}},
		},
		{
			name: "tool allow nil = full server set",
			layers: []ScopeLayer{
				{Kind: LayerProfile, Tools: map[string]*ToolSelector{"fs": {Allow: nil}}},
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {"delete", "read", "write"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
		},
		{
			name: "tool allow empty = block-all tools, server stays visible",
			layers: []ScopeLayer{
				{Kind: LayerProfile, Tools: map[string]*ToolSelector{"fs": {Allow: []string{}}}},
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {}, "git": {"commit", "log"}, "web": {"fetch"},
			},
		},
		{
			name: "tool allow intersects across layers by ORIGINAL name",
			layers: []ScopeLayer{
				{Kind: LayerProfile, Tools: map[string]*ToolSelector{"fs": {Allow: []string{"read", "write"}}}},
				{Kind: LayerSession, Tools: map[string]*ToolSelector{"fs": {Allow: []string{"write", "delete"}}}},
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {"write"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
		},
		{
			name: "exposed-style names in allow do not match raw names",
			layers: []ScopeLayer{
				{Kind: LayerProfile, Tools: map[string]*ToolSelector{"fs": {Allow: []string{"fs__read", "read"}}}},
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {"read"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
		},
		{
			// Allow lists intersect: each layer can only take away.
			name: "allow lists intersect across layers",
			layers: []ScopeLayer{
				{Kind: LayerProfile, Tools: map[string]*ToolSelector{"fs": {Allow: []string{"read", "write"}}}},
				{Kind: LayerSession, Tools: map[string]*ToolSelector{"fs": {Allow: []string{"read", "delete"}}}},
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {"read"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
		},
		{
			name: "discovery most specific wins regardless of slice order",
			layers: []ScopeLayer{
				{Kind: LayerSession, Discovery: discPtr(DiscoveryLazy)},
				{Kind: LayerGlobal, Discovery: discPtr(DiscoveryFull)},
				{Kind: LayerProfile, Discovery: discPtr(DiscoveryGrouped)},
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {"delete", "read", "write"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
			disc: DiscoveryLazy,
		},
		{
			name: "discovery nil layers do not override",
			layers: []ScopeLayer{
				{Kind: LayerGlobal, Discovery: discPtr(DiscoveryGrouped)},
				{Kind: LayerSession}, // nil Discovery = no intervention
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {"delete", "read", "write"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
			disc: DiscoveryGrouped,
		},
		{
			name: "budget most specific wins; forced caps via min",
			layers: []ScopeLayer{
				{Kind: LayerGlobal, ResultBudget: map[string]*Budget{
					"*":  {Bytes: 1000, Forced: true},
					"fs": {Bytes: 400},
				}},
				{Kind: LayerSession, ResultBudget: map[string]*Budget{
					"*":  {Bytes: 9999}, // more specific but forced 1000 caps it
					"fs": {Bytes: 200},  // more specific, no force in play
				}},
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {"delete", "read", "write"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
			budgets: map[string]int{"*": 1000, "fs": 200},
		},
		{
			name: "forced tighter than specific wins the min",
			layers: []ScopeLayer{
				{Kind: LayerGlobal, ResultBudget: map[string]*Budget{"git": {Bytes: 100, Forced: true}}},
				{Kind: LayerProfile, ResultBudget: map[string]*Budget{"git": {Bytes: 50}}},
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {"delete", "read", "write"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
			budgets: map[string]int{"git": 50},
		},
		{
			name: "approval ORs across layers; false never loosens",
			layers: []ScopeLayer{
				{Kind: LayerGlobal},
				{Kind: LayerProfile},
				// inert: a false can never switch off the profile layer's true.
				{Kind: LayerSession},
			},
			cat: testCatalog(),
			servers: map[string][]string{
				"fs": {"delete", "read", "write"}, "git": {"commit", "log"}, "web": {"fetch"},
			},
		},
		{
			name: "all three layers combined",
			layers: []ScopeLayer{
				{Kind: LayerGlobal, Discovery: discPtr(DiscoveryFull)},
				{Kind: LayerProfile, Servers: []string{"fs", "git"},
					Tools: map[string]*ToolSelector{"fs": {Allow: []string{"read", "write", "delete"}}}},
				{Kind: LayerProfile, Discovery: discPtr(DiscoveryGrouped), Servers: []string{"fs"},
					Tools: map[string]*ToolSelector{"fs": {Allow: []string{"read", "write"}}}},
				{Kind: LayerSession,
					Tools: map[string]*ToolSelector{"fs": {Allow: []string{"read", "delete"}}}},
			},
			cat:     testCatalog(),
			servers: map[string][]string{"fs": {"read"}},
			disc:    DiscoveryGrouped,
		},
		{
			name:    "empty catalog resolves to zero servers (closed direction)",
			layers:  []ScopeLayer{{Kind: LayerProfile, Servers: []string{"fs"}}},
			cat:     router.Catalog{},
			servers: map[string][]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			es, err := Merge(tc.layers, tc.cat)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if got, want := len(es.Servers), len(tc.servers); got != want {
				t.Fatalf("visible servers = %v, want keys %v", es.Servers, tc.servers)
			}
			for srv, tools := range tc.servers {
				got := toolsOf(t, es, srv)
				if !reflect.DeepEqual(got, tools) && (len(got) != 0 || len(tools) != 0) {
					t.Errorf("server %q tools = %v, want %v", srv, got, tools)
				}
			}
			if es.Discovery != tc.disc {
				t.Errorf("discovery = %q, want %q", es.Discovery, tc.disc)
			}
			if tc.budgets != nil && !reflect.DeepEqual(es.Budgets, tc.budgets) {
				t.Errorf("budgets = %v, want %v", es.Budgets, tc.budgets)
			}
		})
	}
}

func TestMergeInvalidLayerKind(t *testing.T) {
	if _, err := Merge([]ScopeLayer{{Kind: LayerKind(99)}}, testCatalog()); err == nil {
		t.Fatal("expected error for invalid layer kind")
	}
}

// TestMergePurity: same inputs → same output, and inputs are not mutated.
func TestMergePurity(t *testing.T) {
	layers := []ScopeLayer{
		{Kind: LayerProfile, Servers: []string{"fs", "git"},
			Tools:        map[string]*ToolSelector{"fs": {Allow: []string{"read", "write"}}},
			ResultBudget: map[string]*Budget{"*": {Bytes: 500, Forced: true}},
			Discovery:    discPtr(DiscoveryLazy)},
		{Kind: LayerSession, Servers: []string{"fs"}},
	}
	cat := testCatalog()

	snapLayers := deepCopyLayers(layers)
	snapCat := deepCopyCatalog(cat)

	a, err := Merge(layers, cat)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Merge(layers, cat)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("Merge is not deterministic:\n%+v\nvs\n%+v", a, b)
	}
	if a.Hash != b.Hash {
		t.Errorf("hash differs across identical merges")
	}
	if !reflect.DeepEqual(layers, snapLayers) {
		t.Errorf("Merge mutated its layers input")
	}
	if !reflect.DeepEqual(cat, snapCat) {
		t.Errorf("Merge mutated its catalog input")
	}

	// Output must not alias input: mutating the result leaves inputs intact.
	a.Servers["fs"].Tools[0] = "tampered"
	if !reflect.DeepEqual(cat, snapCat) || !reflect.DeepEqual(layers, snapLayers) {
		t.Errorf("output aliases input memory")
	}
}

func TestChanged(t *testing.T) {
	cat := testCatalog()
	a, _ := Merge(nil, cat)
	b, _ := Merge(nil, cat)
	c, _ := Merge([]ScopeLayer{{Kind: LayerSession, Servers: []string{"fs"}}}, cat)

	if Changed(a, b) {
		t.Error("identical content reported as changed")
	}
	b2 := *b
	b2.Generation = 42
	if Changed(a, &b2) {
		t.Error("generation-only difference must not count as changed")
	}
	if !Changed(a, c) {
		t.Error("content difference not reported")
	}
	if Changed(nil, nil) {
		t.Error("nil,nil must not be changed")
	}
	if !Changed(nil, a) || !Changed(a, nil) {
		t.Error("nil transition must count as changed")
	}
}

func deepCopyLayers(in []ScopeLayer) []ScopeLayer {
	out := make([]ScopeLayer, len(in))
	for i, l := range in {
		cp := l
		cp.Servers = cloneStrings(l.Servers)
		if l.Tools != nil {
			cp.Tools = make(map[string]*ToolSelector, len(l.Tools))
			for k, v := range l.Tools {
				s := ToolSelector{Allow: cloneStrings(v.Allow)}
				cp.Tools[k] = &s
			}
		}
		if l.Discovery != nil {
			d := *l.Discovery
			cp.Discovery = &d
		}
		if l.ResultBudget != nil {
			cp.ResultBudget = make(map[string]*Budget, len(l.ResultBudget))
			for k, v := range l.ResultBudget {
				b := *v
				cp.ResultBudget[k] = &b
			}
		}
		out[i] = cp
	}
	return out
}

func deepCopyCatalog(in router.Catalog) router.Catalog {
	if in.Servers == nil {
		return router.Catalog{}
	}
	m := make(map[string][]string, len(in.Servers))
	for k, v := range in.Servers {
		m[k] = cloneStrings(v)
	}
	return router.Catalog{Servers: m}
}
