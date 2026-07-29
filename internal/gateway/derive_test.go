package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// seedDerivingServer writes one enabled stdio entry with a derive policy
// and a ${ROOT}-templated cwd.
func seedDerivingServer(t *testing.T, resolver *platform.Resolver, id, mode string) {
	t.Helper()
	dir, err := resolver.RegistryDir()
	if err != nil {
		t.Fatalf("RegistryDir: %v", err)
	}
	store, err := registry.Open(dir)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	updateRegistry(t, store, func(tx *registry.Tx) {
		// Full, for the same reason as seedRegistry's copy: these tests wait
		// for a derived instance's tool to appear in tools/list by name, and
		// the default (lazy) lists the meta-tools instead.
		tx.Governance.V.Discovery = "full"
		if tx.Servers.V.Servers == nil {
			tx.Servers.V.Servers = map[string]registry.Doc[registry.ServerEntry]{}
		}
		tx.Servers.V.Servers[id] = registry.Doc[registry.ServerEntry]{V: registry.ServerEntry{
			Transport: "stdio",
			Command:   "unused-in-tests",
			Cwd:       "${ROOT}",
			Derive:    mode,
			Enabled:   true,
		}}
	})
}

// specRecorder is a dial function that records every spec it is asked for.
type specRecorder struct {
	mu    sync.Mutex
	specs []downstream.Spec
	dial  downstream.DialFunc
}

func newSpecRecorder(scripts map[string]*fakemcp.Script) *specRecorder {
	r := &specRecorder{}
	inner := scriptedDial(scripts)
	r.dial = func(ctx context.Context, spec downstream.Spec) (transport.Transport, error) {
		r.mu.Lock()
		r.specs = append(r.specs, spec)
		r.mu.Unlock()
		return inner(ctx, spec)
	}
	return r
}

func (r *specRecorder) recorded() []downstream.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]downstream.Spec, len(r.specs))
	copy(out, r.specs)
	return out
}

// derivedSpecs returns only the recorded specs of derived instances.
func (r *specRecorder) derivedSpecs() []downstream.Spec {
	var out []downstream.Spec
	for _, s := range r.recorded() {
		if s.DeriveKey != "" {
			out = append(out, s)
		}
	}
	return out
}

