package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/ratelimit"
	"github.com/dinstein/agent-hub/internal/router"
)

// Gateway-local termination methods. These are not MCP methods (the mcp
// facade deliberately does not know them): they are the LSP-style
// shutdown/exit convention some clients use, accepted here as an upstream
// courtesy alongside plain EOF.
const (
	methodShutdown = "shutdown"
	notifExit      = "exit"
)

// handleRequest dispatches one upstream request. Fast methods are answered
// inline on the read loop; tools/call gets a handler goroutine so a slow
// downstream never blocks the protocol channel (and stays cancellable via
// notifications/cancelled).
func (g *gateway) handleRequest(req *mcp.Request) {
	// tools/call performs protocol-meta acceptance inside its handler, after
	// the strict received record. Every other method keeps the fast inline
	// path. This is what records even a tool access attempt whose declared
	// protocol is rejected.
	if req.Method != mcp.MethodToolsCall && !g.acceptRequestMeta(req) {
		return // rejected with CodeUnsupportedProtocolVersion, reply sent
	}
	// Everything a client asks of agenthub is recorded, not only what it
	// routes, and all of it on ONE path: same span, same payload capture,
	// same finish through reply(). Without it the ledger could not answer the
	// first question anybody brings to it — did this client reach us at all —
	// because a session that initialized, listed and then went quiet left
	// exactly the trace of one that never connected.
	//
	// The record is written HERE, on the read loop, before anything parses or
	// dispatches: earlier than the handler goroutine tools/call runs in, and
	// therefore earlier than any decision made about the request. It cannot
	// change that decision — ledgerBegin returns nothing, and a ledger that
	// could not take the record has cost the timeline a line and nothing else.
	g.ledgerBegin(req)
	switch req.Method {
	case mcp.MethodInitialize:
		g.handleInitialize(req)
	case mcp.MethodDiscover:
		g.handleDiscover(req)
	case mcp.MethodPing:
		g.reply(mcp.NewResponse(req.ID, json.RawMessage(`{}`)))
	case mcp.MethodToolsList:
		g.handleToolsList(req)
	case mcp.MethodSubscriptionsListen:
		g.handleSubscriptionsListen(req)
	case mcp.MethodToolsCall:
		ctx, cancel := context.WithCancel(g.lifeCtx)
		g.registerInflight(req.ID, cancel)
		g.handlers.Add(1)
		go g.handleToolsCall(ctx, req)
	case methodShutdown:
		// Acknowledge; the client follows up with the exit notification or
		// closes the stream.
		g.reply(mcp.NewResponse(req.ID, nil))
	default:
		g.reply(mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code:    mcp.CodeMethodNotFound,
			Message: fmt.Sprintf("method %q not supported by the agenthub gateway", req.Method),
		}))
	}
}

// handleNotification processes one upstream notification. Unknown
// notifications are ignored by design. The returned flag requests loop
// exit.
func (g *gateway) handleNotification(n *mcp.Notification) (exit bool) {
	switch n.Method {
	case mcp.NotificationInitialized:
		g.mu.Lock()
		g.initialized = true
		ready := g.ready
		g.mu.Unlock()
		// The live catalog may have been built before the handshake
		// completed; deliver the deferred change signal now.
		if ready {
			g.notifyToolsChanged()
		}
		// Warm the roots cache off the read loop so downstream roots/list
		// reverse RPCs are usually answered from cache.
		//
		// The refresh afterwards is what makes per-project scope arrive: every
		// scope resolved before this point saw an empty root and therefore
		// consulted no project binding. refreshScopeAndNotify only pushes
		// tools/list_changed when the CONTENT hash actually moved, so a client
		// with no roots — or one whose root matches no binding — sees nothing.
		// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
		go func() {
			g.roots.prefetch(g.lifeCtx)
			g.refreshScopeAndNotify()
		}()
	case mcp.NotificationCancelled:
		var p mcp.CancelledParams
		if err := json.Unmarshal(n.Params, &p); err != nil || !p.RequestID.IsSet() {
			g.log.Warn("ignoring malformed notifications/cancelled", "error", err)
			return false
		}
		g.cancelInflight(p.RequestID, p.Reason)
	case mcp.NotificationRootsListChanged:
		// The root selects the per-project scope layer, so a root change is a
		// scope change. Re-warm off the read loop (the refetch is a reverse
		// RPC to this same client) and then refresh: until the new roots land,
		// cachedPrimaryRoot reports "" and only the client-level binding
		// applies — the widest outcome, and the one that predates this wiring.
		// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
		g.roots.invalidate()
		go func() {
			g.roots.prefetch(g.lifeCtx)
			g.refreshScopeAndNotify()
		}()
	case notifExit:
		return true
	default:
		g.log.Debug("ignoring unknown upstream notification", "method", n.Method)
	}
	return false
}

