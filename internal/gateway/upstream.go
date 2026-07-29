package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/integrity"
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
	switch req.Method {
	case mcp.MethodInitialize:
		g.handleInitialize(req)
	case mcp.MethodPing:
		g.reply(mcp.NewResponse(req.ID, json.RawMessage(`{}`)))
	case mcp.MethodToolsList:
		g.handleToolsList(req)
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
	version := mcp.ProtocolVersion
	if slices.Contains(mcp.SupportedVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	g.mu.Lock()
	g.clientCaps = p.Capabilities
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
	g.log.Info("initialized upstream session",
		"protocol_version", version, "client", p.ClientInfo.Name)
	g.reply(mcp.NewResponse(req.ID, raw))
}

func (g *gateway) serverVersion() string {
	if g.cfg.Version != "" {
		return g.cfg.Version
	}
	return "0.0.0-dev"
}

// handleToolsList answers from the current exposure surface: the current
// catalog — the live router when ready, the cache-built one otherwise —
// projected through the session's effective scope and rendered in the
// session's discovery mode (full / grouped / lazy, docs/flows.md).
//
// The router is never rebuilt for a scope or mode change: visibility is a
// query-time projection (docs/architecture.md §7 invariant 2), and the mode only
// decides how many of the visible names are printed.
func (g *gateway) handleToolsList(req *mcp.Request) {
	raw, err := json.Marshal(mcp.ListToolsResult{Tools: g.currentSurface().List()})
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

	var p mcp.CallToolParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		g.reply(mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code: mcp.CodeInvalidParams, Message: "invalid tools/call params: " + err.Error(),
		}))
		return
	}

	s := g.currentSurface()
	switch s.Classify(p.Name) {
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
			inputSchema: def.InputSchema,
			annotations: def.Annotations,
			description: def.Description,
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
		g.replyUnknownTool(req.ID, exposed)
		return
	}
	// Derived instances (docs/modules/dataplane.md): which PROCESS runs this call is a
	// connection-plane decision made per call, after routing and before the
	// gates. The route — and therefore visibility, scope and audit — is the
	// base server either way.
	lease, err := g.acquire(ctx, srv, route.ServerID)
	if err != nil {
		g.log.Warn("derived instance unavailable",
			logx.Server(route.ServerID), logx.Tool(route.RawTool), "error", err)
		g.reply(mcp.NewErrorResponse(req.ID, callError(err)))
		return
	}
	defer lease.Release()
	target := lease.Server

	// The routed tool's definition feeds the precheck (inputSchema) and the
	// HITL destructive predicate (annotations; absent = destructive,
	// fail-closed — see pipeline.DefaultDestructive). It is read from the
	// BASE server: a derived instance serves the same catalog by
	// construction, and the base list is the one the router aggregated.
	var inputSchema, annotations json.RawMessage
	var description string
	for _, def := range srv.Tools() {
		if def.Name == route.RawTool {
			inputSchema, annotations, description = def.InputSchema, def.Annotations, def.Description
			break
		}
	}

	g.runCall(ctx, req, callTarget{
		exposed:     exposed,
		route:       route,
		inputSchema: inputSchema,
		annotations: annotations,
		description: description,
		call: func(ctx context.Context) (*mcp.CallResult, error) {
			return target.Call(ctx, route.RawTool, args)
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
	inputSchema json.RawMessage
	annotations json.RawMessage
	description string
	call        pipeline.CallFunc
}

// runCall feeds one resolved target through the pipeline and delivers the
// outcome upstream.
func (g *gateway) runCall(ctx context.Context, req *mcp.Request, t callTarget, args json.RawMessage) {
	route := t.route
	// Approval metadata rides the context (the pipeline passes ctx through
	// to the HITL asker): raw args for frontend display over the socket,
	// and the live definition for the allowlist fingerprint (asker.go).
	ctx = withCallMeta(ctx, callMeta{
		args: args,
		snap: integrity.ToolSnapshot{
			Name:        route.RawTool,
			Description: t.description,
			InputSchema: t.inputSchema,
		},
	})

	callReq := pipeline.CallRequest{
		Exposed:     t.exposed,
		ServerID:    route.ServerID,
		RawTool:     route.RawTool,
		Args:        args,
		InputSchema: t.inputSchema,
		Annotations: t.annotations,
		// The credential's tier rides EVERY execute path of this gateway
		// because there is only one: direct tools/call and lazy call_tool
		// both reach pipeline.Execute through here.
		CallerTier: g.cfg.CallerTier,
		Call:       t.call,
	}
	// Quota admission (internal/ratelimit, ratelimit.go): Guard WRAPS the
	// call closures, so a token is spent after EVERY gate — HITL included —
	// and immediately before the downstream call. It is not a fifth gate:
	// the frozen chain order is untouched, and a call any gate denied never
	// spends a token. A nil limiter (no rules configured) leaves the request
	// exactly as it was.
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

	res, err := g.pipe.Execute(ctx, callReq)
	if ctx.Err() != nil {
		// Cancelled: the downstream cancellation was already forwarded by
		// the transport; the upstream client gets no response by contract.
		g.log.Info("tools/call cancelled",
			logx.Server(route.ServerID), logx.Tool(route.RawTool), "id", req.ID.String())
		return
	}
	if err != nil {
		g.reply(mcp.NewErrorResponse(req.ID, callError(err)))
		return
	}
	raw, merr := json.Marshal(res)
	if merr != nil {
		g.reply(mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeInternalError, Message: merr.Error()}))
		return
	}
	g.reply(mcp.NewResponse(req.ID, raw))
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

// callClient performs one gateway→client reverse RPC (roots/list) over the
// upstream channel. Replies are matched by id in the read loop.
func (g *gateway) callClient(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
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
