package gateway

import (
	"maps"
	"slices"
	"time"

	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/mcp/transport"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/scope"
)

// This file is the gateway's configuration hot-reload plane
// (docs/subsystems/registry.md, canonical.md §5c). Change notifications arrive
// on TWO channels —
// the local registry watcher (fsnotify + poll) and the daemon control link
// (LinkEventRegistry) — and both funnel into onRegistryChange, which
// re-reads the registry itself (a Change is a notification, never a
// snapshot) and adopts what it read iff generation >= applied (Applier).
//
// Impact routing by document kind:
//   - servers: diff the enabled spec set — only added/removed/changed
//     servers are (re)connected or closed; unchanged connections are kept
//     (docs/flows.md: no respawn storms), then the router is rebuilt.
//   - profiles / clients / governance: scope inputs only — invalidate the
//     scope cache, recompute, and push tools/list_changed iff the hash
//     moved. No downstream connection is ever touched.

// startWatch attaches the local registry watcher. Best-effort: a watcher
// failure only costs local hot reload (the daemon link channel still
// works); the gateway serves either way.
func (g *gateway) startWatch() {
	if g.store == nil {
		return
	}
	w, err := g.store.Watch()
	if err != nil {
		g.log.Warn("registry watch unavailable; hot reload via daemon link only", "error", err)
		return
	}
	g.watcher = w
	g.watchWG.Add(1)
	go func() {
		defer g.watchWG.Done()
		for ch := range w.Events() {
			g.onRegistryChange(ch.Kind)
		}
	}()
}

// onRegistryChange handles one change notification from either channel:
// reload, adopt by the >= criterion, route the impact by kind, refresh the
// scope. Safe to call concurrently; reloads serialize.
func (g *gateway) onRegistryChange(kind registry.DocKind) {
	if g.store == nil {
		return
	}
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()

	// reloadFailed records that this process is still serving the PREVIOUS
	// generation. It is the one gateway state that leaves no other trace: the
	// config on disk says one thing, every reader of it agrees, and the
	// running process quietly disagrees until something restarts it. Both
	// branches below reach it, because "the file would not load" and "the
	// snapshot would not apply" leave the client in the same place.
	reloadFailed := func(msg string, err error) {
		g.eventStream().Emit(g.log, eventlog.Record{
			Scope: eventlog.ScopeGateway, Kind: eventlog.KindRegistryReloadFailed,
			Client: g.cfg.ClientID, Detail: err.Error(),
		}, msg, "error", err)
	}

	snap, err := g.store.Reload(g.lifeCtx)
	if err != nil {
		// Half-written file or transient lock failure: keep serving the old
		// config; the next debounce/poll/link event retries (load failure
		// never advances the applied state — docs/subsystems/registry.md).
		reloadFailed("registry reload failed; keeping previous config", err)
		return
	}
	adopted, aerr := g.applier.Apply(snap.Generation, func() error {
		g.snap.Store(snap)
		// Inside the apply, so the trace state and the config it came from
		// can never disagree. It is what makes `server trace <id> on` reach
		// a client that is already running: the flip is a change to the
		// entry, not to the connection, so nothing reconnects.
		g.traces.apply(snap)
		return nil
	})
	if aerr != nil {
		reloadFailed("registry apply failed; keeping previous config", aerr)
		return
	}
	if !adopted {
		return // stale event: something newer is already applied
	}
	g.log.Info("registry change applied", "kind", string(kind), logx.Rev(snap.Generation))

	if kind == registry.DocServers {
		g.syncServers(snap)
	}
	if kind == registry.DocGovernance {
		g.syncAudit()
		// Call quotas are a governance field too (ratelimit.go). Rebuilding
		// keeps a tightened limit from waiting for the next gateway restart;
		// a rule set that no longer parses keeps the previous one (the
		// runtime half of the failure direction documented there).
		g.syncRateLimits()
		// The skills face is governance-gated (docs/subsystems/skills.md). A flip of the
		// switch changes the CATALOG, so it must rebuild — a scope refresh
		// alone would leave the face listed (or missing) until the next
		// downstream event.
		if g.syncSkills(g.resolver) {
			g.rebuildAndNotify()
		}
	}
	// Every document kind is a scope input (servers/profiles/clients/
	// governance all feed the three-layer merge): recompute, push on hash
	// change only.
	if g.scopeRes != nil {
		g.scopeRes.Invalidate(scope.Event{Kind: scope.EvRegistryChanged})
		g.refreshScopeAndNotify()
	}
}