// handleInitialize answers the handshake immediately — before any
// downstream is connected (docs/flows.md: "answer first with whatever can be answered").
func (g *gateway) handleInitialize(req *mcp.Request) {
	var p mcp.InitializeParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		g.log.Warn("initialize params unparsable; proceeding with defaults", "error", err)
	}
	// Version negotiation: echo the client's version when we support it,
	// otherwise answer with our own default and let the client decide.
	//
	// initialize can only ever negotiate the STATEFUL protocol family:
	// 2026-07-28's handshake is server/discover, so a client declaring it
	// HERE is answered with the default instead — echoing 2026 would
	// promise per-request _meta semantics on a session that just used the
	// handshake 2026 removed.
	version := mcp.ProtocolVersion
	if p.ProtocolVersion != mcp.Version2026 && slices.Contains(mcp.SupportedVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	g.mu.Lock()
	g.clientCaps = p.Capabilities
	g.protocol = version
	g.mu.Unlock()

	res := mcp.InitializeResult{
		ProtocolVersion: version,
		Capabilities:    json.RawMessage(`{"tools":{"listChanged":true}}`),
		ServerInfo:      mcp.Implementation{Name: serverName, Version: g.serverVersion()},
	}
	raw, err := json.Marshal(res)
	if err != nil {
		g.reply(mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeInternalError, Message: err.Error()}))
		return
	}
	// One call, both streams (eventlog.Emit). The event answers a
	// question the `started` record cannot: a gateway process starts when the
	// client launches it, and attaches only once the client actually speaks
	// MCP. A configuration that starts and never attaches is the signature of
	// a client that was pointed at the hub and never restarted, and without
	// this record the two look identical from outside.
	//
	// The peer's SELF-REPORTED name goes in Detail, never in Client: that
	// field carries the CONFIGURED id, which is the join key, and letting a
	// peer's own string reach it would be the collision the log line above
	// had to be fixed for. From/To stay empty — this is an arrival, not a
	// transition, and the protocol version is elaboration rather than a state
	// the session moved between.
	//
	// client_name, not client: logx.FieldClient is already bound on this
	// logger and holds the CONFIGURED client id. slog's JSON handler does not
	// deduplicate keys, so writing the peer's self-reported name under the
	// same key emitted the field twice on one line — and a reader that takes
	// the last wins (most do) silently read the mandatory join key as
	// "Claude Code" instead of "claude-code".
	g.eventStream().Emit(g.log, eventlog.Record{
		Scope: eventlog.ScopeGateway, Kind: eventlog.KindClientAttached,
		Client: g.cfg.ClientID,
		Detail: p.ClientInfo.Name + " speaking " + version,
	}, "initialized upstream session",
		"protocol_version", version, "client_name", p.ClientInfo.Name)
	g.reply(mcp.NewResponse(req.ID, raw))
}

func (g *gateway) serverVersion() string {
	if g.cfg.Version != "" {
		return g.cfg.Version
	}
	return "0.0.0-dev"
}

// handleDiscover answers the 2026-07-28 stateless handshake probe. Like
// initialize it answers immediately, before any downstream is connected.
// The version list is everything this facade speaks: a client that picks
// ≤ 2025-11-25 from it follows up with the stateful initialize handshake,
// exactly as agenthub's own downstream client does.
func (g *gateway) handleDiscover(req *mcp.Request) {
	res := mcp.DiscoverResult{
		ResultType:        mcp.ResultTypeComplete,
		SupportedVersions: mcp.SupportedVersions,
		Capabilities:      json.RawMessage(`{"tools":{"listChanged":true}}`),
		Meta: &mcp.ResultMeta{
			ServerInfo: &mcp.Implementation{Name: serverName, Version: g.serverVersion()},
		},
	}
	// DiscoverResult is a CacheableResult: ttlMs and cacheScope are required
	// members of the shape, not optional hints. cacheScope is private for the
	// same reason tools/list's is — what this gateway answers is decided by
	// the calling client's profile, so no cache may be shared across
	// authorization contexts.
	ttl := listTTLMs
	res.CacheableResult = mcp.CacheableResult{TtlMs: &ttl, CacheScope: "private"}
	raw, err := json.Marshal(res)
	if err != nil {
		g.reply(mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeInternalError, Message: err.Error()}))
		return
	}
	g.log.Info("answered server/discover", "versions", mcp.SupportedVersions)
	g.reply(mcp.NewResponse(req.ID, raw))
}

