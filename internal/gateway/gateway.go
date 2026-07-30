// Package gateway assembles the per-client stdio gateway — the implementation
// body of `agenthub connect --client <id>` (canonical.md §2: gateway only
// assembles; the execute pipeline lives in internal/pipeline).
//
// Run serves the upstream AI client as an MCP server over the given
// reader/writer pair (os.Stdin / os.Stdout in production; stdout is the
// protocol channel, so logs go to stderr and the JSON log file, never
// stdout). Everything MCP goes through the internal/mcp facade.
//
// Startup sequence (docs/flows.md, "answer first, connect later"):
//
//  1. load the on-disk tool cache (<data>/cache/tools/<server>.json),
//  2. load the registry — a load failure does NOT abort: the gateway starts
//     with an empty config, logs a warning, and answers from the cache,
//  3. answer initialize immediately (tool catalog may come from the cache),
//  4. connect every enabled downstream concurrently in the background,
//  5. once the first real tool lists are ready: build the live router, push
//     notifications/tools/list_changed upstream, and persist each server's
//     tools back into the cache (atomic write).
//
// While the live router is not ready, tools/list answers from the cache
// (aggregated by router.BuildFromCache — same exposed-name rules) and
// tools/call answers a retryable busy error.
//
// Upstream surface: initialize / initialized / ping / tools/list /
// tools/call / notifications/cancelled / roots reverse RPC, plus
// shutdown+exit and EOF for termination. Unknown methods get
// MethodNotFound; unknown notifications are ignored.
//
// What tools/list SHOWS and what an incoming tools/call NAME means are the
// discovery plane's business (discovery.go): the session's effective scope
// selects the exposure mode (full / grouped / lazy), and in lazy mode the
// five meta-tools are dispatched there. Everything that actually executes
// still goes through one function (execTool → pipeline.Execute), and every
// delivered result is budgeted by one hook (shapeResult).
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/audit"
	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/guard/injection"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/ratelimit"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/scope"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/shaping"
	"github.com/dinstein/agent-hub/internal/skills"
	"github.com/dinstein/agent-hub/internal/tier"
)

// serverName is what the gateway reports as serverInfo.name upstream.
const serverName = "agenthub"

// codeRetryBusy is the JSON-RPC error code for "downstream servers are
// still connecting; retry shortly" (implementation-defined server error
// range). The condition is transient by construction, so clients should
// treat it as retryable.
const codeRetryBusy = -32000

// rootsTimeout bounds one roots/list reverse RPC to the upstream client.
const rootsTimeout = 10 * time.Second

