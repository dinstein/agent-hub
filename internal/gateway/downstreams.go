package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/scope"
)

// refreshTimeout bounds a gateway-driven tools/list refresh after a
// downstream list_changed notification.
const refreshTimeout = 30 * time.Second

// connectAll dials every enabled downstream concurrently (docs/flows.md:
// the handshake is never blocked by a slow server). Each success rebuilds
// the live router, persists the server's tools into the cache, and pushes
// tools/list_changed upstream.
func (g *gateway) connectAll() {
	g.mu.Lock()
	specs := make([]downstream.Spec, len(g.specs))
	copy(specs, g.specs)
	g.mu.Unlock()
	for _, spec := range specs {
		go g.connectOne(spec)
	}
}

// connectOne dials one downstream and wires it in. A failure only logs and
// records the reason: the gateway keeps serving whatever else it has (cache
// and/or other servers), and the re-dial ladder (redial.go) brings this one
// back on its own.
//
// The caller MUST have claimed the dial slot (beginDialLocked, or the startup
// pre-claim in newGateway); finishDial releases it on every exit path.
func (g *gateway) connectOne(spec downstream.Spec) {
	defer g.finishDial(spec.ID)

	srv, err := downstream.Connect(g.lifeCtx, spec, g.downstreamDeps())

	if err != nil {
		g.log.Warn("downstream connect failed", logx.Server(spec.ID), "error", err)
		// Remember WHY: without it this server would report "connecting"
		// forever, which is the failure this whole state path exists to
		// stop (a red server that displays as merely slow).
		g.noteConnectResult(spec.ID, err)
		g.reportServers()
		return
	}
	if g.lifeCtx.Err() != nil {
		srv.Close() // lost the race against shutdown
		return
	}

	// Registrations survive respawn (downstream re-registers them).
	srv.OnListChanged(func(mask transport.ChangeMask) {
		if !mask.Has(transport.ChangeTools) {
			return
		}
		// The callback runs on the transport read loop and must not block:
		// refresh + rebuild happen on their own goroutine.
		go g.refreshServer(srv)
	})
	srv.OnPeerRequest(g.peerHandler(spec.ID))

	g.mu.Lock()
	cur, still := g.specByIDLocked(spec.ID)
	if !still || !specEqual(cur, spec) {
		// The registry moved while we were connecting: this spec was
		// removed or redefined. Never wire a stale definition into the
		// catalog. If the entry still exists it is the CURRENT definition
		// that now has no connection and no dial, so arm the ladder — the
		// reload that redefined it could not claim a slot while this dial
		// held one.
		if still {
			g.redialAt[spec.ID] = time.Time{}
		}
		g.mu.Unlock()
		srv.Close()
		g.log.Info("dropping connection of a stale server definition", logx.Server(spec.ID))
		return
	}
	old := g.servers[spec.ID]
	g.servers[spec.ID] = srv
	g.mu.Unlock()
	if old != nil {
		old.Close() // replaced by a concurrent reconnect race; keep the newest
	}

	g.noteConnectResult(spec.ID, nil)
	g.persistTools(srv)
	g.rebuildAndNotify()
	g.reportServers()
	g.log.Info("downstream connected", logx.Server(spec.ID), "tools", len(srv.Tools()))
}

// refreshServer re-queries a downstream's tools after list_changed, then
// rebuilds the catalog and re-persists the cache entry.
func (g *gateway) refreshServer(srv *downstream.Server) {
	ctx, cancel := context.WithTimeout(g.lifeCtx, refreshTimeout)
	defer cancel()
	if err := srv.RefreshTools(ctx); err != nil {
		g.log.Warn("tools refresh failed", logx.Server(srv.ID()), "error", err)
		// Fall through: rebuild with whatever list the server has cached.
	}
	g.persistTools(srv)
	g.rebuildAndNotify()
	g.reportServers() // the tool count just moved
}

// persistTools writes one server's current tool list into the on-disk
// cache (atomic write) so the next gateway start can answer before
// connecting.
func (g *gateway) persistTools(srv *downstream.Server) {
	if g.cache == nil {
		return
	}
	err := g.cache.write(srv.ID(), srv.Tools())
	switch {
	case err == nil:
	case errors.Is(err, errCacheSealed):
		// The gateway shut down while this connect goroutine was still in
		// flight. Refusing the write is the point (toolCache.seal), so this is
		// an expected outcome and not a warning — logging it as one would
		// train readers to ignore the line that means a real write failed.
		g.log.Debug("tool cache write skipped: gateway shut down", logx.Server(srv.ID()))
	default:
		g.log.Warn("tool cache write failed", logx.Server(srv.ID()), "error", err)
	}
}