// acceptRequestMeta inspects the per-request _meta a 2026-07-28 client
// carries (stateless protocol). Absence is fine — a stateful session has
// no _meta on its requests. Presence switches the session into stateless
// mode: the declared capabilities replace the initialize-time slot, and the
// session counts as initialized, because there will never be a
// notifications/initialized. A declared version this gateway cannot serve
// statelessly is rejected with CodeUnsupportedProtocolVersion rather than
// answered — answering would promise semantics the session does not have
// (fail closed).
//
// Reports whether dispatch should proceed; false means the rejection was
// already sent.
func (g *gateway) acceptRequestMeta(req *mcp.Request) bool {
	if len(req.Params) == 0 {
		return true
	}
	var probe struct {
		Meta *mcp.RequestMeta `json:"_meta"`
	}
	if err := json.Unmarshal(req.Params, &probe); err != nil || probe.Meta == nil ||
		probe.Meta.ProtocolVersion == "" {
		return true // no protocol _meta; params may not even be an object
	}
	if probe.Meta.ProtocolVersion != mcp.Version2026 {
		// The payload is what makes the refusal actionable: the client is
		// told to retry with a version from the list, so the list has to be
		// in the answer. Only Version2026 is offered here, not
		// SupportedVersions — this is the stateless face, and the older
		// members of that list are reachable through initialize instead.
		g.reply(mcp.NewErrorResponse(req.ID, mcp.NewUnsupportedVersionError(
			probe.Meta.ProtocolVersion, []string{mcp.Version2026},
			fmt.Sprintf(
				"per-request _meta declares protocol %q; this gateway serves %q statelessly — earlier versions use the initialize handshake",
				probe.Meta.ProtocolVersion, mcp.Version2026))))
		return false
	}
	g.mu.Lock()
	first := !g.stateless
	g.stateless = true
	g.protocol = mcp.Version2026
	g.clientCaps = probe.Meta.ClientCapabilities
	wasInitialized := g.initialized
	g.initialized = true
	ready := g.ready
	g.mu.Unlock()
	if first {
		g.log.Info("stateless upstream session", "client_name", clientName(probe.Meta.ClientInfo))
	}
	if first && !wasInitialized {
		// The first stateless request plays the role notifications/initialized
		// plays on the stateful path: deferred change signals may flow now.
		//
		// The live catalog may have arrived while the session was still
		// uninitialized, in which case swapCatalog DROPPED its notification
		// and nothing else will resend it: swapCatalog re-baselines lastScope
		// on the way past, so the refresh below sees no content change and
		// stays silent. Deliver it here, exactly as the stateful path does on
		// notifications/initialized.
		if ready {
			g.notifyToolsChanged()
		}
		// No roots prefetch — 2026-07-28 removed server-initiated RPCs, so
		// the client can never be asked (see clientRoots.fetchFromClient).
		go g.refreshScopeAndNotify()
	}
	return true
}

func clientName(info *mcp.Implementation) string {
	if info == nil {
		return ""
	}
	return info.Name
}

// statelessSession reports whether this session negotiated 2026-07-28.
func (g *gateway) statelessSession() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stateless
}

// listTTLMs is the freshness hint stamped on 2026-07-28 tools/list answers.
// Deliberately short: the surface changes with profile bindings and
// downstream list_changed at any moment, and the listChanged notification —
// not the TTL — is the real invalidation signal.
const listTTLMs int64 = 60_000