// Config configures one gateway process. In/Out and ClientID are required;
// everything else has production defaults.
type Config struct {
	// ClientID is the client identity this gateway serves (scope routing
	// key, log field, log file name).
	ClientID string
	// In/Out is the upstream MCP channel (os.Stdin / os.Stdout in
	// production). Out carries protocol frames only.
	In  io.Reader
	Out io.Writer
	// Resolver overrides platform path resolution (nil = real environment;
	// AGENTHUB_DATA_DIR is honored through it either way).
	Resolver *platform.Resolver
	// Secrets resolves ${SECRET_X} placeholders at dial time. nil builds
	// the production four-level chain; tests inject a fake so no run ever
	// reaches the real OS keyring. A resolver that returns nothing is not
	// the same as no resolver: the latter makes every placeholder a dial
	// error (downstream.ErrNoResolver).
	Secrets secrets.Resolver
	// Auth builds the OAuth bearer credential of one HTTP downstream. nil
	// builds the production source (vault token + offline refresh) unless
	// Secrets was injected, which marks a test assembly that must not reach
	// the real keyring.
	//
	// Secrets is NOT enough on its own: it resolves ${SECRET_X} placeholders
	// written into a spec, while this supplies the Authorization bearer an
	// OAuth server never appears in the spec for. Leaving it nil is why
	// every HTTP downstream answered 401 while `auth ls` showed the very
	// same tokens as authorized — the gateway held the vault but never
	// opened it.
	// scopeName is secrets.DefaultScope for a base connection and the derive
	// key for a derived instance (docs/modules/dataplane.md).
	Auth func(serverID, scopeName string) downstream.TokenSource
	// Log overrides the logger. nil builds the production logx pair: text
	// to LogWriter (default os.Stderr) + JSON to
	// <data>/logs/gateway-<client>.log.
	Log *slog.Logger
	// LogWriter receives the human-readable text log when Log is nil.
	LogWriter io.Writer
	// Version is reported as serverInfo.version (default "0.0.0-dev").
	Version string
	// Dial overrides downstream transport creation (tests). nil spawns
	// real stdio children.
	Dial downstream.DialFunc
	// ConnectTimeout bounds each downstream first-connect (0 = downstream
	// default, 120s).
	ConnectTimeout time.Duration
	// LinkRetry is the daemon control-link re-register interval
	// (0 = 30s, docs/architecture.md §2). Tests shrink it.
	LinkRetry time.Duration
	// RedialBase is the first rung of the re-dial ladder (redial.go); the
	// consult tick and the ceiling derive from it. 0 = 5s. Tests shrink it.
	RedialBase time.Duration
	// DerivedPool tunes the derived-instance pool (docs/modules/dataplane.md). Deps,
	// Connect, Log and Now are overwritten by the gateway; the caps and
	// timers are honoured. Tests use it to drive reclaim deterministically.
	DerivedPool downstream.PoolOptions

	// CallerTier is the operation tier of the CREDENTIAL this gateway serves
	// (internal/httpbridge mints it from an agent token). It travels verbatim
	// into pipeline.CallRequest.CallerTier, where the token tier gate — not
	// this package — compares it against each tool's annotation-derived tier.
	//
	// The empty value is the stdio default and is NOT a hole: a pipe from the
	// user's own terminal carries no credential, so there is no tier to
	// enforce (see pipeline.tokenTierGate).
	CallerTier tier.Tier

	// ScopeLayers supplies extra narrowing layers for the session's effective
	// scope (scope.Sources.Extra) — how a credential's server allowlist and
	// profile pin join the SAME three-layer intersection the persisted config
	// uses instead of growing a second visibility rule. nil = no extra layers.
	//
	// Failure direction: layers can only tighten (Merge intersects security
	// fields), so a broken source costs visibility, never grants it.
	ScopeLayers func() []scope.ScopeLayer
}

// Run assembles and runs the gateway until the upstream client goes away
// (EOF), sends shutdown+exit, or ctx is cancelled. It returns nil on a
// clean client-initiated termination.
func Run(ctx context.Context, cfg Config) error {
	g, err := newGateway(cfg)
	if err != nil {
		return err
	}
	defer g.shutdown()
	return g.run(ctx)
}

