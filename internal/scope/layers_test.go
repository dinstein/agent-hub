package scope

import (
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/router"
)

func doc[T any](v T) registry.Doc[T] { return registry.Doc[T]{V: v} }

func docPtr[T any](v T) *registry.Doc[T] { d := doc(v); return &d }

func emptySnap() *registry.Snapshot {
	return &registry.Snapshot{
		Generation: 1,
		Servers:    doc(registry.ServersDoc{Servers: map[string]registry.Doc[registry.ServerEntry]{}}),
		Profiles:   doc(registry.ProfilesDoc{Profiles: map[string]registry.Doc[registry.Profile]{}}),
		Clients:    doc(registry.ClientsDoc{Clients: map[string]registry.Doc[registry.ClientEntry]{}}),
		Governance: doc(registry.GovernanceDoc{}),
	}
}

func layerOfKind(t *testing.T, layers []ScopeLayer, k LayerKind) *ScopeLayer {
	t.Helper()
	for i := range layers {
		if layers[i].Kind == k {
			return &layers[i]
		}
	}
	return nil
}

func TestFromRegistryEmptySnapshot(t *testing.T) {
	layers, diags := FromRegistry(emptySnap(), SessionKey{ClientID: "claude-code"})
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %v", diags)
	}
	if len(layers) != 1 || layers[0].Kind != LayerGlobal {
		t.Fatalf("want single global layer, got %+v", layers)
	}
	if layers[0].Servers != nil {
		t.Error("global layer must not intervene on servers")
	}
}

func TestFromRegistryGovernance(t *testing.T) {
	snap := emptySnap()
	snap.Governance = doc(registry.GovernanceDoc{
		Discovery:    "grouped",
		ResultBudget: map[string]registry.Doc[registry.Budget]{"*": doc(registry.Budget{Bytes: 1024, Forced: true})},
	})
	layers, _ := FromRegistry(snap, SessionKey{ClientID: "x"})
	gl := layerOfKind(t, layers, LayerGlobal)
	if gl.Discovery == nil || *gl.Discovery != DiscoveryGrouped {
		t.Errorf("discovery not mapped: %v", gl.Discovery)
	}
	if b := gl.ResultBudget["*"]; b == nil || b.Bytes != 1024 || !b.Forced {
		t.Errorf("budget not mapped: %+v", b)
	}
}

// A bound client contributes NO layer of its own: clients.json says which
// profile applies, and the profile says everything about what it contains —
// including how it is surfaced. Two places to look was the defect.
func TestFromRegistryClientSelectsProfileAndAddsNoLayer(t *testing.T) {
	snap := emptySnap()
	snap.Profiles.V.Profiles["dev"] = doc(registry.Profile{
		Servers:   []string{"fs", "git"},
		Discovery: "lazy",
		Tools: map[string]registry.Doc[registry.ToolSelector]{
			"fs": doc(registry.ToolSelector{Allow: []string{"read"}}),
		},
	})
	snap.Clients.V.Clients["claude-code"] = doc(registry.ClientEntry{Profile: "dev"})

	layers, diags := FromRegistry(snap, SessionKey{ClientID: "claude-code"})
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %v", diags)
	}
	pl := layerOfKind(t, layers, LayerProfile)
	if pl == nil {
		t.Fatal("missing profile layer")
	}
	if pl.Origin != "profiles.json#dev" {
		t.Errorf("profile origin = %q", pl.Origin)
	}
	if len(pl.Servers) != 2 || pl.Tools["fs"] == nil || len(pl.Tools["fs"].Allow) != 1 {
		t.Errorf("profile layer content wrong: %+v", pl)
	}
	// Discovery rides with the tool set it describes.
	if pl.Discovery == nil || *pl.Discovery != DiscoveryLazy {
		t.Errorf("profile discovery not mapped: %+v", pl.Discovery)
	}
	for _, l := range layers {
		if l.Kind != LayerGlobal && l.Kind != LayerProfile {
			t.Errorf("unexpected layer %s from a bound client: only global and profile "+
				"are persisted layers", l.Kind)
		}
	}
}