// handleToolsList answers from the current exposure surface: the current
// catalog — the live router when ready, the cache-built one otherwise —
// projected through the session's effective scope and rendered in the
// session's discovery mode (full / grouped / lazy, docs/flows.md).
//
// The router is never rebuilt for a scope or mode change: visibility is a
// query-time projection (docs/architecture.md §7 invariant 2), and the mode only
// decides how many of the visible names are printed.
func (g *gateway) handleToolsList(req *mcp.Request) {
	res := mcp.ListToolsResult{Tools: g.currentSurface().List()}
	if g.statelessSession() {
		// 2026-07-28 requires resultType and the freshness hints on every
		// list result. cacheScope is private: the surface is a per-session
		// scope projection, never shareable across callers.
		ttl := listTTLMs
		res.ResultType = mcp.ResultTypeComplete
		res.CacheableResult = mcp.CacheableResult{TtlMs: &ttl, CacheScope: "private"}
	}
	raw, err := json.Marshal(res)
	if err != nil {
		g.reply(mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeInternalError, Message: err.Error()}))
		return
	}
	g.reply(mcp.NewResponse(req.ID, raw))
}

// handleToolsCall runs on its own goroutine and classifies the incoming name
// against the current surface (docs/flows.md):
//
//   - meta   → the discovery handlers; call_tool re-enters execTool, so the
//     gate chain never forks.
//   - group  → the grouped-mode aggregate listing of one server.
//   - tool   → the direct execute path.
//   - unknown → DROPPED, fail-closed. It is never reinterpreted as a
//     meta-tool, not even when it looks like one (a bare name under a cold
//     catalog is exactly that case).
func (g *gateway) handleToolsCall(ctx context.Context, req *mcp.Request) {
	defer g.handlers.Done()
	defer g.unregisterInflight(req.ID)
	defer func() {
		if ctx.Err() != nil {
			g.ledgerFinishCancelled(req.ID)
		}
	}()

	// The received record was written by handleRequest, on the read loop,
	// before this goroutine started — which is stricter than writing it here
	// and is why the ordering rule holds for every method rather than for
	// this one: the complete incoming params are durable before anything
	// parses, routes or gates them, and a write failure refused the call
	// there. Discovery remains available for repair either way.
	if !g.acceptRequestMeta(req) {
		return // rejection response was sent and finalized by reply
	}

	var p mcp.CallToolParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		g.reply(mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code: mcp.CodeInvalidParams, Message: "invalid tools/call params: " + err.Error(),
		}))
		return
	}
	g.ledgerSetExposed(req.ID, p.Name)

	s := g.currentSurface()
	kind := s.Classify(p.Name)
	g.ledgerSetSurface(req.ID, kind.String())
	switch kind {
	case discovery.KindMeta:
		g.handleMetaCall(ctx, req, s, p)
	case discovery.KindGroup:
		g.guard.ObserveOther()
		g.replyResult(req.ID, s.HandleGroup(p.Name))
	case discovery.KindTool:
		g.guard.ObserveOther()
		g.execTool(ctx, req, p.Name, p.Arguments)
	default:
		if rt, _, _ := g.catalog(); routable(rt, p.Name) {
			// Routable but not on the surface: the name exists in the catalog
			// and the SCOPE is what hides it. The scope gate is the
			// enforcement point (docs/architecture.md §9) and owns that rejection with
			// its stable code, so the call still enters the pipeline — where
			// it is denied, never executed.
			g.execTool(ctx, req, p.Name, p.Arguments)
			return
		}
		// Fail-closed drop: the name resolves to nothing at all. It is never
		// reinterpreted — in lazy mode a bare unknown name looks exactly like
		// a meta-tool, and guessing would invent a capability the session
		// does not have.
		g.log.Warn("dropping unroutable tools/call name",
			logx.Tool(p.Name), "mode", string(s.Mode()), "bare", discovery.IsBareName(p.Name))
		g.replyUnroutable(req.ID, p.Name)
	}
}

// routable reports whether the exposed name has a route in the current
// catalog. It exists so the drop decision is a map lookup through RouteOf,
// never a parse of the exposed name (router doc: splitting on "__" is
// forbidden repo-wide).
func routable(rt *router.Router, name string) bool {
	_, ok := rt.RouteOf(name)
	return ok
}