// gateway is the assembled process state.
type gateway struct {
	cfg      Config
	log      *slog.Logger
	logClose func() error

	// resolver is the path resolver this gateway was assembled with; kept so
	// a late assembly step (the governance-gated skills face) can resolve
	// <data> without threading it through every call.
	resolver *platform.Resolver

	fw    *mcp.FrameWriter
	pipe  *pipeline.Pipeline
	cache *toolCache
	roots *clientRoots

	// Discovery / shaping plane (docs/flows.md, discovery.go).
	//
	// guard is per SESSION and deliberately outlives surface rebuilds: it
	// tracks an agent's search loop, which a catalog refresh does not end. A
	// scope change does end it (the surface it looped over is gone), which is
	// why refreshScopeAndNotify resets it.
	guard *discovery.SearchGuard
	// pins reports config-pinned tools (exposed directly even in lazy mode).
	// nil = nothing pinned: the registry schema has no pin field yet, and the
	// neutral direction is to pin nothing rather than guess.
	pins discovery.PinSet
	// cursors retains truncated remainders for fetch_result. MemStore is the
	// right store here because the PROCESS IS THE SESSION: cursor lifetime
	// aligns with the client connection by construction, and nothing needs to
	// survive a restart.
	cursors *shaping.MemStore
	// owner binds every cursor to this session. It is process-fixed rather
	// than derived from the daemon-assigned session id, which may change
	// under a re-registration while the client connection — and therefore the
	// cursors it holds — stays the same.
	owner shaping.Owner
	// savings appends the token-savings estimates (nil when the stream could
	// not be opened; accounting never blocks serving).
	savings *audit.SavingsStream
	// traces owns the per-server JSON-RPC frame logs (trace.go). nil when
	// the logs directory could not be resolved, which disables tracing and
	// nothing else.
	traces *traceLogs

	// Quota plane (internal/ratelimit, ratelimit.go): rlStore owns the
	// counter file SHARED with every other gateway process and the daemon;
	// limiter holds the rule set currently applied (nil = no quotas
	// configured, the zero-cost path). They live here rather than inside the
	// pipeline because a quota is an admission WRAPPER around the call, not
	// a gate — the frozen gate chain stays four gates long.
	rlStore *ratelimit.Store
	limiter atomic.Pointer[ratelimit.Limiter]

	// pool owns the DERIVED downstream instances (docs/modules/dataplane.md). The base
	// connections stay in g.servers: derivation is an addition to the
	// connection plane, never a replacement for it.
	pool *downstream.Pool
	// skills is the skills-over-MCP supply face (docs/modules/config.md), nil when
	// governance leaves the switch off. It is a router.Provider: its tools
	// aggregate, route, scope and execute exactly like a downstream's.
	skills *skills.Provider

	// Tool governance plane (toolpolicy.go): the operator kill switch and the
	// integrity quarantine set, enforced at aggregation time. policySrc is
	// nil when the state stores could not be opened; policy is the deny-set
	// snapshot the router builds read, and a NIL pointer means UNKNOWN — the
	// fail-closed state in which the gateway exposes nothing.
	policySrc *toolPolicySource
	policy    atomic.Pointer[router.Policy]
	policyWG  sync.WaitGroup
	// cachedCatalog is the tool-cache selection this gateway starts from. It
	// is retained so a governance change that lands BEFORE the first
	// downstream connects can re-aggregate the cold catalog without
	// pretending the live one is ready.
	cachedCatalog map[string][]mcp.ToolDef

	// store is the registry handle (nil when the registry could not be
	// opened at all — the gateway then serves unscoped from the tool cache,
	// docs/flows.md). snap is the APPLIED snapshot (adoption goes through
	// applier: read generation >= applied, canonical.md §5c #2); scopeRes
	// caches the effective-scope resolution over it. watcher is the local
	// fsnotify+poll channel (the daemon link is the second, faster channel;
	// both feed onRegistryChange).
	store    *registry.Store
	snap     atomic.Pointer[registry.Snapshot]
	applier  registry.Applier
	scopeRes *scope.CachedResolver
	watcher  *registry.Watcher
	watchWG  sync.WaitGroup
	redialWG sync.WaitGroup // the re-dial loop (redial.go)
	redial   redialParams   // its resolved ladder
	// The credential announcement plane (credwatch.go). credEpochs is nil in
	// an assembly with no vault wiring, which is what startCredWatch tests
	// before subscribing.
	credEpochs  *credEpochs
	credWatcher *secrets.CredWatcher
	credWG      sync.WaitGroup
	// reloadMu serializes onRegistryChange (watcher + daemon link may fire
	// concurrently; reload/diff/apply must not interleave).
	reloadMu sync.Mutex
	// ctl is the best-effort daemon control link (nil when the control
	// socket path cannot even be resolved). The data plane never depends
	// on it (docs/architecture.md §2).
	ctl *ctlLink
	// linkDone is closed when the ctl goroutine has fully exited; shutdown
	// waits on it so a late link log line can never race the caller's
	// post-Run cleanup (nil when the link never started).
	linkDone chan struct{}

	// lifeCtx spans from newGateway to shutdown; every downstream connect
	// and in-flight call derives from it.
	lifeCtx context.Context
	stop    context.CancelFunc

	nextReqID atomic.Int64 // ids for gateway→client reverse RPCs

	mu          sync.Mutex
	rt          *router.Router                // current catalog (cache-built until ready)
	cat         router.Catalog                // raw-name projection of rt (scope input)
	catGen      uint64                        // bumped on every rt swap; surface cache key
	surface     *discovery.Surface            // cached exposure snapshot for (catGen, scope hash)
	lastScope   *scope.EffectiveScope         // last scope pushed/listed (hash diff)
	specs       []downstream.Spec             // applied enabled downstreams
	ready       bool                          // live router built from real connections
	pending     int                           // downstreams still connecting
	servers     map[string]*downstream.Server // connected downstreams
	connErr     map[string]string             // last connect failure per server id
	dialing     map[string]struct{}           // servers with a dial in flight (redial.go)
	redialAt    map[string]time.Time          // when each failed server may be dialed again
	redialTries map[string]int                // rungs climbed, per failed server
	inflight    map[string]context.CancelFunc // upstream request id → cancel
	pendingRPC  map[string]chan *mcp.Response // reverse RPC id → reply channel
	clientCaps  mcp.ClientCapabilities
	initialized bool // upstream sent notifications/initialized

	handlers sync.WaitGroup // per-request tools/call handler goroutines
}

