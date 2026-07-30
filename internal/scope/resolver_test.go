package scope

import (
	"context"
	"sync"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/router"
)

// testSources is a mutable Sources backend for resolver tests.
type testSources struct {
	mu   sync.Mutex
	snap *registry.Snapshot
	cat  router.Catalog
}

func newTestSources() *testSources {
	return &testSources{
		snap: emptySnap(),
		cat:  testCatalog(),
	}
}

func (s *testSources) sources() Sources {
	return Sources{
		Registry: func() *registry.Snapshot { s.mu.Lock(); defer s.mu.Unlock(); return s.snap },
		Catalog:  func() router.Catalog { s.mu.Lock(); defer s.mu.Unlock(); return s.cat },
	}
}

func (s *testSources) setCatalog(c router.Catalog) { s.mu.Lock(); s.cat = c; s.mu.Unlock() }

func (s *testSources) setSnap(sn *registry.Snapshot) { s.mu.Lock(); s.snap = sn; s.mu.Unlock() }

func key1() SessionKey { return SessionKey{ClientID: "claude-code", SessionID: "claude-code:1"} }

func key2() SessionKey { return SessionKey{ClientID: "cursor", SessionID: "cursor:1"} }

func TestResolveCacheHit(t *testing.T) {
	src := newTestSources()
	r := NewCachedResolver(src.sources())
	ctx := context.Background()

	a, err := r.Resolve(ctx, key1())
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Resolve(ctx, key1())
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("unchanged tuple must return the cached *EffectiveScope")
	}
	if a.Generation != 1 {
		t.Errorf("generation = %d, want 1", a.Generation)
	}
}

func TestResolveAutoMissOnTupleChange(t *testing.T) {
	src := newTestSources()
	r := NewCachedResolver(src.sources())
	ctx := context.Background()

	a, _ := r.Resolve(ctx, key1())

	// Registry generation moved: recompute without any explicit event.
	snap2 := emptySnap()
	snap2.Generation = 2
	src.setSnap(snap2)
	b, _ := r.Resolve(ctx, key1())
	if a == b || b.Generation != 2 {
		t.Error("generation bump must recompute")
	}

	c := b

	// Root moved: a HIT, not a miss. No persisted layer reads the root since
	// the per-project layer was retired, so keying on it would split one
	// client's cache across every directory it reports from while never
	// changing the answer.
	k := key1()
	k.Root = "/some/proj"
	d, _ := r.Resolve(ctx, k)
	if d != c {
		t.Error("root is not part of the key: a root change must serve the cached scope")
	}
}

func TestInvalidateCatalogChanged(t *testing.T) {
	src := newTestSources()
	r := NewCachedResolver(src.sources())
	ctx := context.Background()

	a, _ := r.Resolve(ctx, key1())

	// The catalog is NOT part of the cache tuple: without the event the
	// stale value keeps being served (that is exactly why the event exists).
	src.setCatalog(router.NewCatalog(map[string][]string{"fs": {"read"}}))
	b, _ := r.Resolve(ctx, key1())
	if a != b {
		t.Fatal("catalog change without event should still hit the cache")
	}

	r.Invalidate(Event{Kind: EvCatalogChanged})
	c, _ := r.Resolve(ctx, key1())
	if !Changed(a, c) {
		t.Error("EvCatalogChanged must force a recompute against the new catalog")
	}
	if len(c.Servers) != 1 {
		t.Errorf("new catalog not reflected: %v", c.Servers)
	}
}

func TestInvalidateRegistryChangedClearsAll(t *testing.T) {
	src := newTestSources()
	r := NewCachedResolver(src.sources())
	ctx := context.Background()

	_, _ = r.Resolve(ctx, key1())
	_, _ = r.Resolve(ctx, key2())
	if r.cachedSessions() != 2 {
		t.Fatalf("cached = %d, want 2", r.cachedSessions())
	}
	r.Invalidate(Event{Kind: EvRegistryChanged})
	if r.cachedSessions() != 0 {
		t.Errorf("EvRegistryChanged must clear ALL sessions, %d left", r.cachedSessions())
	}
}

func TestResolveNoSnapshotFails(t *testing.T) {
	r := NewCachedResolver(Sources{Registry: func() *registry.Snapshot { return nil }})
	if _, err := r.Resolve(context.Background(), key1()); err == nil {
		t.Fatal("nil snapshot must fail-closed with an error")
	}
}

func TestResolveContextCancelled(t *testing.T) {
	src := newTestSources()
	r := NewCachedResolver(src.sources())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Resolve(ctx, key1()); err == nil {
		t.Fatal("cancelled context must abort resolution")
	}
}

func TestResolverConcurrency(t *testing.T) {
	src := newTestSources()
	r := NewCachedResolver(src.sources())
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				k := key1()
				if n%2 == 0 {
					k = key2()
				}
				if _, err := r.Resolve(ctx, k); err != nil {
					t.Errorf("resolve: %v", err)
					return
				}
				switch j % 4 {
				case 0:
					r.Invalidate(Event{Kind: EvCatalogChanged})
				case 1:
					r.Invalidate(Event{Kind: EvRootChanged, Session: k.SessionID})
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestResolveExtraLayersOnlyTighten pins the contract of Sources.Extra: the
// credential layers a caller supplies go through the SAME Merge as the
// persisted five, so they intersect. An Extra layer naming a server that is
// not in the catalog cannot conjure it, and one naming a subset narrows.
func TestResolveExtraLayersOnlyTighten(t *testing.T) {
	src := newTestSources()
	s := src.sources()
	s.Extra = func(SessionID) []ScopeLayer {
		return []ScopeLayer{
			{Kind: LayerSession, Origin: "token:agent", Servers: []string{"git", "nonexistent"}},
			{Kind: LayerProfile, Origin: "profiles.json#pinned",
				Tools: map[string]*ToolSelector{"git": {Allow: []string{"log"}}}},
		}
	}
	r := NewCachedResolver(s)

	es, err := r.Resolve(context.Background(), key1())
	if err != nil {
		t.Fatal(err)
	}
	if len(es.Servers) != 1 {
		t.Fatalf("visible servers = %v, want only git", es.Servers)
	}
	// "nonexistent" was named by the extra layer and is still invisible:
	// intersection cannot add.
	if _, ok := es.Servers["nonexistent"]; ok {
		t.Error("an Extra layer widened visibility to a server outside the catalog")
	}
	tools := toolsOf(t, es, "git")
	if len(tools) != 1 || tools[0] != "log" {
		t.Errorf("git tools = %v, want [log] from the pinned profile allow list", tools)
	}
}

// TestPinnedProfileLayerFailsClosed: a pin that does not resolve yields a
// block-all layer, never a pass-through.
func TestPinnedProfileLayerFailsClosed(t *testing.T) {
	layer, ok := PinnedProfileLayer(emptySnap(), "does-not-exist")
	if ok {
		t.Fatal("a missing profile reported ok")
	}
	if layer.Servers == nil || len(layer.Servers) != 0 {
		t.Fatalf("Servers = %v, want the empty (block-all) slice, not nil (no intervention)", layer.Servers)
	}
	es, err := Merge([]ScopeLayer{layer}, testCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if len(es.Servers) != 0 {
		t.Fatalf("a dangling pin left %v visible; want nothing", es.Servers)
	}
}