// execTool is the SINGLE execute path of the gateway: route the exposed name,
// feed pipeline.Execute, deliver the result. Both the direct tools/call and
// the lazy call_tool meta-tool land here — that identity is what keeps the
// governance chain unforkable (canonical.md §2: one execute pipeline).
//
// If the request was cancelled (per-request cancel or gateway shutdown) no
// response is sent — MCP receivers of a cancellation must not expect one.
func (g *gateway) execTool(ctx context.Context, req *mcp.Request, exposed string, args json.RawMessage) {
	rt, ready, pending := g.catalog()

	// Host-served providers (the skills face, docs/modules/config.md) resolve BEFORE
	// the readiness gate: they have no downstream to wait for, so making
	// them busy while unrelated servers connect would be a lie. Everything
	// after this point — gates, shaping, audit — is the same code path.
	if prov, route, ok := rt.LookupProvider(exposed); ok {
		def, _ := rt.Def(exposed)
		g.runCall(ctx, req, callTarget{
			exposed:     exposed,
			route:       route,
			annotations: def.Annotations,
			provider:    "host",
			call: func(ctx context.Context) (*mcp.CallResult, error) {
				return prov.Call(ctx, route.RawTool, args)
			},
		}, args)
		return
	}

	if !ready {
		g.replyBusy(req.ID)
		return
	}
	srv, route, ok := rt.Lookup(exposed)
	if !ok || srv == nil {
		if pending > 0 {
			// The name may belong to a server still connecting.
			g.replyBusy(req.ID)
			return
		}
		// Listable but not callable: the catalog entry came from the tool
		// cache and its server never connected. The client is answered the
		// same "unknown tool" every other miss gets — an anti-probing rule,
		// docs/flows.md — so the log is the only place the difference between
		// "no such tool" and "that server is down" can be recorded, and it is
		// the difference between an agent's bug and an operator's.
		g.log.Warn("tools/call routed to a tool no connected server serves",
			logx.Tool(exposed), "cached_entry", ok)
		g.replyUnknownTool(req.ID, exposed)
		return
	}
	// The routed tool's definition feeds the token tier gate (annotations;
	// absent = destructive, fail-closed — see pipeline.ToolTier). rt.Def
	// reads the router's own snapshot rather than re-scanning the server's
	// live tool table (router.Def's doc: that is exactly why it exists), so
	// a list_changed refresh that lands between routing and this read cannot
	// hand the gate a definition inconsistent with the snapshot the call was
	// routed and scope-checked against.
	def, _ := rt.Def(exposed)

	g.runCall(ctx, req, callTarget{
		exposed:     exposed,
		route:       route,
		annotations: def.Annotations,
		// Derived instances (docs/modules/dataplane.md): which PROCESS runs
		// this call is a connection-plane decision made per call. It is made
		// INSIDE the call closure, so it happens after both gates and after
		// rate-limit admission — acquiring can spawn a child or open an
		// authenticated remote connection, and a call the scope gate is about
		// to deny must not cause either. The route — and therefore
		// visibility, scope and audit — is the base server either way.
		call: func(ctx context.Context) (*mcp.CallResult, error) {
			lease, err := g.acquire(ctx, srv, route.ServerID)
			if err != nil {
				g.log.Warn("derived instance unavailable",
					logx.Server(route.ServerID), logx.Tool(route.RawTool), "error", err)
				return nil, err
			}
			defer lease.Release()
			// The ledger's call id travels with the call, so every frame it
			// produces — including the ones a retry or a respawn adds — joins
			// back to the lifecycle records of this same request. The closure
			// already holds the span, which is why this is an argument rather
			// than something hidden in the context.
			return lease.Server.CallFor(ctx, downstream.CallOrigin(g.ledgerCallID(req.ID)),
				route.RawTool, args)
		},
	}, args)
}

// callTarget is one resolved execution target: the route (always from
// RouteOf), the definition fields the gates read, and the closure that
// performs the call. It exists so the downstream path and the host-served
// provider path share ONE pipeline.Execute call site — a second one would
// be a second governance surface (canonical.md §2).
type callTarget struct {
	exposed     string
	route       router.Route
	annotations json.RawMessage
	provider    string
	call        pipeline.CallFunc
}