// newGateway resolves paths, sets up logging, loads the tool cache and the
// registry (failure tolerated), and prepares — but does not start — the
// downstream connections.
func newGateway(cfg Config) (*gateway, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("gateway: ClientID must not be empty")
	}
	if cfg.In == nil || cfg.Out == nil {
		return nil, fmt.Errorf("gateway: In and Out must not be nil")
	}
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = platform.Default()
	}
	log, logClose, err := buildLogger(cfg, resolver)
	if err != nil {
		return nil, err
	}
	// The pid is attached once, for the whole process: the log FILE is named
	// after the client, so every `agenthub connect --client <id>` of the same
	// client appends to the same file and a user normally has several running
	// at once (one per editor window). Without it two gateways' lines read as
	// one gateway doing impossible things — a server connecting and failing
	// at the same instant, a backoff ladder that appears to skip rungs.
	log = log.With(logx.FieldClient, cfg.ClientID, logx.FieldPID, os.Getpid())

	// The vault chain backs BOTH credential faces — ${SECRET_X} resolution
	// and the OAuth bearer — so it is built once and shared. An injected
	// Secrets marks a test assembly: it keeps its own resolver and gets no
	// OAuth source, so no test run reaches the real keyring through here.
	var epochs *credEpochs
	if cfg.Secrets == nil {
		dir, derr := secretsDir(resolver)
		if derr != nil {
			// Same failure direction as a missing resolver: a placeholder or a
			// bearer that cannot be resolved becomes a dial error, never a
			// silently unauthenticated request.
			log.Warn("secrets directory unresolved; downstream credentials unavailable", "error", derr)
			cfg.Secrets = secrets.NewChain(secrets.ChainConfig{}).Resolver()
		} else {
			chain := secrets.NewChain(secrets.ChainConfig{Dir: dir})
			cfg.Secrets = chain.Resolver()
			if cfg.Auth == nil {
				// The epochs are created here, ahead of the gateway struct,
				// because the TokenSource factory closes over them: the round
				// trippers must read the SAME counters credwatch.go bumps.
				epochs = newCredEpochs()
				cfg.Auth = vaultAuth(chain, dir, epochs, log)
			}
		}
	}

	lifeCtx, stop := context.WithCancel(context.Background())
	g := &gateway{
		cfg:         cfg,
		resolver:    resolver,
		log:         log,
		logClose:    logClose,
		fw:          mcp.NewFrameWriter(cfg.Out),
		guard:       discovery.NewSearchGuard(),
		cursors:     shaping.NewMemStore(0),
		owner:       shaping.Owner("stdio:" + cfg.ClientID),
		catGen:      1,
		redial:      newRedialParams(cfg.RedialBase),
		credEpochs:  epochs,
		lifeCtx:     lifeCtx,
		stop:        stop,
		servers:     make(map[string]*downstream.Server),
		connErr:     make(map[string]string),
		dialing:     make(map[string]struct{}),
		redialAt:    make(map[string]time.Time),
		redialTries: make(map[string]int),
		inflight:    make(map[string]context.CancelFunc),
		pendingRPC:  make(map[string]chan *mcp.Response),
	}
	g.roots = &clientRoots{g: g}
	g.savings = openSavings(resolver, log)
	// Before the pool: poolOptions closes over downstreamDeps, which reads
	// g.traces to build TraceFor.
	g.traces = newTraceLogs(resolver, log)
	g.pool = downstream.NewPool(g.poolOptions())

	// Tool cache first: it is what makes "answer before downstreams are
	// up" possible.
	if dir, err := resolver.CacheDir(); err != nil {
		log.Warn("tool cache unavailable", "error", err)
	} else {
		g.cache = newToolCache(filepath.Join(dir, toolCacheSubdir), log)
	}
	cached := map[string][]mcp.ToolDef{}
	if g.cache != nil {
		cached = g.cache.load()
	}

	// Tool governance state: the kill switch and the quarantine set feed
	// router.Policy on EVERY build below. Unlike the registry, a failure here
	// is NOT tolerated by widening — an unreadable deny set leaves the policy
	// unknown and the catalog empty (toolpolicy.go header).
	g.policySrc = openToolPolicySource(resolver, log)
	g.loadToolPolicyOnce(lifeCtx)

	// Registry: docs/flows.md — a load failure must not kill the gateway.
	specs, regOK := g.loadRegistry(resolver)
	g.specs = specs

	// Skills over MCP: opt-in governance switch, read from the snapshot the
	// registry load just applied (nil snapshot = no governance = off).
	g.syncSkills(resolver)

	// Call quotas (ratelimit.go): also read from the applied governance
	// document. A rule set this process cannot honour is FATAL — a
	// configuration that claims a quota must enforce it or refuse to start,
	// never degrade into silently unlimited.
	if err := g.initRateLimits(); err != nil {
		g.shutdown()
		return nil, err
	}

	// Control link: best-effort by design. Path resolution failure only
	// costs coordination features; the gateway still serves.
	if sock, serr := resolver.CtlSocketPath(); serr == nil {
		g.ctl = newCtlLink(g, sock, cfg.LinkRetry)
	} else {
		log.Warn("control socket unresolved; running without daemon link", "error", serr)
	}

	// Scope resolution exists exactly when the registry does: without a
	// store there is no scope config to enforce, and the gateway serves
	// unscoped from the cache (the pre-M1 baseline; the pipeline scope gate
	// treats a nil provider as that same no-authority mode).
	var scopeFn func() *scope.EffectiveScope
	if g.store != nil {
		g.scopeRes = scope.NewCachedResolver(scope.Sources{
			Registry: func() *registry.Snapshot { return g.snap.Load() },
			Catalog:  g.catalogSnapshot,
			Extra: func(scope.SessionID) []scope.ScopeLayer {
				if cfg.ScopeLayers == nil {
					return nil
				}
				return cfg.ScopeLayers()
			},
		})
		scopeFn = g.currentScope
	}

	// The pipeline reads the SAME effective-scope pointer the tools/list
	// projection uses (docs/architecture.md §7: one visibility source, two readers).
	g.pipe = pipeline.New(pipeline.Options{
		Scope:           scopeFn,
		Scanner:         injection.NewDefault(),
		InjectionPolicy: g.injectionPolicy,
		// Shaping runs inside the pipeline, not around it: every execute path
		// (direct tools/call, lazy call_tool) is budgeted by the same rule
		// because there is only one place it is applied.
		ResultShaper: g.shapeResult,
	})

	// Cache-built catalog: with a healthy registry, serve only the cached
	// tools of currently enabled servers; with a broken registry we cannot
	// know what is enabled, so serve everything cached ("answer first with whatever can be answered").
	if regOK {
		enabled := make(map[string][]mcp.ToolDef, len(specs))
		for _, spec := range specs {
			if tools, ok := cached[spec.ID]; ok {
				enabled[spec.ID] = tools
			}
		}
		cached = enabled
	}
	g.cachedCatalog = cached
	rt := g.buildColdCatalog()
	g.rt = rt
	g.cat = catalogFromRouter(rt)
	g.pending = len(specs)
	// Claim every startup dial up front rather than inside connectAll: the
	// connect goroutines are spawned from run(), so between here and there
	// the re-dial loop must already see these servers as busy, not as
	// untouched entries it may dial itself.
	for _, spec := range specs {
		g.dialing[spec.ID] = struct{}{}
	}
	if g.scopeRes != nil {
		// Seed the hash-diff baseline so the first overlay/registry event
		// does not push a spurious tools/list_changed when nothing visible
		// actually changed.
		g.lastScope = g.currentScope()
	}
	return g, nil
}

