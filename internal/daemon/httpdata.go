package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/scope"
)

// This file assembles the daemon's DATA plane: the httpbridge.Dispatcher that
// turns an authenticated HTTP caller into MCP work.
//
// The assembly is deliberately thin, and that is the whole point. It owns no
// gates, no router and no shaping; it resolves a credential to a
// gateway.Conn — the SAME gateway body `agenthub connect` runs, reached over
// an in-memory pipe instead of stdin/stdout — and writes the request into it.
// canonical.md §2 freezes "one execute pipeline"; the way to keep that true
// while adding a second transport is to add no second assembly, so the HTTP
// face has no shortcut available to it.
//
// The credential enters the governance chain in exactly two places, both of
// them existing ones:
//
//   - Caller.Tier becomes gateway.Config.CallerTier, which the pipeline's
//     token tier gate compares against each tool's annotation-derived tier;
//   - Caller.Servers and Caller.Profile become extra scope layers
//     (scope.Sources.Extra), merged by the same Merge that folds the
//     persisted five. Both are security fields, so they intersect: a token
//     can only ever narrow what its client configuration already allows.

// httpConnIdle bounds how long an unused per-credential gateway stays up.
// Downstream processes are expensive, and a credential that stopped calling
// should stop costing; the next request re-assembles in the background the
// same way a fresh `agenthub connect` does.
const httpConnIdle = 30 * time.Minute

// httpConnSweep is the reaper period.
const httpConnSweep = time.Minute

// httpPlaneDeps is what the data plane needs from the daemon assembly.
type httpPlaneDeps struct {
	Resolver *platform.Resolver
	Log      *slog.Logger
	// Events records the HTTP face's session lifecycle. This is the one face
	// where a process holds many sessions at once, so it is the only one that
	// can answer "which were live at 11:03".
	Events  *eventlog.Stream
	Version string
	// Registry is the daemon's live store; it is read only to resolve a
	// token's profile pin. nil = no pin can be resolved, which fails closed
	// (see scopeLayers).
	Registry *registry.Store
	// THERE ARE DELIBERATELY NO CREDENTIAL FIELDS HERE, and the pair that
	// used to be — a ${SECRET_x} resolver and an OAuth bearer factory, both
	// built from the daemon's vault — must not come back. gateway.newGateway
	// builds its own vault chain exactly when both arrive nil, and only that
	// chain wraps the bearer in the two optional faces the round tripper
	// reads: the credential epoch (a CredWatcher bumps it when any process
	// rewrites the vault) and the refresh deadline (renew before expiry,
	// which is the only rule that fires against a downstream answering a
	// dead token with 200 instead of 401). Supplying either field suppressed
	// that chain and replaced it with a bare vault read, leaving these
	// gateways recoverable only by a 401 — strictly weaker than the stdio
	// ones they are supposed to be identical to.
	//
	// Dial overrides downstream transport creation (tests).
	Dial downstream.DialFunc
	// Now overrides the clock (tests).
	Now func() time.Time
}

// httpPlane is the httpbridge.Dispatcher backed by per-credential gateways.
type httpPlane struct {
	deps httpPlaneDeps

	mu     sync.Mutex
	conns  map[string]*httpConn
	closed bool
}

// httpConn is one live gateway plus its idle bookkeeping.
type httpConn struct {
	conn *gateway.Conn
	// ready is closed once conn is assembled (or failed); concurrent first
	// requests for one credential must not each start a gateway.
	ready    chan struct{}
	err      error
	lastUsed time.Time
}

var _ httpbridge.Dispatcher = (*httpPlane)(nil)

func newHTTPPlane(deps httpPlaneDeps) *httpPlane {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	return &httpPlane{deps: deps, conns: make(map[string]*httpConn)}
}

// Dispatch implements httpbridge.Dispatcher.
func (p *httpPlane) Dispatch(ctx context.Context, c *httpbridge.Caller, _ *httpbridge.Session, req *mcp.Request) *mcp.Response {
	conn, err := p.connFor(ctx, c)
	if err != nil {
		// Assembly failure is an internal error with no detail: the message
		// crosses an authenticated but untrusted boundary.
		p.deps.Log.Warn("http data plane could not serve a caller",
			"caller", string(c.Kind), "token", c.Token, "error", err)
		return mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code: mcp.CodeInternalError, Message: "agenthub could not start this session's gateway",
		})
	}
	return conn.Do(ctx, req)
}

// Notify implements httpbridge.Dispatcher.
func (p *httpPlane) Notify(ctx context.Context, c *httpbridge.Caller, _ *httpbridge.Session, n *mcp.Notification) {
	conn, err := p.connFor(ctx, c)
	if err != nil {
		return // a notification is never answered, so a failure is only logged below
	}
	if err := conn.Notify(n); err != nil {
		p.deps.Log.Debug("notification not delivered to the gateway", "method", n.Method, "error", err)
	}
}

// Close stops every gateway. Called once, from the daemon's cleanup.
func (p *httpPlane) Close() {
	p.mu.Lock()
	p.closed = true
	conns := make([]*httpConn, 0, len(p.conns))
	for _, hc := range p.conns {
		conns = append(conns, hc)
	}
	clear(p.conns)
	p.mu.Unlock()
	for _, hc := range conns {
		<-hc.ready
		if hc.conn != nil {
			hc.conn.Close()
		}
	}
}