// TestDerivedInstancePerRoot is the end-to-end shape of docs/modules/dataplane.md in
// the gateway: the client's root selects a per-root instance, dialed with
// the expanded cwd, while the CATALOG is untouched — one server id, one
// exposed name, one route.
func TestDerivedInstancePerRoot(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedDerivingServer(t, resolver, "fs", "root")
	rec := newSpecRecorder(map[string]*fakemcp.Script{"fs": fakemcp.Minimal("echo")})

	g, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     rec.dial,
	})
	c.answerRoots = true
	c.roots = []mcp.Root{{URI: "file:///w/app", Name: "app"}}
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{Roots: &mcp.RootsCapability{}})
	waitForTools(t, c, "fs__echo")

	// The base connection is dialed WITHOUT a derivation: deriving is an
	// addition to the connection plane, not a replacement.
	if base := rec.recorded(); len(base) != 1 || base[0].DeriveKey != "" {
		t.Fatalf("base dial = %+v, want exactly one underived connection", base)
	}

	res := callToolResult(t, c, "fs__echo", map[string]any{"x": 1})
	if res.IsError {
		t.Fatalf("call reported an error: %s", res.Content)
	}
	derived := rec.derivedSpecs()
	if len(derived) != 1 {
		t.Fatalf("derived dials = %+v, want 1", derived)
	}
	wantKey := downstream.RootDeriveKey("/w/app")
	if derived[0].DeriveKey != wantKey {
		t.Fatalf("derive key = %q, want %q", derived[0].DeriveKey, wantKey)
	}
	if derived[0].Cwd != "/w/app" {
		t.Fatalf("derived cwd = %q, want the expanded root", derived[0].Cwd)
	}
	if derived[0].ID != "fs" || derived[0].ScopeName != string(wantKey) {
		t.Fatalf("derived spec identity = id %q scope %q", derived[0].ID, derived[0].ScopeName)
	}

	// Visibility plane untouched: the derivation added no tool, renamed
	// nothing, and RouteOf still names the base server.
	if got := toolNames(c.listTools()); len(got) != 1 || got[0] != "fs__echo" {
		t.Fatalf("catalog changed by derivation: %v", got)
	}
	rt, _, _ := g.catalog()
	if route, ok := rt.RouteOf("fs__echo"); !ok || route.ServerID != "fs" {
		t.Fatalf("RouteOf = %+v ok %v", route, ok)
	}

	// A second call on the same root reuses the instance.
	_ = callToolResult(t, c, "fs__echo", map[string]any{"x": 2})
	if got := len(rec.derivedSpecs()); got != 1 {
		t.Fatalf("derived dials after a second call on the same root = %d, want 1", got)
	}

	// A new root is a new instance; the old one stays until it idles out.
	c.mu.Lock()
	c.roots = []mcp.Root{{URI: "file:///w/other"}}
	c.mu.Unlock()
	c.notify(mcp.NotificationRootsListChanged, nil)
	waitFor(t, "the second root to produce its own instance", func() bool {
		_ = callToolResult(t, c, "fs__echo", map[string]any{"x": 3})
		return len(rec.derivedSpecs()) == 2
	})
	inst := g.pool.Instances()
	if len(inst) != 2 {
		t.Fatalf("live instances = %+v, want two roots", inst)
	}
	if inst[0].ServerID != "fs" || inst[1].ServerID != "fs" {
		t.Fatalf("instances belong to the wrong server: %+v", inst)
	}
}

// TestDerivedInstanceCapFallsBackToBase: over the per-server cap the call is
// still served — by the base instance — instead of failing. Degraded
// sharing beats an unbounded process fan-out.
func TestDerivedInstanceCapFallsBackToBase(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedDerivingServer(t, resolver, "fs", "root")
	rec := newSpecRecorder(map[string]*fakemcp.Script{"fs": fakemcp.Minimal("echo")})

	g, c, _ := startGateway(t, Config{
		ClientID:    "test-client",
		Resolver:    resolver,
		Dial:        rec.dial,
		DerivedPool: downstream.PoolOptions{MaxPerServer: 1, SweepInterval: -1},
	})
	c.answerRoots = true
	c.roots = []mcp.Root{{URI: "file:///w/one"}}
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{Roots: &mcp.RootsCapability{}})
	waitForTools(t, c, "fs__echo")

	_ = callToolResult(t, c, "fs__echo", map[string]any{})
	if got := len(rec.derivedSpecs()); got != 1 {
		t.Fatalf("derived dials = %d, want 1", got)
	}

	c.mu.Lock()
	c.roots = []mcp.Root{{URI: "file:///w/two"}}
	c.mu.Unlock()
	c.notify(mcp.NotificationRootsListChanged, nil)

	// The roots notification is processed asynchronously, so poll until the
	// second root is the one being derived for.
	waitFor(t, "the second root to hit the per-server cap", func() bool {
		res := callToolResult(t, c, "fs__echo", map[string]any{})
		if res.IsError {
			t.Fatalf("over-cap call failed instead of falling back: %s", res.Content)
		}
		return g.pool.Overflows() > 0
	})
	if got := len(rec.derivedSpecs()); got != 1 {
		t.Fatalf("derived dials after the cap = %d, want 1 (the second root must reuse the base)", got)
	}
}