// buildLogger returns cfg.Log or assembles the production text+JSON pair.
// The JSON file failure is downgraded to text-only: a gateway that cannot
// write its log file must still serve.
func buildLogger(cfg Config, resolver *platform.Resolver) (*slog.Logger, func() error, error) {
	if cfg.Log != nil {
		return cfg.Log, func() error { return nil }, nil
	}
	lc := logx.Config{TextEnabled: true, TextWriter: cfg.LogWriter}
	if dir, err := resolver.LogsDir(); err == nil {
		if err := platform.EnsureDir(dir); err == nil {
			lc.JSONPath = filepath.Join(dir, "gateway-"+fsSafe(cfg.ClientID)+".log")
		}
	}
	log, closeFn, err := logx.Setup(lc)
	if err != nil {
		// Fall back to text-only rather than refusing to start.
		log, closeFn, err = logx.Setup(logx.Config{TextEnabled: true, TextWriter: cfg.LogWriter})
		if err != nil {
			return nil, nil, fmt.Errorf("gateway: logger setup: %w", err)
		}
	}
	return log, closeFn, nil
}

// openSavings opens the savings stream (<data>/logs/savings.jsonl). Every
// failure degrades to nil — a gateway that cannot write its accounting
// records must still serve tools.
func openSavings(resolver *platform.Resolver, log *slog.Logger) *audit.SavingsStream {
	dir, err := resolver.LogsDir()
	if err == nil {
		err = platform.EnsureDir(dir)
	}
	if err == nil {
		var st *audit.SavingsStream
		if st, err = audit.NewSavingsStream(filepath.Join(dir, audit.SavingsFileName), audit.WriterOptions{}); err == nil {
			return st
		}
	}
	log.Warn("savings stream unavailable; token savings will not be recorded", "error", err)
	return nil
}

