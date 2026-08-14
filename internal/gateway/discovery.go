package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/shaping"
)

// This file is the gateway's DISCOVERY plane (docs/flows.md §2, docs/modules/dataplane.md): which
// names the upstream client is shown, what an incoming name means, and how
// a result is bounded before it is delivered.
//
// Three invariants hold here and are the reason the code is shaped this way:
//
//  1. ONE scope, three enforcement points. tools/list, search_tools and
//     call_tool all read the SAME *scope.EffectiveScope through the SAME
//     projection (discovery.Visible over the current router). This file
//     never re-derives visibility.
//
//  2. The gate chain cannot fork. call_tool{tool, arguments} resolves to a
//     route and then enters pipeline.Execute — the identical call the direct
//     tools/call path makes (execTool is the only implementation). A second
//     execute path would be a second governance surface.
//
//  3. Fail-closed naming. A name that resolves to nothing is DROPPED, never
//     promoted to a meta-tool. Under a cold catalog every name resolves to
//     nothing, which is the closed direction and is deliberate.

// budgetWildcard is the EffectiveScope.Budgets key that applies to every
// server; a per-server entry wins over it (docs/model.md).
const budgetWildcard = "*"

// currentSurface returns the exposure snapshot for the current (catalog,
// scope) pair, rebuilding it only when that pair moved.
//
// The cache key is discovery.Key{Generation, ScopeHash}: the catalog
// generation is bumped on every router swap and the scope hash covers every
// visibility-relevant field, so a stale surface can never be served — no
// explicit invalidation exists, or could go missing.
func (g *gateway) currentSurface() *discovery.Surface {
	es := g.currentScope() // resolve OUTSIDE g.mu (the resolver takes its own locks)

	g.mu.Lock()
	rt, gen, cached := g.rt, g.catGen, g.surface
	g.mu.Unlock()

	key := discovery.CacheKeyOf(gen, es)
	if cached != nil && cached.CacheKey() == key {
		return cached
	}
	s := discovery.New(discovery.Options{
		Mode:       discovery.ModeOf(es),
		Tools:      discovery.Visible(rt, es),
		Pins:       g.pins,
		Scope:      es,
		Generation: gen,
	})

	g.mu.Lock()
	// Two concurrent requests may build the surface for the same key; New is
	// a pure function of its inputs, so the values are equal and either may
	// be published. A build over a catalog that has since been swapped is
	// dropped instead (its key no longer describes the current state).
	if g.catGen == gen {
		g.surface = s
	}
	g.mu.Unlock()
	return s
}

// invalidateSurfaceLocked marks the cached surface stale by advancing the
// catalog generation. Callers hold g.mu and have just swapped g.rt.
func (g *gateway) invalidateSurfaceLocked() {
	g.catGen++
	g.surface = nil
}

// handleMetaCall dispatches one of the five lazy meta-tools. Only status,
// search_tools, describe_tool and fetch_result are answered here; call_tool
// resolves its target and hands it to execTool, the single execute path.
func (g *gateway) handleMetaCall(ctx context.Context, req *mcp.Request, s *discovery.Surface, p mcp.CallToolParams) {
	switch p.Name {
	case discovery.MetaStatus:
		g.guard.ObserveOther()
		g.replyResult(req.ID, s.HandleStatus(g.connDiagnosis(g.currentScope())))

	case discovery.MetaSearchTools:
		// The guard is advanced BY Search (it is the search observer); no
		// ObserveOther here, or every search would clear its own streak.
		res, sr := s.HandleSearch(p.Arguments, g.guard)
		g.logSearch(sr)
		g.replyResult(req.ID, res)

	case discovery.MetaDescribeTool:
		// describe_tool is a read of the surface, like status: it advances
		// the guard's "something other than a search happened" counter and
		// executes nothing.
		g.guard.ObserveOther()
		res, _ := s.HandleDescribe(p.Arguments)
		g.replyResult(req.ID, res)

	case discovery.MetaCallTool:
		g.guard.ObserveOther()
		t, args, err := s.ResolveCall(p.Arguments)
		if err != nil {
			g.replyResolveFailure(req, err)
			return
		}
		g.execTool(ctx, req, t.Exposed, args)

	case discovery.MetaFetchResult:
		g.guard.ObserveOther()
		g.handleFetchResult(ctx, req, p.Arguments)

	default:
		// Unreachable TODAY, and only because one switch is off. Classify
		// returns KindMeta for the names above — plus the three intent
		// variants, whenever the surface was built with them
		// (Surface.exposesMeta). This gateway never sets
		// discovery.Options.IntentVariants, so that cannot happen here.
		//
		// Whoever wires that switch must add the three cases with it. The
		// variants REPLACE call_tool rather than joining it (MetaDefsFor), so
		// forgetting this arm does not degrade the feature — it removes the
		// only call door the session has, and every call answers "unknown
		// tool" while tools/list plainly advertises three. Resolve them with
		// Surface.ResolveCallVariant, which is ResolveCall plus the tier
		// equality check; execTool takes it from there unchanged.
		//
		// Staying closed is still right for whatever is left: an unlisted
		// name reaching this far is a bug somewhere above, and answering it
		// would be guessing.
		g.replyUnknownTool(req.ID, p.Name)
	}
}