// syncServers diffs the enabled spec set of snap against the applied one
// and applies the minimal connection-plane delta: close removed servers,
// (re)connect added/changed ones, keep everything else connected.
func (g *gateway) syncServers(snap *registry.Snapshot) {
	newSpecs := g.specsFromSnapshot(snap)
	newByID := make(map[string]downstream.Spec, len(newSpecs))
	for _, sp := range newSpecs {
		newByID[sp.ID] = sp
	}

	// closing pairs each server with WHY it is being taken down. The reason
	// lives here because this is the only layer that has it: downstream.Close
	// reports that a connection ended, and cannot say whether the operator
	// deleted the entry or edited it — which are the two different things an
	// operator wants confirmed after a config change.
	type closing struct {
		srv    *downstream.Server
		reason string
	}
	var toClose []closing
	var toConnect []downstream.Spec
	removedAny := false

	g.mu.Lock()
	oldByID := make(map[string]downstream.Spec, len(g.specs))
	for _, sp := range g.specs {
		oldByID[sp.ID] = sp
	}
	for id, ns := range newByID {
		os, existed := oldByID[id]
		switch {
		case !existed:
			toConnect = append(toConnect, ns)
		case !specEqual(os, ns):
			// Definition changed: only THIS server reconnects.
			if srv := g.servers[id]; srv != nil {
				toClose = append(toClose, closing{srv, "definition changed"})
				delete(g.servers, id)
			}
			toConnect = append(toConnect, ns)
		}
	}
	for id := range oldByID {
		if _, ok := newByID[id]; !ok {
			removedAny = true
			if srv := g.servers[id]; srv != nil {
				toClose = append(toClose, closing{srv, "removed from the configuration"})
				delete(g.servers, id)
			}
		}
	}
	g.specs = newSpecs
	// A server about to be redialled starts from a clean slate: keeping the
	// previous failure would report the OLD definition's error against the
	// NEW one, and keeping its backoff would make the operator's fix wait out
	// a rung the PREVIOUS definition earned. Servers no longer in the spec
	// set leave the maps entirely.
	dialNow := toConnect[:0]
	for _, sp := range toConnect {
		delete(g.connErr, sp.ID)
		g.resetLadderLocked(sp.ID)
		if g.beginDialLocked(sp.ID) { // also accounts it as pending
			dialNow = append(dialNow, sp)
			continue
		}
		// A dial of the PREVIOUS definition is still in flight. It drops
		// itself as stale when it lands (connectOne), which would leave the
		// new definition dialed by nobody — so hand it to the ladder instead,
		// due at the next tick.
		g.redialAt[sp.ID] = time.Time{}
	}
	toConnect = dialNow
	for id := range g.connErr {
		if _, ok := newByID[id]; !ok {
			delete(g.connErr, id)
			g.resetLadderLocked(id)
		}
	}
	g.mu.Unlock()

	for _, c := range toClose {
		// Announced BEFORE the close, which is what makes the pair worth two
		// lines: Close blocks on the owner goroutine, so this line without
		// downstream's "downstream connection closed" after it is a teardown
		// that did not finish — a state that otherwise looks exactly like a
		// server quietly missing from the catalog.
		g.log.Info("closing a downstream connection",
			logx.Server(c.srv.ID()), "reason", c.reason)
		c.srv.Close() // outside g.mu: Close waits for the owner goroutine
		// Derived instances of a redefined or removed server are stale by the
		// same argument their base connection is: they were dialed from the
		// old spec. Closing them here means the next call re-derives from the
		// applied one (docs/modules/dataplane.md lifecycle).
		if g.pool != nil {
			g.pool.CloseServer(c.srv.ID())
		}
	}
	for _, sp := range toConnect {
		go g.connectOne(sp)
	}
	if removedAny || len(toClose) > 0 {
		// Removed/replaced servers must leave the catalog now; additions
		// rebuild when their connect completes (connectOne).
		g.rebuildAndNotify()
	}
	// Report the new spec set immediately, before any of the dials finish:
	// a server that was just added shows as "connecting" rather than
	// disappearing from /v1/servers until its handshake returns.
	g.reportServers()
}

// specsFromSnapshot extracts the enabled downstream specs from a registry
// snapshot, whatever their transport (shared by startup load and hot reload).
func (g *gateway) specsFromSnapshot(snap *registry.Snapshot) []downstream.Spec {
	var specs []downstream.Spec
	for id, doc := range snap.Servers.V.Servers {
		entry := doc.V
		if !entry.Enabled {
			continue
		}
		// SpecFromEntry is the single registry→spec translation for the
		// whole binary. Hand-building a Spec here used to look harmless and
		// was not: fields that landed in the translator (the container
		// runtime, http endpoints and headers, provenance) were silently
		// dropped on the ONE path that actually serves clients, so a server
		// configured as contained ran on the host with nothing saying so.
		//
		// A rejected entry disables that entry and nothing else — a typo in
		// `derive` or a runtime this build cannot honor must never collapse
		// into a weaker connection.
		spec, err := downstream.SpecFromEntry(id, entry)
		if err != nil {
			g.log.Warn("skipping unusable server entry", logx.Server(id), "error", err)
			continue
		}
		specs = append(specs, spec)
	}
	return specs
}

// specByIDLocked finds the applied spec for id. Callers hold g.mu.
func (g *gateway) specByIDLocked(id string) (downstream.Spec, bool) {
	for _, sp := range g.specs {
		if sp.ID == id {
			return sp, true
		}
	}
	return downstream.Spec{}, false
}

// specEqual compares the connection-relevant fields of two specs; equality
// means the existing downstream connection may be kept across a reload.
// Every connection-relevant field must appear here. A field that is
// compared nowhere is a field whose edit leaves the old connection running
// under the old definition — for the container block that would mean an
// operator changing the image, the mounts or the network and getting the
// previous isolation until the next restart.
func specEqual(a, b downstream.Spec) bool {
	if a.ID != b.ID || a.Kind != b.Kind || a.Command != b.Command ||
		a.Cwd != b.Cwd || a.URL != b.URL || a.Derive != b.Derive ||
		a.Provenance != b.Provenance {
		return false
	}
	if !slices.Equal(a.Args, b.Args) {
		return false
	}
	if !maps.Equal(a.Env, b.Env) || !maps.Equal(a.Headers, b.Headers) {
		return false
	}
	return dockerEqual(a.Docker, b.Docker)
}

// dockerEqual compares two container configurations, treating nil (host)
// and non-nil (contained) as different by construction.
func dockerEqual(a, b *transport.DockerConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Image != b.Image || a.Network != b.Network || a.Memory != b.Memory ||
		a.CPUs != b.CPUs || a.User != b.User || a.Workdir != b.Workdir {
		return false
	}
	return slices.Equal(a.Mounts, b.Mounts) && slices.Equal(a.ExtraRunArgs, b.ExtraRunArgs)
}
