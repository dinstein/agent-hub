package scope

import (
	"testing"

	"github.com/dinstein/agent-hub/internal/router"
)

func convergeCatalog() router.Catalog {
	return router.NewCatalog(map[string][]string{
		"alpha": {"one", "two"},
		"beta":  {"three"},
	})
}

// A finished scope cannot say which layer narrowed it. The chain is an
// intersection and no layer widens, so a client seeing nothing has exactly
// one layer to blame — and Converge is what names it.
func TestConvergeReportsTheShapeAfterEachLayer(t *testing.T) {
	layers := []ScopeLayer{
		{Kind: LayerGlobal, Origin: "governance.json"},
		{Kind: LayerProfile, Origin: "profile:team", Servers: []string{"alpha"}},
		{Kind: LayerSession, Origin: "session:1", Tools: map[string]*ToolSelector{
			"alpha": {Allow: []string{"one"}},
		}},
	}

	steps, err := Converge(layers, convergeCatalog())
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("got %d steps, want one per layer", len(steps))
	}

	// The global layer intervenes in nothing, so the catalog survives whole.
	if steps[0].Servers != 2 || steps[0].Tools != 3 {
		t.Errorf("after global: %d servers / %d tools, want 2/3", steps[0].Servers, steps[0].Tools)
	}
	// The profile drops beta — this is the layer a "why is beta gone" reads off.
	if steps[1].Servers != 1 || steps[1].Tools != 2 {
		t.Errorf("after profile: %d servers / %d tools, want 1/2", steps[1].Servers, steps[1].Tools)
	}
	// The session narrows alpha's tools without removing the server.
	if steps[2].Servers != 1 || steps[2].Tools != 1 {
		t.Errorf("after session: %d servers / %d tools, want 1/1", steps[2].Servers, steps[2].Tools)
	}
	for i, want := range []string{"governance.json", "profile:team", "session:1"} {
		if steps[i].Origin != want {
			t.Errorf("step %d origin = %q, want %q", i, steps[i].Origin, want)
		}
	}
}

// Converge must describe the SAME fold Resolve performs. If the two could
// disagree the trace would explain a resolution that never happened, which is
// worse than no trace: it is a wrong answer that looks authoritative.
func TestConvergeAgreesWithMergeOnTheFullChain(t *testing.T) {
	layers := []ScopeLayer{
		{Kind: LayerGlobal, Origin: "governance.json"},
		{Kind: LayerProfile, Origin: "profile:team", Servers: []string{"alpha"}},
	}
	cat := convergeCatalog()

	steps, err := Converge(layers, cat)
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	es, err := Merge(layers, cat)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	last := steps[len(steps)-1]
	tools := 0
	for _, v := range es.Servers {
		tools += len(v.Tools)
	}
	if last.Servers != len(es.Servers) || last.Tools != tools {
		t.Fatalf("final step %d/%d disagrees with Merge %d/%d",
			last.Servers, last.Tools, len(es.Servers), tools)
	}
}

// An empty chain is not a narrowing, so it produces no step. A step for the
// bare catalog would sit in front of every trace claiming a layer acted.
func TestConvergeOnNoLayersReportsNothing(t *testing.T) {
	steps, err := Converge(nil, convergeCatalog())
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("got %d steps for an empty chain, want 0", len(steps))
	}
}

// A block-all selector is `[]`, not nil, and the two are opposite answers.
// Converge must show the layer taking everything rather than the layer doing
// nothing — the distinction the whole selector convention rests on.
func TestConvergeShowsABlockAllLayerTakingEverything(t *testing.T) {
	steps, err := Converge([]ScopeLayer{
		{Kind: LayerGlobal, Origin: "governance.json"},
		{Kind: LayerProfile, Origin: "profile:locked", Servers: []string{}},
	}, convergeCatalog())
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if steps[1].Servers != 0 || steps[1].Tools != 0 {
		t.Fatalf("a block-all layer left %d servers / %d tools, want 0/0",
			steps[1].Servers, steps[1].Tools)
	}
}