// connFor resolves the credential's gateway, assembling it on first use.
//
// The key is the WHOLE credential (kind, name, tier, allowlist, profile), not
// just the token name: two credentials that differ in any of those must not
// share a gateway, because tier and scope are baked into the assembly. That
// also means a token narrowed after issue gets a fresh gateway rather than
// riding the old one's authority — the same rule httpbridge applies to
// sessions (Caller.identity).
func (p *httpPlane) connFor(ctx context.Context, c *httpbridge.Caller) (*gateway.Conn, error) {
	if c == nil {
		return nil, fmt.Errorf("daemon: no caller identity") // fail-closed
	}
	key := c.Identity()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("daemon: http data plane is shutting down")
	}
	hc, ok := p.conns[key]
	if ok {
		hc.lastUsed = p.deps.Now()
		p.mu.Unlock()
		select {
		case <-hc.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return hc.conn, hc.err
	}
	hc = &httpConn{ready: make(chan struct{}), lastUsed: p.deps.Now()}
	p.conns[key] = hc
	p.mu.Unlock()

	conn, err := gateway.Open(p.gatewayConfig(c))
	hc.conn, hc.err = conn, err
	close(hc.ready)
	if err != nil {
		// Do not cache a failed assembly: the cause (an unreadable registry,
		// a missing binary) is usually transient from the caller's point of
		// view, and a cached failure would outlive its repair.
		p.mu.Lock()
		if p.conns[key] == hc {
			delete(p.conns, key)
		}
		p.mu.Unlock()
		return nil, err
	}
	p.deps.Log.Info("http data plane gateway assembled",
		"caller", string(c.Kind), "token", c.Token, "tier", string(c.Tier), logx.Client(clientIDOf(c)))
	return conn, nil
}

// gatewayConfig maps one credential onto a gateway assembly.
//
// Secrets and Auth are left UNSET, which is what makes the gateway build the
// production credential chain for itself (see httpPlaneDeps). Setting either
// is the one edit here that changes nothing visible and silently costs three
// of the round tripper's four cache-invalidation rules.
func (p *httpPlane) gatewayConfig(c *httpbridge.Caller) gateway.Config {
	return gateway.Config{
		ClientID:    clientIDOf(c),
		Face:        "http",
		Resolver:    p.deps.Resolver,
		Log:         p.deps.Log.With("caller", string(c.Kind), "token", c.Token),
		Version:     p.deps.Version,
		Dial:        p.deps.Dial,
		CallerTier:  c.Tier,
		ScopeLayers: p.scopeLayers(c),
	}
}

// clientIDOf is the scope routing key of a credential.
//
// An agent token routes as its own NAME, so `agenthub client` configuration
// (profile binding, discovery mode, further narrowing) applies to a token the
// same way it applies to Cursor or Claude Code — one visibility model, not
// two. The tokenless kinds get fixed ids that no `token create` can mint
// (names are [A-Za-z0-9._-], so a colon cannot collide).
func clientIDOf(c *httpbridge.Caller) string {
	switch c.Kind {
	case httpbridge.CallerAgent:
		return c.Token
	case httpbridge.CallerAdmin:
		return "http:admin"
	default:
		return "http:loopback"
	}
}

// scopeLayers turns the credential's allowlist and profile pin into scope
// layers. Returns nil when the credential constrains nothing (an admin token,
// or an agent token with no allowlist and no pin) — nil layers are "no
// intervention", which is what an unconstrained credential means.
func (p *httpPlane) scopeLayers(c *httpbridge.Caller) func() []scope.ScopeLayer {
	servers := allowlistLayerServers(c.Servers)
	profile := strings.TrimSpace(c.Profile)
	if servers == nil && profile == "" {
		return nil
	}
	origin := "token:" + c.Token
	if c.Token == "" {
		origin = "caller:" + string(c.Kind)
	}
	return func() []scope.ScopeLayer {
		var layers []scope.ScopeLayer
		if servers != nil {
			// Session kind: the narrowing lives for the credential's session
			// and is never persisted, which is exactly the session layer's
			// contract.
			layers = append(layers, scope.ScopeLayer{
				Kind:    scope.LayerSession,
				Origin:  origin,
				Servers: servers,
			})
		}
		if profile != "" {
			var snap *registry.Snapshot
			if p.deps.Registry != nil {
				snap = p.deps.Registry.Snapshot()
			}
			layer, ok := scope.PinnedProfileLayer(snap, profile)
			if !ok {
				// Fail-closed: PinnedProfileLayer already returned a
				// block-all layer; say so rather than let an operator wonder
				// why the token sees nothing.
				p.deps.Log.Warn("token pins a profile that does not exist; its scope is empty",
					"token", c.Token, "profile", profile)
			}
			layers = append(layers, layer)
		}
		return layers
	}
}

// allowlistLayerServers converts a token allowlist into a scope layer's
// Servers field, preserving the three-state contract: nil = no intervention,
// [] = block-all, [...] = intersect. The wildcard collapses to nil, its
// explicit spelling of "every server".
func allowlistLayerServers(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == httpbridge.ServerWildcard {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// reap closes gateways that have been idle past httpConnIdle. It runs until
// ctx is done; the daemon's Close handles whatever is left.
func (p *httpPlane) reap(ctx context.Context) {
	t := time.NewTicker(httpConnSweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sweep(p.deps.Now())
		}
	}
}

// sweep closes and forgets every gateway idle since before cutoff.
func (p *httpPlane) sweep(now time.Time) {
	var stale []*httpConn
	p.mu.Lock()
	for key, hc := range p.conns {
		select {
		case <-hc.ready:
		default:
			continue // still assembling; never reap a connection in flight
		}
		if now.Sub(hc.lastUsed) < httpConnIdle {
			continue
		}
		delete(p.conns, key)
		stale = append(stale, hc)
	}
	p.mu.Unlock()
	for _, hc := range stale {
		if hc.conn != nil {
			hc.conn.Close()
		}
	}
}