// TestDerivedInstanceIdleReclaim: once the call is done the lease is
// released, and the instance is reclaimed after the idle TTL — the delayed
// close of docs/modules/dataplane.md.
func TestDerivedInstanceIdleReclaim(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedDerivingServer(t, resolver, "fs", "session")
	rec := newSpecRecorder(map[string]*fakemcp.Script{"fs": fakemcp.Minimal("echo")})

	clock := time.Unix(1_700_000_000, 0)
	var clockMu sync.Mutex
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	g, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     rec.dial,
		DerivedPool: downstream.PoolOptions{
			IdleTTL:       30 * time.Minute,
			SweepInterval: -1, // reclaim driven explicitly: no sweeper race
			Now:           now,
		},
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fs__echo")

	_ = callToolResult(t, c, "fs__echo", map[string]any{})
	if len(g.pool.Instances()) != 1 {
		t.Fatalf("instances = %+v, want the session derivation", g.pool.Instances())
	}
	if n := g.pool.Sweep(); n != 0 {
		t.Fatalf("swept %d instances before the TTL", n)
	}

	clockMu.Lock()
	clock = clock.Add(31 * time.Minute)
	clockMu.Unlock()
	if n := g.pool.Sweep(); n != 1 {
		t.Fatalf("swept %d instances after the TTL, want 1", n)
	}

	// Reclaim is invisible upstream: the next call re-dials and answers.
	res := callToolResult(t, c, "fs__echo", map[string]any{})
	if res.IsError {
		t.Fatalf("call after reclaim failed: %s", res.Content)
	}
	if got := len(rec.derivedSpecs()); got != 2 {
		t.Fatalf("derived dials = %d, want 2 (initial + re-dial)", got)
	}
}

// TestNonDerivingServerIsUnchanged pins the backward-compatible default: an
// entry without `derive` never enters the pool, and its calls run on the
// base connection.
func TestNonDerivingServerIsUnchanged(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	rec := newSpecRecorder(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")})

	g, c, _ := startGateway(t, Config{ClientID: "test-client", Resolver: resolver, Dial: rec.dial})
	c.answerRoots = true
	c.roots = []mcp.Root{{URI: "file:///w/app"}}
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{Roots: &mcp.RootsCapability{}})
	waitForTools(t, c, "fake__echo")

	_ = callToolResult(t, c, "fake__echo", map[string]any{})
	if got := rec.derivedSpecs(); len(got) != 0 {
		t.Fatalf("a non-deriving server produced derived instances: %+v", got)
	}
	if got := g.pool.Instances(); len(got) != 0 {
		t.Fatalf("pool instances = %+v, want none", got)
	}
}

// TestRootPathOf covers the URI shapes clients actually send. Anything
// unusable yields "" — an unusable root must never become a cwd.
func TestRootPathOf(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"file:///w/app":     "/w/app",
		"file:///C:/w/app":  "C:/w/app",
		"/w/app":            "/w/app",
		"":                  "",
		"https://host/path": "",
		"file://":           "",
	} {
		if got := rootPathOf(in); got != want {
			t.Fatalf("rootPathOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDeriveKeyWithoutRootIsBase: a root-deriving server on a session that
// reports no root uses the base instance rather than a variant keyed by "".
func TestDeriveKeyWithoutRootIsBase(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedDerivingServer(t, resolver, "fs", "root")
	rec := newSpecRecorder(map[string]*fakemcp.Script{"fs": fakemcp.Minimal("echo")})

	g, c, _ := startGateway(t, Config{ClientID: "test-client", Resolver: resolver, Dial: rec.dial})
	// No roots capability: the client is never asked, so there is no root.
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fs__echo")

	res := callToolResult(t, c, "fs__echo", map[string]any{})
	if res.IsError {
		t.Fatalf("call failed: %s", res.Content)
	}
	if got := rec.derivedSpecs(); len(got) != 0 {
		t.Fatalf("derived instances without a root: %+v", got)
	}
	if got := g.pool.Instances(); len(got) != 0 {
		t.Fatalf("pool instances = %+v, want none", got)
	}
	if c.rootsCalls.Load() != 0 {
		t.Fatal("a client without the roots capability must never be queried")
	}
}
