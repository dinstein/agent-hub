package scope

import (
	"context"
	"errors"
	"sync"

	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/router"
)

// EventKind classifies scope-cache invalidation events (docs/model.md).
// Event-driven, never polled.
type EventKind uint8

const (
	// EvRegistryChanged: registry generation changed — drop ALL cached scopes.
	EvRegistryChanged EventKind = iota
	// EvRootChanged: roots/list_changed for one session — drop that session
	// only. Since the per-project layer was retired no persisted layer reads
	// the root, so this can no longer change a resolution; it is kept because
	// dropping one session's entry is cheap and the alternative (having
	// callers decide which notices matter) is how a stale scope gets served
	// the next time something does depend on the root.
	EvRootChanged
	// EvCatalogChanged: downstream tool set changed — drop ALL. The catalog
	// is NOT part of the cache key tuple, so this event is the only thing
	// that makes a tool-set change visible; losing it would serve stale
	// scopes (which is why it clears everything, the closed direction).
	EvCatalogChanged
)

// Event is one invalidation notice. The per-session events need a target, so
// the kind is its own type and Event carries the optional session (empty ⇒
// ignored for the global kinds).
type Event struct {
	Kind    EventKind
	Session SessionID // required for EvRootChanged
}

// Resolver is the cached resolution entry point. The core Merge stays a
// pure function and is independently unit-testable; Resolve only adds
// caching around FromRegistry + Merge.
type Resolver interface {
	Resolve(ctx context.Context, key SessionKey) (*EffectiveScope, error)
	Invalidate(ev Event)
}

// Sources supplies the live inputs of resolution. All funcs must be safe
// for concurrent use; they are called outside any resolver lock.
type Sources struct {
	// Registry returns the current registry snapshot (nil = not loaded yet,
	// Resolve errors out — fail-closed, never resolve against nothing).
	Registry func() *registry.Snapshot
	// Catalog returns the current tool-directory snapshot. Nil func or an
	// empty catalog resolves to zero visible servers (closed direction).
	Catalog func() router.Catalog
	// Extra supplies additional layers appended after the persisted ones — the seam
	// through which a credential narrows a session it does not own a
	// registry entry for (the daemon's HTTP data plane folds an agent
	// token's server allowlist and profile pin in here). They join the
	// three-layer resolution chain of docs/model.md; they do not
	// stand outside it.
	//
	// Failure direction: these are ordinary layers, so Merge treats them
	// exactly like the persisted ones — security fields intersect. An Extra
	// layer can therefore only
	// TIGHTEN; there is no shape of it that widens visibility.
	//
	// Contract: the returned layers must be a pure function of the session
	// id and the registry generation. They are NOT part of the cache key, so
	// a source that varies independently of those two would serve a stale
	// scope until the next invalidation.
	Extra func(SessionID) []ScopeLayer
}

// CachedResolver caches one EffectiveScope per session, validated by the
// tuple (clientID, registryGeneration) — docs/subsystems/scope.md#internalscope.
// Cache values are immutable *EffectiveScope, safe to share.
//
// The session's root is deliberately NOT part of the key. It was, while the
// per-project layer keyed on it; with that layer retired no persisted layer
// reads the root, so keeping it would split one client's cache across every
// directory it happens to report from — more misses for a value that cannot
// change the answer. The root still reaches internal/downstream, which
// derives per-root server instances from it.
type CachedResolver struct {
	src Sources

	mu    sync.Mutex
	cache map[SessionID]cacheEntry
}

type cacheEntry struct {
	clientID   string
	generation uint64
	scope      *EffectiveScope
}

// NewCachedResolver builds a resolver over the given sources.
// src.Registry must be non-nil.
func NewCachedResolver(src Sources) *CachedResolver {
	if src.Registry == nil {
		panic("scope: Sources.Registry is required")
	}
	return &CachedResolver{src: src, cache: make(map[SessionID]cacheEntry)}
}