// runCall feeds one resolved target through the pipeline and delivers the
// outcome upstream.
func (g *gateway) runCall(ctx context.Context, req *mcp.Request, t callTarget, args json.RawMessage) {
	route := t.route
	// The routed event (including the complete effective arguments) is written
	// before the frozen gate chain runs. This observes provenance; it is not a
	// gate, never inspects or rewrites args, and cannot stop the call — a
	// record the ledger could not take is a line missing from the history.
	g.ledgerRoute(req.ID, route, args, t.provider)
	callReq := pipeline.CallRequest{
		Exposed:     t.exposed,
		ServerID:    route.ServerID,
		RawTool:     route.RawTool,
		Args:        args,
		Annotations: t.annotations,
		// The credential's tier rides EVERY execute path of this gateway
		// because there is only one: direct tools/call and lazy call_tool
		// both reach pipeline.Execute through here.
		CallerTier: g.cfg.CallerTier,
		Call:       t.call,
	}
	// Quota admission (internal/ratelimit, ratelimit.go): Guard WRAPS the
	// call closures, so a token is spent after BOTH gates and immediately
	// before the downstream call. It is not a third gate: the frozen chain
	// order is untouched, and a call either gate denied never spends a
	// token. A nil limiter (no rules configured) leaves the request exactly
	// as it was.
	//
	// The key uses the ROUTED (server, tool) — RouteOf provenance, never the
	// exposed name: renaming a tool must not move which quota it spends
	// from. Both the direct tools/call and the lazy call_tool meta-tool land
	// here, so there is one enforcement point, not two.
	if lim := g.limiter.Load(); lim != nil {
		lim.Guard(ratelimit.Key{
			Client: g.cfg.ClientID,
			Server: route.ServerID,
			Tool:   route.RawTool,
		}, &callReq)
	}

	started := time.Now()
	res, err := g.pipe.Execute(ctx, callReq)
	if ctx.Err() != nil {
		// Cancelled: the downstream cancellation was already forwarded by
		// the transport; the upstream client gets no response by contract.
		g.log.Info("tools/call cancelled", g.callFields(req, route, started)...)
		return
	}
	if err != nil {
		g.ledgerMarkFailure(req.ID, err)
		g.logCallFailure(req, route, started, err)
		g.reply(mcp.NewErrorResponse(req.ID, callError(err)))
		return
	}
	// INFO, and the level is the point. This is the one thing the hub exists
	// to do, and it was the only call outcome below the default: a failure is
	// warn, a denial is warn, a cancellation is info, and success was debug.
	// The ledger that would otherwise hold the record is disabled unless the
	// operator enables it (registry.CallsPolicy.Enabled zero-values to
	// false), and the event log records server lifecycle, not calls — so on
	// a default installation a call that WORKED left no trace in any of the
	// three streams, and "did the agent ever call this tool", "which tool is
	// slow" and "was this server used at all" had no answer to read.
	//
	// One line per call is affordable: callFields is bounded and carries no
	// arguments, and internal/jsonl rotates the file at 32 MiB.
	g.log.Info("tools/call served", g.callFields(req, route, started)...)
	// replyResult owns the marshal so resultType normalization (session
	// generation, not downstream generation) has exactly one enforcement
	// point for every result-shaped answer.
	g.replyResult(req.ID, res)
}

// callFields is the identity every call outcome is reported under: the
// ROUTED server and tool (RouteOf provenance, never the exposed name — a
// renamed tool must not become a different tool in the log), the upstream
// request id, and how long the whole pipeline took.
//
// The id is what ties the outcome to the client's own record of the call,
// and it is what tells two concurrent calls apart: every tools/call runs on
// its own goroutine, so without it the lines of an agent making six calls at
// once interleave into one unreadable sequence.
//
// Arguments are deliberately absent, here and everywhere: they are the one
// part of a call that carries the user's data, and a log that records them is
// a log that cannot be attached to a bug report.
func (g *gateway) callFields(req *mcp.Request, route router.Route, started time.Time) []any {
	return []any{
		logx.Server(route.ServerID), logx.Tool(route.RawTool),
		"id", req.ID.String(), "dur_ms", time.Since(started).Milliseconds(),
	}
}

