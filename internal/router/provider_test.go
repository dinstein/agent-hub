package router_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/router"
)

// fakeProvider is a host-served tool source — the shape internal/skills has.
type fakeProvider struct {
	id    string
	tools []mcp.ToolDef
	calls []string
}

func (p *fakeProvider) ID() string { return p.id }

func (p *fakeProvider) Tools() []mcp.ToolDef { return p.tools }

func (p *fakeProvider) Call(_ context.Context, raw string, _ json.RawMessage) (*mcp.CallResult, error) {
	p.calls = append(p.calls, raw)
	return &mcp.CallResult{Content: json.RawMessage(`[{"type":"text","text":"provided"}]`)}, nil
}

func provider(id string, names ...string) *fakeProvider {
	p := &fakeProvider{id: id}
	for _, n := range names {
		p.tools = append(p.tools, mcp.ToolDef{Name: n, InputSchema: json.RawMessage(`{"type":"object"}`)})
	}
	return p
}

// TestProviderAggregatesLikeAServer: a provider is namespaced, ordered and
// routable exactly like a downstream. That identity is what puts it under
// the same scope projection and the same gate chain (docs/subsystems/skills.md).
func TestProviderAggregatesLikeAServer(t *testing.T) {
	t.Parallel()
	srv := startServer(t, "github", markedTool("create_issue", "gh"))
	sk := provider("skills", "skill_pdf", "skill_git")

	rt, err := router.BuildWith([]*downstream.Server{srv}, []router.Provider{sk})
	if err != nil {
		t.Fatalf("BuildWith: %v", err)
	}
	want := []string{"github__create_issue", "skills__skill_git", "skills__skill_pdf"}
	if got := exposedNames(rt); !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	route, ok := rt.RouteOf("skills__skill_pdf")
	if !ok || route.ServerID != "skills" || route.RawTool != "skill_pdf" {
		t.Fatalf("RouteOf = %+v ok %v", route, ok)
	}

	// LookupProvider is the provider-only reverse lookup: a downstream tool
	// must never resolve through it (or the host would answer for a server).
	got, proute, ok := rt.LookupProvider("skills__skill_pdf")
	if !ok || got != router.Provider(sk) || proute != route {
		t.Fatalf("LookupProvider = %v %+v %v", got, proute, ok)
	}
	if _, _, ok := rt.LookupProvider("github__create_issue"); ok {
		t.Fatal("a downstream tool resolved as host-served")
	}
	// And the opposite direction: a provider entry has no live server, so
	// the downstream Lookup cannot call it by accident.
	if s, _, ok := rt.Lookup("skills__skill_pdf"); !ok || s != nil {
		t.Fatalf("Lookup on a provider entry = %v %v, want (nil, true)", s, ok)
	}

	res, err := sk.Call(context.Background(), route.RawTool, nil)
	if err != nil || res == nil {
		t.Fatalf("provider call: %v", err)
	}
	if !reflect.DeepEqual(sk.calls, []string{"skill_pdf"}) {
		t.Fatalf("provider saw calls %v", sk.calls)
	}

	// Definitions are readable by exposed name, with the name rewritten.
	def, ok := rt.Def("skills__skill_git")
	if !ok || def.Name != "skills__skill_git" {
		t.Fatalf("Def = %+v ok %v", def, ok)
	}
}

// TestProviderIsCallableFromAColdCatalog: the cache-built router (every
// downstream still connecting) already carries the provider, because a
// provider has nothing to connect to.
func TestProviderIsCallableFromAColdCatalog(t *testing.T) {
	t.Parallel()
	sk := provider("skills", "skill_pdf")
	rt, err := router.BuildFromCacheWith(map[string][]mcp.ToolDef{
		"github": {{Name: "create_issue", InputSchema: json.RawMessage(`{}`)}},
	}, []router.Provider{sk})
	if err != nil {
		t.Fatalf("BuildFromCacheWith: %v", err)
	}
	if _, _, ok := rt.LookupProvider("skills__skill_pdf"); !ok {
		t.Fatal("the provider is missing from a cache-built catalog")
	}
	// The cached downstream stays listable-but-not-callable, as before.
	if srv, _, ok := rt.Lookup("github__create_issue"); !ok || srv != nil {
		t.Fatalf("cached downstream = %v %v", srv, ok)
	}
}

// TestProviderIDCollisionIsAnError: a provider must never shadow (or be
// shadowed by) a configured server — the ambiguity is reported instead.
func TestProviderIDCollisionIsAnError(t *testing.T) {
	t.Parallel()
	srv := startServer(t, "skills", markedTool("x", "server"))
	if _, err := router.BuildWith([]*downstream.Server{srv}, []router.Provider{provider("skills", "y")}); err == nil {
		t.Fatal("a provider id colliding with a server id must be an error")
	}
}