// Resolve returns the session's effective scope, computing and caching it
// when the (clientID, generation) tuple has moved — the root is deliberately
// not part of it, for the reason CachedResolver's own comment gives.
// Concurrent resolves of the same key may compute twice; both results are
// identical values (Merge is pure) so last-store-wins is harmless.
func (r *CachedResolver) Resolve(ctx context.Context, key SessionKey) (*EffectiveScope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snap := r.src.Registry()
	if snap == nil {
		// Fail-closed: without a registry snapshot we refuse to resolve
		// rather than invent an empty-but-valid scope.
		return nil, errors.New("scope: no registry snapshot available")
	}
	r.mu.Lock()
	if e, ok := r.cache[key.SessionID]; ok &&
		e.clientID == key.ClientID &&
		e.generation == snap.Generation {
		r.mu.Unlock()
		return e.scope, nil
	}
	r.mu.Unlock()

	layers, diags := r.layersFor(snap, key)
	es, err := MergeWithDiagnostics(layers, r.catalog(), diags)
	if err != nil {
		return nil, err
	}
	es.Generation = snap.Generation // stamped after merge; Hash excludes it

	r.mu.Lock()
	r.cache[key.SessionID] = cacheEntry{
		clientID:   key.ClientID,
		generation: snap.Generation,
		scope:      es,
	}
	r.mu.Unlock()
	return es, nil
}

// layersFor composes the layer list Resolve merges: the persisted layers,
// then whatever Extra supplies, in resolution order.
//
// It exists so that order lives in ONE place. Explain returns the same list
// to a caller that wants to see the fold happen, and a second copy of the
// composition is how a diagnostic starts describing a resolution that no
// longer runs — the reason pickDiscovery is a function too.
func (r *CachedResolver) layersFor(snap *registry.Snapshot, key SessionKey) ([]ScopeLayer, []Diagnostic) {
	layers, diags := FromRegistry(snap, key)
	if r.src.Extra != nil {
		layers = append(layers, r.src.Extra(key.SessionID)...)
	}
	return layers, diags
}

// catalog reads the current tool directory. A nil func is an empty catalog,
// which resolves to zero visible servers — the closed direction.
func (r *CachedResolver) catalog() router.Catalog {
	if r.src.Catalog == nil {
		return router.Catalog{}
	}
	return r.src.Catalog()
}

// Explain reports how one key's scope converged: the shape remaining after
// each layer is folded in, in the order Resolve folds them.
//
// It answers the question the final shape cannot — WHICH layer narrowed it.
// The scope chain is three intersecting layers and none can widen, so a
// client seeing nothing has exactly one layer to blame, and a caller holding
// only the result has no way to tell which. It is deliberately separate from
// Resolve and never cached: this is a diagnostic, paid for only when someone
// asks, and Merge is pure so re-folding prefixes is free of side effects.
//
// Fail-closed like Resolve: no registry snapshot is an error, never an empty
// explanation that would read as "nothing narrowed anything".
func (r *CachedResolver) Explain(key SessionKey) ([]Step, error) {
	snap := r.src.Registry()
	if snap == nil {
		return nil, errors.New("scope: no registry snapshot available")
	}
	layers, _ := r.layersFor(snap, key)
	return Converge(layers, r.catalog())
}

// Invalidate applies one invalidation event (see EventKind for the
// clear-all vs per-session split). Unknown kinds clear everything —
// fail-closed: over-invalidation costs a recompute, under-invalidation
// serves a stale scope.
func (r *CachedResolver) Invalidate(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Kind {
	case EvRootChanged:
		delete(r.cache, ev.Session)
	case EvRegistryChanged, EvCatalogChanged:
		clear(r.cache)
	default:
		clear(r.cache)
	}
}

// cachedSessions reports the currently cached session IDs (test hook).
func (r *CachedResolver) cachedSessions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cache)
}