// loadRegistry opens the registry, applies its first snapshot and extracts
// the enabled downstream specs. Any failure — including quarantined
// documents — returns regOK=false so the caller falls back to the full
// tool cache; the gateway keeps running either way.
func (g *gateway) loadRegistry(resolver *platform.Resolver) (specs []downstream.Spec, regOK bool) {
	dir, err := resolver.RegistryDir()
	if err != nil {
		g.log.Warn("registry dir unresolved; starting with empty config", "error", err)
		return nil, false
	}
	store, err := registry.Open(dir)
	if store == nil {
		g.log.Warn("registry load failed; starting with empty config, serving from tool cache", "error", err)
		return nil, false
	}
	if err != nil {
		// Quarantine-healed documents: the store is usable but its content
		// is not trustworthy as "the user's real config" — answer from the
		// full cache while still connecting whatever the healed registry
		// lists.
		g.log.Warn("registry loaded with quarantined documents; serving from tool cache", "error", err)
	}
	g.store = store
	snap := store.Snapshot()
	g.snap.Store(snap)
	// Before connectAll, so a server whose entry says trace records its
	// handshake too — the frames of a connection that fails to come up are
	// the ones worth having.
	g.traces.apply(snap)
	g.applier.MarkApplied(snap.Generation)
	specs = g.specsFromSnapshot(snap)
	g.log.Info("registry loaded", logx.Rev(snap.Generation), "enabled_servers", len(specs))
	return specs, err == nil
}