func TestFromRegistryFollowActiveWithoutActiveProfile(t *testing.T) {
	snap := emptySnap()
	snap.Clients.V.Clients["c"] = doc(registry.ClientEntry{}) // default followActive
	layers, diags := FromRegistry(snap, SessionKey{ClientID: "c"})
	if layerOfKind(t, layers, LayerProfile) != nil {
		t.Error("no active profile must yield no profile layer (full set)")
	}
	if len(diags) != 0 {
		t.Errorf("unexpected diags: %v", diags)
	}
}

// followActive must actually FOLLOW. activeProfileName was hardcoded to ""
// while `agenthub profile use` wrote the name to a state file that scope
// resolution never read, so the marker could be set and listed while no
// session ever applied it — a narrowing the operator believed was in force
// and was not.
func TestFromRegistryFollowActiveAppliesTheActiveProfile(t *testing.T) {
	snap := emptySnap()
	snap.Profiles.V.Profiles["payments"] = doc(registry.Profile{Servers: []string{"stripe"}})
	snap.Governance.V.ActiveProfile = "payments"
	snap.Clients.V.Clients["c"] = doc(registry.ClientEntry{}) // default followActive

	layers, diags := FromRegistry(snap, SessionKey{ClientID: "c"})
	pl := layerOfKind(t, layers, LayerProfile)
	if pl == nil {
		t.Fatal("the active profile produced no layer: followActive did not follow")
	}
	if len(pl.Servers) != 1 || pl.Servers[0] != "stripe" {
		t.Fatalf("profile layer servers = %#v, want [stripe]", pl.Servers)
	}
	if len(diags) != 0 {
		t.Errorf("unexpected diags: %v", diags)
	}
}

// An active marker naming a profile that does not exist takes the SAME
// fail-closed path a named binding does: block-all plus a diagnostic, never
// a silent widening back to the full server set.
func TestFromRegistryDanglingActiveProfileFailsClosed(t *testing.T) {
	snap := emptySnap()
	snap.Governance.V.ActiveProfile = "ghost"
	snap.Clients.V.Clients["c"] = doc(registry.ClientEntry{})

	layers, diags := FromRegistry(snap, SessionKey{ClientID: "c"})
	pl := layerOfKind(t, layers, LayerProfile)
	if pl == nil {
		t.Fatal("a dangling active marker must still produce a profile layer")
	}
	if pl.Servers == nil || len(pl.Servers) != 0 {
		t.Fatalf("dangling active profile must be block-all ([]), got %#v", pl.Servers)
	}
	if len(diags) == 0 {
		t.Error("a dangling active marker must be reported, never silent")
	}
}

// An explicitly named binding still wins over the active marker: the marker
// is the fallback for clients that name nothing.
func TestFromRegistryNamedBindingBeatsTheActiveProfile(t *testing.T) {
	snap := emptySnap()
	snap.Profiles.V.Profiles["payments"] = doc(registry.Profile{Servers: []string{"stripe"}})
	snap.Profiles.V.Profiles["ops"] = doc(registry.Profile{Servers: []string{"grafana"}})
	snap.Governance.V.ActiveProfile = "payments"
	snap.Clients.V.Clients["c"] = doc(registry.ClientEntry{Profile: "ops"})

	layers, _ := FromRegistry(snap, SessionKey{ClientID: "c"})
	pl := layerOfKind(t, layers, LayerProfile)
	if pl == nil || len(pl.Servers) != 1 || pl.Servers[0] != "grafana" {
		t.Fatalf("profile layer = %#v, want the client's own binding [grafana]", pl)
	}
}