// logCallFailure records a call that did not produce a result.
//
// Before this line the whole class was silent: a downstream error, a dead
// transport, an open circuit, exhausted retries and a gate rejection were all
// turned into a JSON-RPC error and answered upstream without a word, so the
// first question after "the tool failed" — which server, which tool, why —
// had no answer in the file at all, and the only way to get one was to turn
// on frame tracing and reproduce it.
//
// A gate rejection is reported apart from a failure because it is not one:
// nothing broke, the call was refused by configuration written before the
// client connected. It carries the gate and the stable rejection code, which
// is the same thing internal/ratelimit's rejection line does, and for the
// reason recorded there: a governance decision that never fires and a
// governance decision that is not running must not look alike from outside.
func (g *gateway) logCallFailure(req *mcp.Request, route router.Route, started time.Time, err error) {
	fields := g.callFields(req, route, started)
	var be *pipeline.BlockedError
	if errors.As(err, &be) {
		g.log.Warn("tools/call denied", append(fields, "gate", be.Gate, "code", be.Code)...)
		return
	}
	g.log.Warn("tools/call failed", append(fields, "error", err)...)
}

// callError maps a pipeline/downstream failure to the upstream JSON-RPC
// error: a downstream JSON-RPC error passes through with its code, a gate
// rejection keeps its stable code in the message, anything else is an
// internal error.
func callError(err error) *mcp.Error {
	var me *mcp.Error
	if errors.As(err, &me) {
		return me
	}
	var be *pipeline.BlockedError
	if errors.As(err, &be) {
		return &mcp.Error{Code: mcp.CodeInvalidRequest, Message: be.Error()}
	}
	return &mcp.Error{Code: mcp.CodeInternalError, Message: err.Error()}
}

// replyResult answers a tools/call with a computed CallResult (meta-tools,
// grouped listings). A nil result is answered as an internal error rather
// than as an empty success — a handler that produced nothing is a bug, and
// silently delivering "no content" would hide it.
func (g *gateway) replyResult(id mcp.ID, res *mcp.CallResult) {
	if res == nil {
		g.reply(mcp.NewErrorResponse(id, &mcp.Error{
			Code: mcp.CodeInternalError, Message: "agenthub produced no result",
		}))
		return
	}
	// Normalize resultType to the SESSION's protocol generation, not the
	// downstream's: a 2026 session must always see one, a stateful session
	// must never see the member a 2026 downstream happened to include
	// (docs/mcp-2026-07-28.md §7.5).
	if g.statelessSession() {
		if res.ResultType == "" {
			res.ResultType = mcp.ResultTypeComplete
		}
	} else {
		res.ResultType = ""
	}
	// content is a REQUIRED array on a CallToolResult, and this gateway must
	// not put its name on a result that has something else there. Two ways
	// it could: a downstream that omitted the member leaves Content nil,
	// which marshals as null, and a downstream that sent null explicitly
	// arrives as the four bytes `null`. Both become the empty array, which
	// is the valid rendering of "the call produced no content".
	//
	// This is not "editing what a downstream returned" — the rule that
	// forbids that is about preserving structure, and there is no structure
	// here to preserve. It is the same normalization resultType gets one
	// statement up, for the same reason: one enforcement point for the shape
	// of every result-shaped answer.
	if len(res.Content) == 0 || string(res.Content) == "null" {
		res.Content = json.RawMessage(`[]`)
	}
	// The downstream's _meta travels on, minus the specification's own key
	// namespace, which belongs to whichever hop is speaking. Only one
	// reserved key can legitimately reach here on a tools/call result —
	// io.modelcontextprotocol/serverInfo — and relaying it would name the
	// downstream as the server that produced a response this gateway
	// produced, which internal/shaping may have truncated or reformatted
	// besides. The third normalization at this one point, for the reason
	// the other two are here.
	res.Meta = mcp.StripReservedMetaKeys(res.Meta)
	raw, err := json.Marshal(res)
	if err != nil {
		g.reply(mcp.NewErrorResponse(id, &mcp.Error{Code: mcp.CodeInternalError, Message: err.Error()}))
		return
	}
	g.reply(mcp.NewResponse(id, raw))
}

// replyUnroutable answers a name that resolves to nothing. While downstreams
// are still connecting the answer is the RETRYABLE busy error, not "unknown
// tool": the name may belong to a server that has not finished its handshake,
// and telling an agent a tool does not exist teaches it to stop asking.
func (g *gateway) replyUnroutable(id mcp.ID, name string) {
	if _, ready, pending := g.catalog(); !ready || pending > 0 {
		g.replyBusy(id)
		return
	}
	g.replyUnknownTool(id, name)
}