// replyResolveFailure answers a call_tool whose target did not resolve. An
// unknown name while downstreams are still connecting is the RETRYABLE busy
// condition, not "no such tool": telling an agent a tool does not exist when
// it is merely still connecting teaches it to stop asking.
func (g *gateway) replyResolveFailure(req *mcp.Request, err error) {
	var de *discovery.Error
	if errors.As(err, &de) && de.Code == discovery.CodeUnknownTool {
		if _, ready, pending := g.catalog(); !ready || pending > 0 {
			g.replyBusy(req.ID)
			return
		}
	}
	g.replyResult(req.ID, discovery.ErrorResult(err))
}

// handleFetchResult serves one page of a previously truncated result.
//
// Ownership is the ONLY isolation (cursor ids are a guessable sequence by
// design): the owner is this process's session, and every miss — unknown,
// expired, foreign — returns the one frozen not-found result that
// internal/shaping renders, so fetch_result cannot be used as a probe
// oracle.
func (g *gateway) handleFetchResult(ctx context.Context, req *mcp.Request, raw json.RawMessage) {
	args, err := discovery.ParseFetchResult(raw)
	if err != nil {
		g.replyResult(req.ID, discovery.ErrorResult(err))
		return
	}
	// args.Limit is accepted by the frozen schema but NOT honoured: page size
	// is governed by the budget that shaped page 1, which travels with the
	// retained entry. Honouring a per-fetch limit needs a seam in
	// internal/shaping; the schema keeps the field so the wire shape does not
	// change when it lands.
	res, ok := shaping.Fetch(ctx, g.cursors, g.owner, args.Cursor, args.Offset)
	if !ok {
		g.log.Info("fetch_result miss", "cursor", args.Cursor)
	}
	g.replyResult(req.ID, res)
}

// logSearch records the search trace: tool names, scores and query
// MEASUREMENTS only. The query text itself is never logged — it is
// agent-authored free text that may carry secrets or an injected payload
// (internal/discovery.Trace privacy invariant).
func (g *gateway) logSearch(res *discovery.SearchResult) {
	if res == nil {
		return
	}
	tr := res.Trace
	g.log.Info("search_tools",
		"query_bytes", tr.QueryBytes,
		"query_words", tr.QueryWords,
		"matched", tr.Matched,
		"top_score", tr.TopScore,
		"results", tr.Results,
		"truncated", tr.Truncated,
		"escalated", tr.Escalated,
		"rejected", tr.Rejected,
	)
}

// shapeResult is the pipeline's ResultShaper seam: bound the delivered
// result to the session's byte budget and retain the remainder under a
// cursor.
//
// Every unexpected condition delivers the FULL result (shaping fails open —
// internal/shaping doc.go): losing a caller's data to save tokens is a worse
// failure than spending the tokens. The closed direction belongs to the
// gates, which have already run by the time this is reached.
func (g *gateway) shapeResult(ctx context.Context, req *pipeline.CallRequest, res *mcp.CallResult) *mcp.CallResult {
	budget := shaping.Budget{Bytes: g.budgetFor(req.ServerID)}
	if budget.Bytes <= 0 || g.cursors == nil {
		return res
	}
	// The id is minted before shaping because Shape embeds it in the
	// truncation trailer. Unused ids simply leave gaps in a sequence that is
	// guessable by design, so a gap costs nothing.
	page, cursor, truncated := shaping.Shape(res, budget, shaping.Options{
		Owner: g.owner,
		ID:    g.cursors.NextID(),
	})
	if !truncated {
		return res
	}
	if err := shaping.Retain(ctx, g.cursors, cursor); err != nil {
		// The remainder could not be retained: deliver the whole result
		// rather than a page whose continuation is already lost.
		g.log.Warn("shaping cursor could not be retained; delivering the full result",
			logx.Server(req.ServerID), logx.Tool(req.RawTool), "error", err)
		return res
	}
	g.log.Info("result truncated",
		logx.Server(req.ServerID), logx.Tool(req.RawTool),
		"cursor", cursor.ID, "next_offset", cursor.NextOffset, "total", cursor.Total)
	return page
}

// budgetFor resolves the effective byte budget of one server: the
// per-server entry when present, the "*" default otherwise, 0 (= unbounded)
// when no layer set either.
func (g *gateway) budgetFor(serverID string) int {
	es := g.currentScope()
	if es == nil {
		return 0
	}
	if b, ok := es.Budgets[serverID]; ok {
		return b
	}
	return es.Budgets[budgetWildcard]
}
