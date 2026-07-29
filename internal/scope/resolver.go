package scope

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/router"
)

// EventKind classifies scope-cache invalidation events (docs/architecture.md §7).
// Event-driven, never polled.
type EventKind uint8

const (
	// EvRegistryChanged: registry generation changed — drop ALL cached scopes.
	EvRegistryChanged EventKind = iota
	// EvOverlayChanged: one session's overlay mutated — drop that session only.
	EvOverlayChanged
	// EvRootChanged: roots/list_changed for one session — drop that session only.
	EvRootChanged
	// EvCatalogChanged: downstream tool set changed — drop ALL. The catalog
	// is NOT part of the cache key tuple, so this event is the only thing
	// that makes a tool-set change visible; losing it would serve stale
	// scopes (which is why it clears everything, the closed direction).
	EvCatalogChanged
)

// Event is one invalidation notice. docs/architecture.md §7 sketches Event as a bare
// enum; the per-session events need a target, so the enum is EventKind and
// Event carries the optional session (empty ⇒ ignored for the global kinds).
type Event struct {
	Kind    EventKind
	Session SessionID // required for EvOverlayChanged / EvRootChanged
}

// Resolver is the cached resolution entry point. The core Merge stays a
// pure function and is independently unit-testable; Resolve only adds
// caching around FromRegistry + Overlay + Merge.
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
	// Overlay returns the session's live overlay, or nil when it has none.
	Overlay func(SessionID) *Overlay
	// Extra supplies additional layers appended AFTER the overlay — the seam
	// through which a credential narrows a session it does not own a
	// registry entry for (the daemon's HTTP data plane folds an agent
	// token's server allowlist and profile pin in here, docs/architecture.md §9
	// "profile is the sixth constraint source in the scope intersection").
	//
	// Failure direction: these are ordinary layers, so Merge treats them
	// exactly like the persisted five — security fields intersect, deny
	// unions, approval switches OR. An Extra layer can therefore only
	// TIGHTEN; there is no shape of it that widens visibility.
	//
	// Contract: the returned layers must be a pure function of the session
	// id and the registry generation. They are NOT part of the cache key, so
	// a source that varies independently of those two would serve a stale
	// scope until the next invalidation.
	Extra func(SessionID) []ScopeLayer
}

// CachedResolver caches one EffectiveScope per session, validated by the
// tuple (registryGeneration, overlayVersion, normalizedRoot) — docs/architecture.md §7
// . Cache values are immutable *EffectiveScope, safe to share.
type CachedResolver struct {
	src Sources

	mu    sync.Mutex
	cache map[SessionID]cacheEntry
}

type cacheEntry struct {
	clientID   string
	generation uint64
	overlayVer uint64
	root       string
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
// when the (generation, overlayVersion, root) tuple has moved. Concurrent
// resolves of the same key may compute twice; both results are identical
// values (Merge is pure) so last-store-wins is harmless.
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
	var ov *Overlay
	if r.src.Overlay != nil {
		ov = r.src.Overlay(key.SessionID)
	}
	var overlayVer uint64
	if ov != nil {
		overlayVer = ov.Version
	}
	root := NormalizePath(key.Root)

	r.mu.Lock()
	if e, ok := r.cache[key.SessionID]; ok &&
		e.clientID == key.ClientID &&
		e.generation == snap.Generation &&
		e.overlayVer == overlayVer &&
		e.root == root {
		r.mu.Unlock()
		return e.scope, nil
	}
	r.mu.Unlock()

	nkey := key
	nkey.Root = root
	layers, diags := FromRegistry(snap, nkey)
	if ov != nil {
		layers = append(layers, ov.Layer(fmt.Sprintf("session:%s", key.SessionID)))
	}
	if r.src.Extra != nil {
		layers = append(layers, r.src.Extra(key.SessionID)...)
	}
	var cat router.Catalog
	if r.src.Catalog != nil {
		cat = r.src.Catalog()
	}
	es, err := MergeWithDiagnostics(layers, cat, diags)
	if err != nil {
		return nil, err
	}
	es.Generation = snap.Generation // stamped after merge; Hash excludes it

	r.mu.Lock()
	r.cache[key.SessionID] = cacheEntry{
		clientID:   key.ClientID,
		generation: snap.Generation,
		overlayVer: overlayVer,
		root:       root,
		scope:      es,
	}
	r.mu.Unlock()
	return es, nil
}

// Invalidate applies one invalidation event (see EventKind for the
// clear-all vs per-session split). Unknown kinds clear everything —
// fail-closed: over-invalidation costs a recompute, under-invalidation
// serves a stale scope.
func (r *CachedResolver) Invalidate(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Kind {
	case EvOverlayChanged, EvRootChanged:
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
