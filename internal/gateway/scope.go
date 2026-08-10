package gateway

import (
	"log/slog"

	"github.com/dinstein/agent-hub/internal/logx"
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
// (no registry store AND no narrowing credential — see newGateway); with a
// store present, a resolution failure returns an EMPTY scope (zero visible
// servers): fail-closed, an error must never widen visibility.
func (g *gateway) currentScope() *scope.EffectiveScope {
	if g.scopeRes == nil {
		if g.scopeFailClosed {
			// A narrowing credential with no store to resolve against: fail
			// closed to an empty scope, never the nil (allow-all) baseline.
			return &scope.EffectiveScope{Servers: map[string]scope.ToolView{}}
		}
		return nil
	}
	es, err := g.scopeRes.Resolve(g.lifeCtx, g.scopeKey())
	if err != nil {
		g.log.Warn("scope resolution failed; failing closed to an empty scope", "error", err)
		return &scope.EffectiveScope{Servers: map[string]scope.ToolView{}}
	}
	return es
}

// logScopeShape records what this session may currently reach, and why it is
// being said now.
//
// The scope chain is the whole thesis of the hub — what a client may reach is
// decided in advance, by configuration — and it was the one decision that
// left no trace. Three layers intersect, none can widen, and the result was
// visible only by asking `agenthub session`, from outside, after the fact. So
// the commonest question there is, "why can't my client see this tool", had
// nothing in the log to answer it, and the two readings it splits into —
// narrowed on purpose, or narrowed by a mistake — looked identical.
//
// Counts, not names: a hub fronting a dozen servers lists hundreds of tools,
// and a line that grew with the catalog would be unreadable exactly where it
// is needed. The names live in `agenthub session`, which is interactive and
// can afford them; what a log is for is noticing that the number moved.
//
// Called only where the resolved scope is BASELINED — startup, a content
// change, a catalog swap — never per resolution: currentScope runs on the
// tools/list and execute paths, and a line there would be per call.
func (g *gateway) logScopeShape(reason string, es *scope.EffectiveScope) {
	if es == nil {
		return // no registry store, hence no scope authority to describe
	}
	tools := 0
	for _, view := range es.Servers {
		tools += len(view.Tools)
	}
	g.log.Info("effective scope resolved", "reason", reason, logx.Rev(es.Generation),
		"servers", len(es.Servers), "tools", tools, "discovery", string(es.Discovery))
	// Diagnostics are part of the value and documented as never silent
	// (docs/architecture.md §7), but nothing in the gateway had ever read
	// them — `agenthub session` was the only consumer. A dangling profile
	// reference fails CLOSED to an empty scope, so the failure mode they
	// describe is a client that can suddenly see nothing, reported by the one
	// process that noticed and then kept it to itself.
	for _, d := range es.Diags {
		g.log.Warn("scope diagnostic", "layer", d.Layer.String(),
			"origin", d.Origin, "detail", d.Message)
	}
	g.logScopeConvergence()
}

// logScopeConvergence writes, at Debug, the shape each layer of the chain
// leaves behind, in the order they are folded.
//
// The Info line above says what a session ended up with; this says which
// layer took the rest away. The chain is an intersection and no layer can
// widen, so a client seeing nothing has exactly ONE layer to blame — and
// picking it out of a global rule, a server's own allow list, a profile and a
// session overlay was otherwise guesswork against three config files.
//
// Gated on the level, which is not a micro-optimisation: Explain re-folds the
// layer list once per layer, so the work is real and must not be done when
// nobody will read it. Off, it costs one Enabled call.
//
// A failure is swallowed to Debug. This is the explanation of a scope, not
// the scope: a diagnostic able to disturb a resolution would be a worse
// trade than the missing explanation.
func (g *gateway) logScopeConvergence() {
	if g.scopeRes == nil || !g.log.Enabled(g.lifeCtx, slog.LevelDebug) {
		return
	}
	steps, err := g.scopeRes.Explain(g.scopeKey())
	if err != nil {
		g.log.Debug("scope convergence unavailable", "error", err)
		return
	}
	for _, s := range steps {
		g.log.Debug("scope layer applied", "layer", s.Layer.String(),
			"origin", s.Origin, "servers", s.Servers, "tools", s.Tools)
	}
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
	// Only the changes are reported. currentScope above runs whether or not
	// anything moved, and a line per resolution would say the same thing
	// every time the registry was touched.
	g.logScopeShape("recomputed", es)
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
