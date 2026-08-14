package gateway

import (
	"context"
	"net/url"
	"strings"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/scope"
)

// This file wires derived downstream instances (docs/subsystems/execution.md) into the
// stdio gateway. The three planes stay exactly where they were:
//
//   - CONNECTION plane: the base connection of every enabled server is
//     unchanged (connectAll/connectOne). Derived instances are extra
//     connections owned by the pool, dialed on first use.
//   - ROUTING plane: unchanged. router.RouteOf is still the single
//     provenance of a call; a derived instance shares its base server's id,
//     so exposed names, scope keys and audit records are identical.
//   - VISIBILITY plane: untouched. Deriving never adds, hides or renames a
//     tool (docs/model.md invariant 2).
//
// The only thing that changes is WHICH process executes an allowed call,
// decided per call in execTool by the key below.

// deriveTargetFor returns the derivation this session must use for one
// server, plus the context that specializes its spec.
//
// An empty key means "the base instance" and is the answer whenever the
// server does not derive, or the input the mode keys on is missing (no root
// reported, no session identity). Falling back to the base instance is the
// available direction, and it is safe: the base spec is exactly what the
// operator configured.
func (g *gateway) deriveTargetFor(ctx context.Context, spec downstream.Spec) (downstream.DeriveKey, downstream.DeriveContext) {
	switch spec.Derive {
	case downstream.DeriveRoot:
		root := g.primaryRoot(ctx)
		if root == "" {
			return "", downstream.DeriveContext{}
		}
		return downstream.RootDeriveKey(root), downstream.DeriveContext{Root: root}
	case downstream.DeriveSession:
		// The gateway process IS one session (gateway doc). Its identity is
		// the daemon-assigned session id while the control link is
		// registered, and the process-fixed cursor owner otherwise — the
		// same fallback the shaping cursors use, so a standalone gateway
		// still gets exactly one stable derivation instead of none.
		sid := ""
		if g.ctl != nil {
			sid = g.ctl.Session()
		}
		if sid == "" {
			sid = string(g.owner)
		}
		return downstream.SessionDeriveKey(sid), downstream.DeriveContext{Root: g.primaryRoot(ctx)}
	default:
		return "", downstream.DeriveContext{}
	}
}

// primaryRoot returns the session's first client-reported root, normalized.
// Roots are served from the prefetched cache; a fetch failure yields "" (no
// derivation rather than a guessed one).
//
// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
func (g *gateway) primaryRoot(ctx context.Context) string {
	if g.roots == nil {
		return ""
	}
	rctx, cancel := context.WithTimeout(ctx, rootsTimeout)
	defer cancel()
	roots, err := g.roots.Roots(rctx)
	if err != nil || len(roots) == 0 {
		return ""
	}
	return scope.NormalizePath(rootPathOf(roots[0].URI))
}

// cachedPrimaryRoot is primaryRoot without the fetch: it answers only from
// the roots cache and returns "" on a miss.
//
// It is what scope resolution uses (scope.go). The distinction from
// primaryRoot is deliberate and not an optimization: primaryRoot may block on
// a reverse RPC because deriveTargetFor runs inside a single tools/call,
// where waiting for the client is legitimate. Scope resolution runs on
// tools/list, on the execute path, and inside newGateway BEFORE the client
// has initialized — blocking there would stall every listing on a client
// round trip and could not be answered at all in the newGateway case.
//
// A miss returning "" means per-project bindings do not match for that
// resolution, which is exactly the pre-wiring behavior: the client-level
// binding applies. Once the prefetch lands, the next resolve sees the root —
// the resolver's cache key includes it (scope/resolver.go), so the stale
// rootless entry is replaced without any explicit invalidation.
//
// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
func (g *gateway) cachedPrimaryRoot() string {
	if g.roots == nil {
		return ""
	}
	roots, ok := g.roots.Cached()
	if !ok || len(roots) == 0 {
		return ""
	}
	return scope.NormalizePath(rootPathOf(roots[0].URI))
}

// rootPathOf converts a root URI into a filesystem path. MCP roots are
// file:// URIs, but clients have been observed sending bare paths; both are
// accepted, and anything else yields "" (an unusable root is no root — it
// must never become a cwd).
func rootPathOf(uri string) string {
	s := strings.TrimSpace(uri)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	if u.Path == "" {
		return ""
	}
	// A Windows file URI is file:///C:/x — drop the leading slash so the
	// path is the one the OS understands.
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return p
}

// acquire returns the instance that must execute one call on serverID, and
// the lease the caller must release when the call completes.
//
// base is the connected base server (never nil here: execTool resolved it
// through the router). A derivation that cannot connect is reported as an
// ERROR — running the call on the base instance would execute it with the
// wrong cwd/env/credential, which is precisely the isolation the operator
// configured (downstream.Pool doc, failure direction).
func (g *gateway) acquire(ctx context.Context, base *downstream.Server, serverID string) (*downstream.Lease, error) {
	g.mu.Lock()
	spec, known := g.specByIDLocked(serverID)
	pool := g.pool
	g.mu.Unlock()
	if !known || pool == nil {
		return &downstream.Lease{Server: base}, nil
	}
	// An empty key is the one "run on the base instance" answer, and
	// deriveTargetFor already gives it for every mode that does not derive —
	// including DeriveNone and the zero value — without touching the roots
	// cache. Testing spec.Derive here as well only asked the same question
	// twice, in a spelling that had to be kept in step with that switch.
	key, dc := g.deriveTargetFor(ctx, spec)
	if key == "" {
		return &downstream.Lease{Server: base}, nil
	}
	lease, err := pool.Acquire(ctx, base, spec.Derived(key, dc), key)
	if err != nil {
		return nil, err
	}
	if lease.Fallback {
		g.log.Warn("call served by the base instance: derived-instance cap reached",
			logx.Server(serverID), logx.Instance(string(key)))
	}
	return lease, nil
}