// replyUnknownTool answers a name that resolves to nothing. The message does
// not distinguish "does not exist" from "not visible to you": the same
// anti-probing rule the meta-tool errors follow (docs/flows.md).
func (g *gateway) replyUnknownTool(id mcp.ID, name string) {
	g.reply(mcp.NewErrorResponse(id, &mcp.Error{
		Code: mcp.CodeInvalidParams, Message: fmt.Sprintf("unknown tool %q", name),
	}))
}

// replyBusy answers a retryable "still connecting" error (docs/flows.md:
// the catalog can be served from cache, but calls need a live connection).
func (g *gateway) replyBusy(id mcp.ID) {
	// Debug: an agent that asks early gets one of these per attempt, and a
	// retryable non-answer during startup is the system working. It is worth
	// recording at all because "the tool did nothing for the first ten
	// seconds" is otherwise unattributable.
	g.log.Debug("tools/call answered busy: downstreams still connecting", "id", id.String())
	g.reply(mcp.NewErrorResponse(id, &mcp.Error{
		Code:    codeRetryBusy,
		Message: "downstream servers are still connecting; retry shortly",
	}))
}

// registerInflight records the cancel function of an in-flight upstream
// request so notifications/cancelled can reach it.
func (g *gateway) registerInflight(id mcp.ID, cancel context.CancelFunc) {
	g.mu.Lock()
	g.inflight[id.Key()] = cancel
	g.mu.Unlock()
}

// unregisterInflight removes and fires the cancel (releasing the context;
// harmless when the call already completed).
func (g *gateway) unregisterInflight(id mcp.ID) {
	g.mu.Lock()
	cancel := g.inflight[id.Key()]
	delete(g.inflight, id.Key())
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// cancelInflight handles notifications/cancelled: cancel the named request
// if it is still in flight. Unknown ids are ignored (cancellation races are
// inherent).
func (g *gateway) cancelInflight(id mcp.ID, reason string) {
	g.mu.Lock()
	cancel := g.inflight[id.Key()]
	g.mu.Unlock()
	if cancel == nil {
		return
	}
	g.log.Info("cancelling in-flight request", "id", id.String(), "reason", reason)
	cancel()
}

// inflightLen reports the number of in-flight upstream requests (tests).
func (g *gateway) inflightLen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.inflight)
}

// callClient performs one gateway→client reverse RPC (roots/list today) over
// the upstream channel. Replies are matched by id in the read loop.
//
// A stateless (2026-07-28) session is refused HERE, not only at the call
// site. 2026-07-28 removed server-initiated requests, so such a frame is a
// wire error rather than a question, and this function takes the method as a
// string: the next reverse RPC someone adds would otherwise inherit the
// exemption silently. clientRoots.fetchFromClient checks the same flag first
// — that one is the semantic answer ("do not ask"), this one makes it
// unrepresentable.
func (g *gateway) callClient(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if g.statelessSession() {
		return nil, fmt.Errorf("gateway: refusing %s reverse RPC: the session is stateless (2026-07-28 has no server-initiated requests)", method)
	}
	id := mcp.NewStringID(fmt.Sprintf("agenthub-%d", g.nextReqID.Add(1)))
	ch := make(chan *mcp.Response, 1)
	g.mu.Lock()
	g.pendingRPC[id.Key()] = ch
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.pendingRPC, id.Key())
		g.mu.Unlock()
	}()

	if err := g.fw.WriteFrame(mcp.NewRequest(id, method, params)); err != nil {
		return nil, fmt.Errorf("gateway: reverse RPC write: %w", err)
	}
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-g.lifeCtx.Done():
		return nil, fmt.Errorf("gateway: shutting down")
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// deliverRPC routes an upstream response to the waiting reverse RPC.
// Unmatched responses are dropped (late replies after timeout).
func (g *gateway) deliverRPC(resp *mcp.Response) {
	g.mu.Lock()
	ch, ok := g.pendingRPC[resp.ID.Key()]
	if ok {
		delete(g.pendingRPC, resp.ID.Key())
	}
	g.mu.Unlock()
	if ok {
		ch <- resp // buffered(1): never blocks the read loop
	}
}