func TestFromRegistryDanglingProfileFailClosed(t *testing.T) {
	snap := emptySnap()
	snap.Clients.V.Clients["c"] = doc(registry.ClientEntry{Profile: "payments"})
	layers, diags := FromRegistry(snap, SessionKey{ClientID: "c"})

	pl := layerOfKind(t, layers, LayerProfile)
	if pl == nil {
		t.Fatal("dangling reference must still produce a profile layer")
	}
	if pl.Servers == nil || len(pl.Servers) != 0 {
		t.Fatalf("dangling profile must be block-all ([]), got %#v", pl.Servers)
	}
	if len(diags) != 1 || !strings.Contains(diags[0].Message, `dangling profile "payments"`) {
		t.Fatalf("dangling must be diagnosed, never silent: %v", diags)
	}

	// End-to-end: the merged scope is empty even with a full catalog.
	es, err := MergeWithDiagnostics(layers, testCatalog(), diags)
	if err != nil {
		t.Fatal(err)
	}
	if len(es.Servers) != 0 {
		t.Errorf("dangling profile leaked visibility: %v", es.Servers)
	}
	if len(es.Diags) != 1 {
		t.Error("diagnostics must be folded into the EffectiveScope value")
	}
}

func TestFromRegistryExplicitProfileRef(t *testing.T) {
	snap := emptySnap()
	snap.Profiles.V.Profiles["dev"] = doc(registry.Profile{Servers: []string{"fs"}})
	snap.Clients.V.Clients["c"] = doc(registry.ClientEntry{
		Profile:    "dev", // shorthand loses to the explicit ref below
		ProfileRef: docPtr(registry.ProfileBinding{Kind: registry.BindingFollowActive}),
	})
	layers, diags := FromRegistry(snap, SessionKey{ClientID: "c"})
	if layerOfKind(t, layers, LayerProfile) != nil {
		t.Error("explicit followActive ref must beat the named shorthand")
	}
	if len(diags) != 0 {
		t.Errorf("diags: %v", diags)
	}
}

// FromRegistry must not alias snapshot memory: mutating returned layers
// must never write through into the shared read-only snapshot.
func TestFromRegistryDoesNotAliasSnapshot(t *testing.T) {
	snap := emptySnap()
	snap.Profiles.V.Profiles["dev"] = doc(registry.Profile{
		Servers: []string{"fs"},
		Tools:   map[string]registry.Doc[registry.ToolSelector]{"fs": doc(registry.ToolSelector{Allow: []string{"read"}})},
	})
	snap.Clients.V.Clients["c"] = doc(registry.ClientEntry{Profile: "dev"})

	layers, _ := FromRegistry(snap, SessionKey{ClientID: "c"})
	pl := layerOfKind(t, layers, LayerProfile)
	pl.Servers[0] = "tampered"
	pl.Tools["fs"].Allow[0] = "tampered"

	if snap.Profiles.V.Profiles["dev"].V.Servers[0] != "fs" {
		t.Error("layer Servers aliases snapshot memory")
	}
	if snap.Profiles.V.Profiles["dev"].V.Tools["fs"].V.Allow[0] != "read" {
		t.Error("layer Tools aliases snapshot memory")
	}
}

// Guard the router.Catalog contract scope depends on: original names only,
// sorted and deduplicated.
func TestRouterCatalogNormalization(t *testing.T) {
	cat := router.NewCatalog(map[string][]string{"s": {"b", "a", "b"}})
	got := cat.Servers["s"]
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("NewCatalog must sort+dedup, got %v", got)
	}
}