// rebuildAndNotify swaps in a fresh live router built from every connected
// server and pushes tools/list_changed upstream (deferred until the
// upstream session is initialized).
func (g *gateway) rebuildAndNotify() {
	g.mu.Lock()
	servers := make([]*downstream.Server, 0, len(g.servers))
	for _, s := range g.servers {
		servers = append(servers, s)
	}
	g.mu.Unlock()

	providers := g.providers()
	rt, err := router.BuildWith(servers, providers)
	if err != nil {
		g.log.Error("router rebuild failed; keeping previous catalog", "error", err)
		return
	}
	g.swapCatalog(rt, true)
}

// buildColdCatalog aggregates the on-disk tool cache. It is the
// pre-connection catalog of docs/flows.md — listable, not callable for
// downstream tools — and never fails: an aggregation error degrades to an
// empty catalog.
func (g *gateway) buildColdCatalog() *router.Router {
	cached, providers := g.cachedCatalog, g.providers()
	rt, err := router.BuildFromCacheWith(cached, providers)
	if err != nil {
		g.log.Warn("tool cache aggregation failed; serving an empty catalog", "error", err)
		rt, _ = router.BuildFromCacheWith(nil, nil)
	}
	return rt
}

// swapCatalog publishes a freshly built catalog and pushes
// tools/list_changed upstream (deferred until the upstream session is
// initialized). live=false marks a cache-built catalog, which is DROPPED if
// a live one has taken over in the meantime — a cold catalog must never
// replace a live one.
func (g *gateway) swapCatalog(rt *router.Router, live bool) {
	cat := catalogFromRouter(rt)

	g.mu.Lock()
	if !live && g.ready {
		g.mu.Unlock()
		return
	}
	g.rt = rt
	g.cat = cat
	g.invalidateSurfaceLocked()
	if live {
		g.ready = true
	}
	initialized := g.initialized
	g.mu.Unlock()

	if g.scopeRes != nil {
		// The catalog is not part of the resolver cache key: this event is
		// the only thing that keeps cached scopes honest (clear-all, the
		// closed direction). The refreshed scope only re-baselines the
		// hash diff — the unconditional notify below covers this change.
		g.scopeRes.Invalidate(scope.Event{Kind: scope.EvCatalogChanged})
		es := g.currentScope()
		g.mu.Lock()
		g.lastScope = es
		g.mu.Unlock()
		// A catalog swap is the other way the visible surface moves, and it
		// bypasses refreshScopeAndNotify's hash diff entirely — the notify
		// below is unconditional — so without this the tools a server
		// brought or took away would change the scope with nothing recording
		// the new shape.
		g.logScopeShape("catalog changed", es)
	}
	if initialized {
		g.notifyToolsChanged()
	}
}

// notifyToolsChanged pushes notifications/tools/list_changed upstream, when
// this session is one that may receive it — see subscriptions.go: a
// 2026-07-28 session gets what it subscribed to and nothing else, an older
// one gets it as it always has.
func (g *gateway) notifyToolsChanged() {
	if !g.mayNotify(mcp.NotificationToolsListChanged) {
		return
	}
	g.reply(mcp.NewNotification(mcp.NotificationToolsListChanged, nil))
}

// peerHandler answers server-initiated reverse RPCs from one downstream.
// M0 serves roots/list from the gateway's RootSource; everything else is
// method-not-found. It runs on the downstream transport read loop; the
// roots fetch is normally served from the prefetched cache and is bounded
// by rootsTimeout otherwise.
// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
func (g *gateway) peerHandler(serverID string) transport.PeerHandler {
	return func(ctx context.Context, req *mcp.Request) (*mcp.Response, error) {
		switch req.Method {
		case mcp.MethodRootsList:
			rctx, cancel := context.WithTimeout(ctx, rootsTimeout)
			defer cancel()
			roots, err := g.roots.Roots(rctx)
			if err != nil {
				g.log.Warn("roots/list reverse RPC failed", logx.Server(serverID), "error", err)
				return nil, err // transport answers with an internal error
			}
			if roots == nil {
				roots = []mcp.Root{}
			}
			raw, merr := json.Marshal(mcp.ListRootsResult{Roots: roots})
			if merr != nil {
				return nil, merr
			}
			return mcp.NewResponse(req.ID, raw), nil
		default:
			return mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code:    mcp.CodeMethodNotFound,
				Message: fmt.Sprintf("gateway does not serve %q to downstream servers", req.Method),
			}), nil
		}
	}
}
