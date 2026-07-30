package router_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// markedTool builds a fake tool whose call result carries a unique marker,
// so a routed call proves which (server, tool) actually answered.
func markedTool(name, marker string) fakemcp.Tool {
	return fakemcp.Tool{
		Def: mcp.ToolDef{
			Name:        name,
			Description: "marked " + name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Result: &mcp.CallResult{
			Content: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":%q}]`, marker)),
		},
	}
}

// startServer connects an in-process fake downstream server with the given
// ID and tools.
func startServer(t *testing.T, id string, tools ...fakemcp.Tool) *downstream.Server {
	t.Helper()
	script := &fakemcp.Script{Tools: tools}
	dial := func(_ context.Context, _ downstream.Spec) (transport.Transport, error) {
		return fakemcp.Connect(script)
	}
	s, err := downstream.Connect(context.Background(), downstream.Spec{ID: id}, downstream.Deps{Dial: dial})
	if err != nil {
		t.Fatalf("Connect %q: %v", id, err)
	}
	t.Cleanup(s.Close)
	return s
}

func exposedNames(r *router.Router) []string {
	defs := r.List()
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}

func callMarker(t *testing.T, r *router.Router, exposed string) string {
	t.Helper()
	srv, route, ok := r.Lookup(exposed)
	if !ok {
		t.Fatalf("Lookup(%q): not found", exposed)
	}
	res, err := srv.Call(context.Background(), route.RawTool, nil)
	if err != nil {
		t.Fatalf("call %q via %q: %v", route.RawTool, exposed, err)
	}
	var items []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(res.Content, &items); err != nil || len(items) == 0 {
		t.Fatalf("bad content %s (err %v)", res.Content, err)
	}
	return items[0].Text
}

func TestAggregateListAndRoutedCall(t *testing.T) {
	t.Parallel()
	alpha := startServer(t, "alpha", markedTool("echo", "alpha/echo"), markedTool("greet", "alpha/greet"))
	beta := startServer(t, "beta", markedTool("echo", "beta/echo"))

	r, err := router.Build([]*downstream.Server{alpha, beta})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []string{"alpha__echo", "alpha__greet", "beta__echo"}
	if got := exposedNames(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	// Descriptions and schemas pass through verbatim.
	if d := r.List()[0]; d.Description != "marked echo" || string(d.InputSchema) != `{"type":"object"}` {
		t.Fatalf("passthrough broken: %+v", d)
	}
	if got := callMarker(t, r, "beta__echo"); got != "beta/echo" {
		t.Fatalf("beta__echo answered by %q", got)
	}
	if got := callMarker(t, r, "alpha__greet"); got != "alpha/greet" {
		t.Fatalf("alpha__greet answered by %q", got)
	}
	if route, ok := r.RouteOf("alpha__echo"); !ok || route != (router.Route{ServerID: "alpha", RawTool: "echo"}) {
		t.Fatalf("RouteOf(alpha__echo) = %+v, %v", route, ok)
	}
	if _, ok := r.RouteOf("nope__nope"); ok {
		t.Fatal("RouteOf accepted an unknown exposed name")
	}
}

// TestRouteOfWithDoubleUnderscoreNames is the anti-"__"-splitting test
// frozen in docs/flows.md ("RouteOf is the only legitimate way back to
// (server, tool)"): serverID "my__srv" + tool "do__it" and serverID "my" +
// tool "srv__do__it" collapse to the SAME exposed base name. Splitting the
// exposed name on "__" cannot distinguish them; only the Build-time map
// can. Both must route to their true origin.
func TestRouteOfWithDoubleUnderscoreNames(t *testing.T) {
	t.Parallel()
	a := startServer(t, "my__srv", markedTool("do__it", "from my__srv"))
	b := startServer(t, "my", markedTool("srv__do__it", "from my"))

	r, err := router.Build([]*downstream.Server{a, b})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Collision group sorted by raw tool name: "do__it" < "srv__do__it".
	want := []string{"my__srv__do__it", "my__srv__do__it_2"}
	if got := exposedNames(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	if route, _ := r.RouteOf("my__srv__do__it"); route != (router.Route{ServerID: "my__srv", RawTool: "do__it"}) {
		t.Fatalf("RouteOf base = %+v", route)
	}
	if route, _ := r.RouteOf("my__srv__do__it_2"); route != (router.Route{ServerID: "my", RawTool: "srv__do__it"}) {
		t.Fatalf("RouteOf suffixed = %+v", route)
	}
	if got := callMarker(t, r, "my__srv__do__it"); got != "from my__srv" {
		t.Fatalf("base name answered by %q", got)
	}
	if got := callMarker(t, r, "my__srv__do__it_2"); got != "from my" {
		t.Fatalf("suffixed name answered by %q", got)
	}
}

// TestGoldenNamingDeterminism pins the sanitize + conflict-suffix output:
// same input always yields exactly this catalog (determinism is contract).
// Includes a suffix landmine: the group "s__a_b" spills onto "s__a_b_2",
// which is itself the base of raw tool "a_b_2".
func TestGoldenNamingDeterminism(t *testing.T) {
	t.Parallel()
	s := startServer(t, "s",
		markedTool("a.b", "s/a.b"),
		markedTool("a_b", "s/a_b"),
		markedTool("a_b_2", "s/a_b_2"),
		markedTool("write file", "s/write file"),
		markedTool("工具", "s/unicode"),
	)
	weird := startServer(t, "s2!", markedTool("t", "s2!/t"))

	wantNames := []string{
		"s2___t",     // sanitize("s2!") = "s2_"
		"s____",      // sanitize("工具") = "__" (one '_' per rune)
		"s__a_b",     // raw "a.b" (first in raw-name order)
		"s__a_b_2",   // raw "a_b" (conflict suffix _2)
		"s__a_b_2_2", // raw "a_b_2": its base was taken by the suffix above
		"s__write_file",
	}
	wantRoutes := map[string]router.Route{
		"s2___t":        {ServerID: "s2!", RawTool: "t"},
		"s____":         {ServerID: "s", RawTool: "工具"},
		"s__a_b":        {ServerID: "s", RawTool: "a.b"},
		"s__a_b_2":      {ServerID: "s", RawTool: "a_b"},
		"s__a_b_2_2":    {ServerID: "s", RawTool: "a_b_2"},
		"s__write_file": {ServerID: "s", RawTool: "write file"},
	}
	for i := 0; i < 5; i++ { // map iteration randomness must not leak out
		r, err := router.Build([]*downstream.Server{s, weird})
		if err != nil {
			t.Fatalf("Build #%d: %v", i, err)
		}
		if got := exposedNames(r); !reflect.DeepEqual(got, wantNames) {
			t.Fatalf("Build #%d List = %v, want %v", i, got, wantNames)
		}
		for exposed, want := range wantRoutes {
			if got, ok := r.RouteOf(exposed); !ok || got != want {
				t.Fatalf("Build #%d RouteOf(%q) = %+v, %v; want %+v", i, exposed, got, ok, want)
			}
		}
	}
}
func TestBuildRejectsDuplicateServerIDs(t *testing.T) {
	t.Parallel()
	a := startServer(t, "dup", markedTool("t", "a"))
	b := startServer(t, "dup", markedTool("t", "b"))
	if _, err := router.Build([]*downstream.Server{a, b}); err == nil {
		t.Fatal("Build accepted duplicate server IDs")
	}
	if _, err := router.Build([]*downstream.Server{a, nil}); err == nil {
		t.Fatal("Build accepted a nil server")
	}
}

// TestBuildFromCacheMatchesBuild pins that the cache-built catalog uses the
// exact same exposed-name and ordering rules as the live one (the gateway
// serves tools/list from either, and they must never drift), and that
// cache entries are listable and routable but not callable (nil server).
func TestBuildFromCacheMatchesBuild(t *testing.T) {
	t.Parallel()
	live := []*downstream.Server{
		startServer(t, "s", markedTool("a", "1"), markedTool("x!y", "2")),
		startServer(t, "s2!", markedTool("t", "3")),
	}
	fromLive, err := router.Build(live)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cached := map[string][]mcp.ToolDef{}
	for _, s := range live {
		cached[s.ID()] = s.Tools()
	}
	fromCache, err := router.BuildFromCache(cached)
	if err != nil {
		t.Fatalf("BuildFromCache: %v", err)
	}
	if got, want := exposedNames(fromCache), exposedNames(fromLive); !reflect.DeepEqual(got, want) {
		t.Fatalf("cache-built names = %v, live-built names = %v", got, want)
	}
	for _, name := range exposedNames(fromCache) {
		route, ok := fromCache.RouteOf(name)
		if !ok {
			t.Fatalf("RouteOf(%q) missing in cache-built router", name)
		}
		liveRoute, _ := fromLive.RouteOf(name)
		if route != liveRoute {
			t.Errorf("RouteOf(%q) = %+v (cache) vs %+v (live)", name, route, liveRoute)
		}
		srv, _, ok := fromCache.Lookup(name)
		if !ok || srv != nil {
			t.Errorf("Lookup(%q) = (%v, %v), want (nil server, true) for cache entries", name, srv, ok)
		}
	}
}
