package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// RootSource yields the filesystem roots governing this gateway session.
// It is the migration seam frozen by A.5 #30: M0 wires the roots-protocol
// implementation (clientRoots); M1 adds the clients.json explicit-roots
// implementation, and scope resolution consumes the interface without
// caring which one is behind it.
//
// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28): the MCP roots
// feature is deprecated upstream; when it is removed, only RootSource
// implementations change — callers stay untouched.
type RootSource interface {
	Roots(ctx context.Context) ([]mcp.Root, error)
}

// clientRoots implements RootSource via the upstream client's roots/list
// reverse RPC. The result is cached; notifications/roots/list_changed
// invalidates the cache. A client that did not declare the roots
// capability yields an empty root set (cached too — asking would violate
// the capability contract).
//
// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
type clientRoots struct {
	g *gateway

	mu    sync.Mutex
	valid bool
	roots []mcp.Root
	gen   uint64        // bumped by invalidate; a fetch only stores if unchanged
	fetch chan struct{} // non-nil while a fetch is in flight (single-flight)
}

var _ RootSource = (*clientRoots)(nil)

// Roots implements RootSource. Concurrent cache misses coalesce into one
// roots/list RPC (prefetch and on-demand peer requests race after every
// invalidation; the client must see a single query, not one per waiter).
func (c *clientRoots) Roots(ctx context.Context) ([]mcp.Root, error) {
	for {
		c.mu.Lock()
		if c.valid {
			out := make([]mcp.Root, len(c.roots))
			copy(out, c.roots)
			c.mu.Unlock()
			return out, nil
		}
		if c.fetch != nil {
			ch := c.fetch
			c.mu.Unlock()
			select {
			case <-ch:
				continue // leader finished; re-check the cache
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		gen := c.gen
		ch := make(chan struct{})
		c.fetch = ch
		c.mu.Unlock()

		roots, err := c.fetchFromClient(ctx)

		c.mu.Lock()
		if c.fetch == ch {
			c.fetch = nil
		}
		if err == nil && gen == c.gen {
			c.valid = true
			c.roots = roots
		}
		c.mu.Unlock()
		close(ch)
		if err != nil {
			return nil, err
		}
		out := make([]mcp.Root, len(roots))
		copy(out, roots)
		return out, nil
	}
}

// Cached returns the roots already in the cache, and whether the cache is
// populated. It NEVER performs the roots/list reverse RPC.
//
// This exists because scope resolution runs on paths that must not block on
// the upstream client: tools/list, the execute path, and newGateway itself —
// which runs BEFORE initialize, so a fetch there could not be answered at
// all. A cache miss yields ok=false, which callers must read as "no root
// known YET", not "no root": the prefetch at notifications/initialized
// populates it shortly after, and every consumer re-reads.
//
// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28)
func (c *clientRoots) Cached() ([]mcp.Root, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid {
		return nil, false
	}
	out := make([]mcp.Root, len(c.roots))
	copy(out, c.roots)
	return out, true
}

// fetchFromClient performs the actual capability check and reverse RPC.
// A stateless (2026-07-28) session is never asked, whatever its declared
// capabilities: that protocol removed server-initiated requests, so a
// roots/list sent to such a client is a wire error, not a question. Roots
// for stateless sessions come from configuration alone.
func (c *clientRoots) fetchFromClient(ctx context.Context) ([]mcp.Root, error) {
	c.g.mu.Lock()
	supported := c.g.clientCaps.Roots != nil && !c.g.stateless
	c.g.mu.Unlock()
	if !supported {
		return nil, nil
	}

	raw, err := c.g.callClient(ctx, mcp.MethodRootsList, nil)
	if err != nil {
		return nil, fmt.Errorf("gateway: roots/list from client: %w", err)
	}
	var res mcp.ListRootsResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("gateway: decode roots/list result: %w", err)
	}
	return res.Roots, nil
}

// prefetch warms the cache (fire-and-forget; errors only log).
func (c *clientRoots) prefetch(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, rootsTimeout)
	defer cancel()
	if _, err := c.Roots(cctx); err != nil && ctx.Err() == nil {
		c.g.log.Debug("roots prefetch failed", "error", err)
	}
}

// invalidate drops the cache (roots/list_changed). The generation bump
// makes any in-flight fetch discard its (now potentially stale) result.
func (c *clientRoots) invalidate() {
	c.mu.Lock()
	c.gen++
	c.valid = false
	c.roots = nil
	c.mu.Unlock()
}
