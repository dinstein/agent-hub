package gateway

import (
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/scope"
)

// This file is the gateway's visibility plane (docs/architecture.md §7): the
// EffectiveScope is a QUERY-TIME projection over the connection plane. The
// two planes are deliberately separate — invariant 2: narrowing the scope
// (profile edit, session overlay) never touches a downstream connection;
// only servers.json spec changes reconnect anything (see hotreload.go).

// scopeKey identifies this process's single upstream session for the
// resolver cache. The session id is the daemon-assigned one when the
// control link is registered ("" when standalone); a re-registration mints
// a new id and therefore a fresh cache slot — a stale overlay from a dead
// session can never be served under the new identity.
func (g *gateway) scopeKey() scope.SessionKey {
	sid := ""
	if g.ctl != nil {
		sid = g.ctl.Session()
	}
	return scope.SessionKey{
		ClientID:  g.cfg.ClientID,
		SessionID: scope.SessionID(sid),
		// Root is the client's first reported root, read from the cache only
		// (cachedPrimaryRoot explains why this must not block).
		//
		// It no longer decides anything about visibility: no persisted layer
		// reads it since the per-project layer was retired, and it is not in
		// the resolver's cache key. An empty root — client declares no roots
		// capability, reports none, or the prefetch has not landed yet —
		// therefore cannot change what this session resolves to. It is
		// carried because the key is also the identity handed to derivation
		// (downstream derives per-root server instances from it).
		Root: g.cachedPrimaryRoot(),
	}
}

// currentScope resolves the session's effective scope through the cached
// resolver. Returns nil only when scope authority does not exist at all
// (no registry store — see newGateway); with a store present, a resolution
// failure returns an EMPTY scope (zero visible servers): fail-closed, an
// error must never widen visibility.
func (g *gateway) currentScope() *scope.EffectiveScope {
	if g.scopeRes == nil {
		return nil
	}
	es, err := g.scopeRes.Resolve(g.lifeCtx, g.scopeKey())
	if err != nil {
		g.log.Warn("scope resolution failed; failing closed to an empty scope", "error", err)
		return &scope.EffectiveScope{Servers: map[string]scope.ToolView{}}
	}
	return es
}

// catalogSnapshot is the scope.Sources.Catalog input: the raw-name tool
// directory of whatever router currently serves (cache-built before ready,
// live after), so scope intersection and tools/list can never disagree
// about which catalog they describe.
func (g *gateway) catalogSnapshot() router.Catalog {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cat
}

// catalogFromRouter projects a router back into its raw (server, tool)
// catalog via RouteOf — the only legitimate reverse mapping (never by
// splitting exposed names).
func catalogFromRouter(rt *router.Router) router.Catalog {
	m := make(map[string][]string)
	for _, def := range rt.List() {
		route, ok := rt.RouteOf(def.Name)
		if !ok {
			continue // unroutable listing cannot happen by construction
		}
		m[route.ServerID] = append(m[route.ServerID], route.RawTool)
	}
	return router.NewCatalog(m)
}

// The tools/list view projection lives in discovery.Visible (called from
// currentSurface): it drops every aggregated tool whose route is not visible
// under the effective scope, using pipeline.ScopeAllows — the identical
// predicate the pipeline's scope gate enforces on the execute path. The
// router itself is NOT rebuilt and no downstream connection is touched
// (docs/architecture.md §7 invariant 2 — visibility is a query-time projection).

// refreshScopeAndNotify recomputes the effective scope and pushes
// tools/list_changed iff the CONTENT hash moved (docs/architecture.md §7: only a
// content change warrants a push — no rebuild amplification).
func (g *gateway) refreshScopeAndNotify() {
	es := g.currentScope() // resolve OUTSIDE g.mu (resolver takes its own locks)
	g.mu.Lock()
	changed := scope.Changed(g.lastScope, es)
	g.lastScope = es
	initialized := g.initialized
	g.mu.Unlock()
	if !changed {
		return
	}
	// A content change may have moved the discovery MODE as well, so the
	// surface the client sees is a different one. The cached surface needs no
	// explicit invalidation (its key carries the scope hash), but the search
	// guard does: its streak describes a tool surface that no longer exists
	// (docs/architecture.md §7).
	g.guard.Reset()
	if initialized {
		g.notifyToolsChanged()
	}
}

// onSessionChanged reacts to a daemon-session transition (registration, link
// loss). Invalidation clears the whole cache rather than one session: the
// session identity itself has just changed, and this process hosts exactly
// one upstream session — over-invalidation is a cheap recompute (the closed
// direction; under-invalidation would serve a stale scope).
func (g *gateway) onSessionChanged() {
	if g.scopeRes == nil {
		return
	}
	g.scopeRes.Invalidate(scope.Event{Kind: scope.EvRegistryChanged})
	g.refreshScopeAndNotify()
}