// TestServerToolAllowListReachesTheScope covers the global per-server allow
// list in all three of its states, including the one that fails open when a
// re-implementation collapses nil and empty: a server with `tools: []` must
// expose NOTHING, not everything.
func TestServerToolAllowListReachesTheScope(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
		want  []string
	}{
		{"no rule exposes the whole server", nil, []string{"delete", "read", "write"}},
		{"a subset exposes exactly that subset", []string{"read"}, []string{"read"}},
		{"an empty list exposes nothing", []string{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := emptySnap()
			snap.Servers.V.Servers["fs"] = doc(registry.ServerEntry{Enabled: true, Tools: tc.tools})
			layers, _ := FromRegistry(snap, SessionKey{ClientID: "c", SessionID: "c:1"})
			es, err := Merge(layers, testCatalog())
			if err != nil {
				t.Fatal(err)
			}
			view, ok := es.Servers["fs"]
			if !ok {
				t.Fatalf("server fs is not in the scope at all: %+v", es.Servers)
			}
			got := view.Tools
			if len(got) != len(tc.want) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("tools = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestDiscoveryForMatchesTheResolvedSession is the parity that justifies
// DiscoveryFor existing at all: `profile ls` prints the mode a client bound
// to that profile would actually be served, so the two answers must be the
// same value for every combination of global and per-profile setting. A
// listing that drifts from the resolution it describes is worse than the
// dash it replaced — the dash at least admitted it was not the answer.
func TestDiscoveryForMatchesTheResolvedSession(t *testing.T) {
	for _, tc := range []struct {
		name     string
		global   string
		profile  string
		want     DiscoveryMode
		wantFrom LayerKind
		wantSet  bool
	}{
		{name: "nothing set", wantFrom: LayerGlobal},
		{name: "global only", global: "grouped", want: DiscoveryGrouped, wantFrom: LayerGlobal, wantSet: true},
		{name: "profile only", profile: "full", want: DiscoveryFull, wantFrom: LayerProfile, wantSet: true},
		{
			name: "profile overrides global", global: "full", profile: "lazy",
			want: DiscoveryLazy, wantFrom: LayerProfile, wantSet: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := emptySnap()
			snap.Governance.V.Discovery = tc.global
			snap.Profiles.V.Profiles["dev"] = doc(registry.Profile{Discovery: tc.profile})
			snap.Clients.V.Clients["claude-code"] = doc(registry.ClientEntry{Profile: "dev"})

			got, from, ok := DiscoveryFor(snap, "dev")
			if got != tc.want || from != tc.wantFrom || ok != tc.wantSet {
				t.Errorf("DiscoveryFor = (%q, %s, %v), want (%q, %s, %v)",
					got, from, ok, tc.want, tc.wantFrom, tc.wantSet)
			}

			layers, _ := FromRegistry(snap, SessionKey{ClientID: "claude-code"})
			es, err := Merge(layers, testCatalog())
			if err != nil {
				t.Fatal(err)
			}
			if es.Discovery != got {
				t.Errorf("DiscoveryFor = %q but the resolved session gets %q", got, es.Discovery)
			}
		})
	}
}

// An unbound client follows the active profile, and DiscoveryFor is asked
// about the SAME name — including the empty one, which is what `profile use -`
// leaves behind and what the `(default)` row of `profile ls` describes.
func TestDiscoveryForFollowsTheActiveProfile(t *testing.T) {
	snap := emptySnap()
	snap.Governance.V.Discovery = "grouped"
	snap.Governance.V.ActiveProfile = "dev"
	snap.Profiles.V.Profiles["dev"] = doc(registry.Profile{Discovery: "full"})

	got, from, ok := DiscoveryFor(snap, snap.Governance.V.ActiveProfile)
	if got != DiscoveryFull || from != LayerProfile || !ok {
		t.Errorf("active profile discovery = (%q, %s, %v), want (full, profile, true)", got, from, ok)
	}

	layers, _ := FromRegistry(snap, SessionKey{ClientID: "unbound"})
	es, err := Merge(layers, testCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if es.Discovery != got {
		t.Errorf("DiscoveryFor = %q but an unbound session gets %q", got, es.Discovery)
	}

	// No active profile: the global layer is the whole chain.
	snap.Governance.V.ActiveProfile = ""
	if got, from, ok := DiscoveryFor(snap, ""); got != DiscoveryGrouped || from != LayerGlobal || !ok {
		t.Errorf("no active profile = (%q, %s, %v), want (grouped, global, true)", got, from, ok)
	}
}

// A name that resolves nowhere costs VISIBILITY, decided in Merge; the
// presentation question still has the global answer, so DiscoveryFor must not
// invent a third state for it.
func TestDiscoveryForDanglingNameFallsBackToGlobal(t *testing.T) {
	snap := emptySnap()
	snap.Governance.V.Discovery = "full"
	got, from, ok := DiscoveryFor(snap, "ghost")
	if got != DiscoveryFull || from != LayerGlobal || !ok {
		t.Errorf("dangling = (%q, %s, %v), want (full, global, true)", got, from, ok)
	}
	if _, _, ok := DiscoveryFor(nil, "ghost"); ok {
		t.Error("a nil snapshot must report that no layer set a mode")
	}
}