// run starts the background downstream connections and serves the upstream
// read loop until EOF / exit / a fatal read error.
func (g *gateway) run(ctx context.Context) error {
	// Propagate caller cancellation into the gateway lifetime.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			g.stop()
		case <-watchDone:
		case <-g.lifeCtx.Done():
		}
	}()

	go g.connectAll()
	g.startWatch()
	g.startPolicyWatch()
	g.startRedial()
	g.startCredWatch()
	if g.ctl != nil {
		g.linkDone = make(chan struct{})
		go func() {
			defer close(g.linkDone)
			g.ctl.run(g.lifeCtx)
		}()
	}

	fr := mcp.NewFrameReader(g.cfg.In)
	for {
		line, err := fr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				g.log.Info("upstream closed the connection")
				return nil
			}
			if g.lifeCtx.Err() != nil {
				return nil // cancelled while blocked in read
			}
			return fmt.Errorf("gateway: upstream read: %w", err)
		}
		msg, perr := mcp.ParseMessage(line)
		if perr != nil {
			g.log.Warn("malformed upstream frame ignored", "error", perr)
			g.reply(mcp.NewErrorResponse(mcp.ID{}, &mcp.Error{
				Code: mcp.CodeParseError, Message: perr.Error(),
			}))
			continue
		}
		switch m := msg.(type) {
		case *mcp.Request:
			g.handleRequest(m)
		case *mcp.Notification:
			if exit := g.handleNotification(m); exit {
				g.log.Info("upstream requested exit")
				return nil
			}
		case *mcp.Response:
			g.deliverRPC(m)
		}
	}
}

// shutdown tears everything down: cancel in-flight work and downstream
// connects, wait for handler goroutines, close downstream servers, release
// the logger. Idempotent by construction (stop and Close are).
func (g *gateway) shutdown() {
	g.stop()
	// Before anything else that can block: no connect goroutine may write a
	// tool cache entry once this call returns. Those goroutines are not
	// joined here (toolCache.seal explains why joining them would be the
	// worse trade), so the guarantee has to come from the resource.
	if g.cache != nil {
		g.cache.seal()
	}
	g.handlers.Wait()
	if g.watcher != nil {
		g.watcher.Close()
		g.watchWG.Wait()
	}
	g.policyWG.Wait() // ends on lifeCtx cancellation (g.stop above)
	g.redialWG.Wait() // same
	if g.credWatcher != nil {
		g.credWatcher.Close() // closes Events, which ends the subscriber
		g.credWG.Wait()
	}
	if g.linkDone != nil {
		<-g.linkDone
	}
	if g.pool != nil {
		g.pool.Close() // derived instances first: they are extra connections
	}
	g.mu.Lock()
	servers := make([]*downstream.Server, 0, len(g.servers))
	for _, s := range g.servers {
		servers = append(servers, s)
	}
	g.mu.Unlock()
	for _, s := range servers {
		s.Close()
	}
	if g.savings != nil {
		_ = g.savings.Close() // flushes queued records
	}
	// After the connections: a server closing may still emit frames, and a
	// trace that stops one step early loses exactly the shutdown it was
	// opened to explain.
	g.traces.close()
	if g.logClose != nil {
		_ = g.logClose()
	}
}

// reply writes one frame upstream; write failures are logged, not fatal to
// the caller (a dead pipe surfaces as EOF in the read loop).
func (g *gateway) reply(msg any) {
	if err := g.fw.WriteFrame(msg); err != nil {
		g.log.Warn("upstream write failed", "error", err)
	}
}

// catalog returns the current router plus readiness flags under one lock
// acquisition.
func (g *gateway) catalog() (rt *router.Router, ready bool, pending int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rt, g.ready, g.pending
}

// fsSafe rewrites every rune outside [a-zA-Z0-9_-] to '_' for use in file
// names (log file, tool cache entries).
func fsSafe(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
