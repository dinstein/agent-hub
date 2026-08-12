// Package daemon assembles the coordination daemon (docs/modules/controlplane.md:
// assembly only, no business logic of its own): event bus +
// session manager + control-plane server (internal/ctlapi) + registry watch
// + the gateway-reported runtime state aggregator, plus the run/daemon.json
// readiness handshake.
//
// Lifecycle (docs/architecture.md §2):
//
//  1. resolve paths, open logging and the registry;
//  2. bind the control socket (ctlapi.Listen — fails with ErrAlreadyRunning
//     when a live daemon owns it);
//  3. atomically write run/daemon.json (endpoint + pid + version, 0600) —
//     only AFTER a successful bind, so a well-formed daemon.json always
//     describes an endpoint that was live at write time (no TOCTOU probe);
//  4. serve until ctx is done — the CLI cancels it on SIGTERM/SIGINT, and
//     for an owned daemon so does the owner watch (owner.go), which ends the
//     run when the application this daemon belongs to disappears;
//  5. graceful stop: end the long-lived SSE streams (they never drain by
//     themselves, and Shutdown cannot interrupt them), close the listener
//     (no new connections), drain in-flight requests for ShutdownGrace,
//     force-close any straggler, remove daemon.json.
//
// stdio gateways depend on NONE of this: the daemon dying (even kill -9,
// A.3 #2) only loses coordination — session listing, dynamic overlays,
// centralized refresh. Gateways fall back to their static scope and
// re-register with backoff when the daemon returns.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/diag"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/jsonl"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
	"github.com/dinstein/agent-hub/internal/session"
)

// Frozen file names under the run/logs directories.
const (
	// InfoFileName is the readiness handshake file in the run directory.
	InfoFileName = "daemon.json"
	// LogFileName is the daemon's JSON log file in the logs directory
	// (`agenthub daemon logs` reads it).
	LogFileName = "daemon.log"
)

// DefaultShutdownGrace bounds the drain phase of a graceful stop.
const DefaultShutdownGrace = 5 * time.Second

// Info mirrors run/daemon.json: the actual endpoint the daemon serves on,
// its pid and version. CLI/GUI/gateways read it to connect
// (docs/architecture.md §10; api.DialOrStart holds the reader-side copy of
// this shape).
type Info struct {
	Endpoint string `json:"endpoint"`
	Pid      int    `json:"pid"`
	Version  string `json:"version"`
	// Owner is the pid of the application this daemon belongs to, or 0 for a
	// headless one. It is written for diagnosis only — the authoritative
	// answer to "who owns the running daemon" comes from a ping, for the
	// reason `daemon stop` reads its pid from there too: this file names a
	// process, it does not identify one, and it outlives an abrupt death.
	Owner int `json:"owner,omitempty"`
}

// Config configures one daemon process. Everything has production defaults;
// fields exist for the CLI and tests.
type Config struct {
	// Version is reported by /v1/ping and written into daemon.json.
	Version string
	// Resolver overrides platform path resolution (nil = real environment).
	Resolver *platform.Resolver
	// Log overrides the logger. nil builds the production pair: text to
	// LogWriter (default os.Stderr) + JSON to <data>/logs/daemon.log.
	Log *slog.Logger
	// LogWriter receives the human-readable text log when Log is nil.
	LogWriter io.Writer
	// OnReady is called once the daemon is accepting connections and
	// daemon.json is written (foreground CLI prints readiness; tests sync).
	OnReady func(Info)
	// ShutdownGrace bounds the drain phase (0 = DefaultShutdownGrace).
	ShutdownGrace time.Duration
	// Watch overrides registry watch timing (zero value = production
	// defaults). Tests shrink debounce/poll.
	Watch registry.WatchOptions
	// LinkAttachTimeout passes through to ctlapi.Options (0 = ctlapi
	// default). Tests shrink it.
	LinkAttachTimeout time.Duration
	// Secrets overrides the credential vault used by the OAuth refresh
	// coordinator. nil builds the real chain over <data>/secrets; tests
	// inject an in-memory store so no test ever touches the OS keyring.
	Secrets secrets.Store
	// OAuthAllowLoopback lets the refresh coordinator talk to a literal
	// loopback authorization server for EVERY server, whatever the registry
	// says. Off by default: an OAuth endpoint on 127.0.0.1 is normally a
	// misconfiguration or an SSRF probe.
	//
	// It is a blanket override kept for tests. The ordinary route is per
	// server and needs nothing here: `server add --local` records
	// provenance, and the refresher reads it per refresh (oauth.go,
	// allowsLoopback).
	OAuthAllowLoopback bool
	// RefreshScanInterval overrides how often the refresh coordinator
	// rescans for newly stored credentials (0 = 60s). Tests shrink it.
	RefreshScanInterval time.Duration
	// ClientBaseDir is what project-level AI-client configurations are
	// resolved against for /v1/clients ("" = the daemon's working
	// directory). The daemon is normally started from the user's shell, so
	// its cwd is the project they are in.
	ClientBaseDir string

	// Owner is the application process this daemon belongs to and must not
	// outlive (owner.go). The zero value is a headless daemon: nobody owns
	// it, nothing watches, and only an operator stops it.
	Owner Owner
	// OwnerPollInterval overrides how often the owner watch re-asks whether
	// the owner is alive (0 = DefaultOwnerPollInterval). Tests shrink it.
	OwnerPollInterval time.Duration

	// HTTPAddr is the MCP data plane's listen address ("host:port").
	//
	// EMPTY IS THE DEFAULT AND MEANS NO LISTENER AT ALL — not "a default
	// port". Binding a network socket because nobody said otherwise is a
	// decision only the operator may make, and its failure mode is silent.
	// See startHTTPPlane for the two other fail-closed properties (explicit
	// confirmation for non-loopback, AuthorizeBind for the credential-less
	// endpoint).
	HTTPAddr string
	// HTTPAllowRemote is the explicit confirmation a non-loopback HTTPAddr
	// requires. Without it a non-loopback address FAILS the daemon rather
	// than silently downgrading to loopback.
	HTTPAllowRemote bool
	// HTTPInsecureLoopback is httpbridge's documented escape hatch: accept
	// unauthenticated loopback callers. It never covers a non-loopback bind.
	HTTPInsecureLoopback bool
	// HTTPAdminToken is the operator's own bearer for the data plane
	// (AGENTHUB_HTTP_TOKEN). Empty = no admin token is configured.
	HTTPAdminToken string
	// OnHTTPReady reports the addresses the data plane actually bound (port 0
	// resolves before this fires). Tests use it; nothing else needs it.
	OnHTTPReady func(addrs []string)
	// Dial overrides downstream transport creation for the HTTP data plane's
	// gateways (tests). nil spawns real children, like `agenthub connect`.
	Dial downstream.DialFunc
}

// Run assembles and runs the daemon until ctx is done or a fatal serve
// error occurs. It returns nil after a clean shutdown. A second daemon on
// the same socket fails fast with ctlapi.ErrAlreadyRunning.
func Run(ctx context.Context, cfg Config) error {
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = platform.Default()
	}
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = DefaultShutdownGrace
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}

	socket, err := resolver.CtlSocketPath()
	if err != nil {
		return fmt.Errorf("daemon: resolve control socket: %w", err)
	}
	runDir, err := resolver.RunDir()
	if err != nil {
		return fmt.Errorf("daemon: resolve run dir: %w", err)
	}
	logsDir, err := resolver.LogsDir()
	if err != nil {
		return fmt.Errorf("daemon: resolve logs dir: %w", err)
	}
	regDir, err := resolver.RegistryDir()
	if err != nil {
		return fmt.Errorf("daemon: resolve registry dir: %w", err)
	}
	if err := platform.EnsureDirs(runDir, logsDir); err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	log, logClose, err := buildLogger(cfg, logsDir)
	if err != nil {
		return err
	}
	defer func() { _ = logClose() }()

	// Opt-in profiling (internal/diag), armed as soon as there is a logger to
	// announce it through and before any of the slow startup below, so that a
	// daemon which never finishes starting is still one that can be profiled.
	// A refusal is fatal, on the same reasoning as the MCP listener's: an
	// address that cannot be served safely is refused, never downgraded.
	prof, err := diag.ServeFromEnv(log)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	defer func() { _ = prof.Close() }()

	// The owner watch (owner.go) is what makes this daemon's lifetime the
	// owning application's. It is armed here, before the registry is opened
	// and long before the socket is bound, because an owner that dies DURING
	// a slow startup is exactly the case that used to strand a daemon: the
	// application is gone by the time there is anything to send a signal to.
	// Cancelling ctx routes into the same graceful stop a SIGTERM takes, and
	// the cause is what the shutdown log reports as the reason.
	ctx, ownerLost := context.WithCancelCause(ctx)
	defer ownerLost(nil)
	ownerWatch := watchOwner(cfg.Owner, cfg.OwnerPollInterval, log, ownerLost)
	defer ownerWatch.Close()

	// Registry: unlike the gateway (which serves a data plane and tolerates
	// a broken registry), the daemon IS the coordination plane — an
	// unopenable registry is fatal. Healed quarantines are only warnings.
	store, oerr := registry.Open(regDir)
	if store == nil {
		return fmt.Errorf("daemon: open registry: %w", oerr)
	}
	if oerr != nil {
		log.Warn("registry opened with quarantined documents", "error", oerr)
	}

	bus := event.NewBus()
	mgr := session.NewMemoryManager(session.Options{Bus: bus})

	// <data>/state backs the tool-governance stores the control plane reads
	// and writes. An unresolvable state dir is not fatal: the endpoints that
	// need it answer 404 rather than the daemon refusing to start.
	stateDir, serr := resolver.StateDir()
	if serr != nil {
		log.Warn("state dir unresolved; tool governance endpoints unavailable", "error", serr)
		stateDir = ""
	}

	// Runtime state source: ONE aggregator wired as both the sink the
	// gateway-report endpoint writes into and the source /v1/servers reads
	// out of. The daemon holds no downstream connections of its own (it has
	// no data plane until httpbridge is assembled), so the processes that do
	// — the stdio gateways — are the ones that report; see the file comment
	// on internal/ctlapi/gatewaystate.go for why the daemon does not dial
	// its own probe connections instead.
	states := ctlapi.NewGatewayStates()

	// The control-plane event stream (internal/eventlog), opened before every
	// collaborator that records into it — the watcher, the login manager, the
	// HTTP face's session table. Fail-open: a daemon that cannot write its
	// events still coordinates, so each failure degrades to a nil stream,
	// which is silent by contract.
	events := openEvents(logsDir, log, store)
	defer func() { _ = events.Close() }()

	// Non-registry collaborators (credentials, skills, tokens, client
	// adapters, OAuth status). Built before the server because the server
	// takes them by value: routes without dependencies answer "not served",
	// which is indistinguishable from a missing feature to whoever is
	// looking at the GUI.
	var nonReg ctlapi.NonRegistryDeps
	var tokens *httpbridge.Store
	dataDir, dataErr := resolver.DataDir()
	// coordRef is filled once the refresher is built, further down. The
	// self-test endpoint needs the coordinator to renew a token mid-probe,
	// but the control plane is assembled first, so the dependency is handed
	// over as an accessor rather than a value that would still be nil here.
	var coordRef atomic.Pointer[oauthflow.Coordinator]
	if dataErr != nil {
		log.Warn("data dir unresolved; the non-registry endpoints stay off", "error", dataErr)
	} else {
		nonReg, tokens = nonRegistryDeps(cfg, dataDir, cfg.Secrets, log, events, coordRef.Load)
	}

	// The credential half of /v1/servers, filled by the refresher below. It
	// is built here rather than there so the control plane needs no late
	// binding: the refresher starts after the server, and a holder that is
	// simply empty until it does is indistinguishable from one nobody feeds.
	tokenStates := ctlapi.NewTokenStates()

	srv, err := ctlapi.NewServer(ctlapi.Options{
		Version: cfg.Version,
		Owner:   cfg.Owner.PID,
		// The same handle the owner watch pulls. A stop asked for over the
		// socket and an owner that died then take one code path, and the
		// shutdown log reports each one's reason the same way.
		RequestStop:   func(reason string) { ownerLost(errors.New(reason)) },
		Registry:      store,
		Sessions:      mgr,
		Bus:           bus,
		States:        states,
		ServerReports: states,
		// Empty until the refresher publishes its first scan, and empty
		// forever if the data dir could not be resolved — which reads as
		// "nothing is known about any credential", the answer this control
		// plane gave before there was a producer at all.
		TokenStates:       tokenStates,
		Logger:            log,
		LinkAttachTimeout: cfg.LinkAttachTimeout,
		NonRegistry:       nonReg,
		// Tool governance and quarantine are gated on this directory being
		// known: an empty one silently unregisters the routes, which is
		// indistinguishable from an unimplemented feature to whoever is
		// looking at the GUI.
		StateDir: stateDir,
		// Deleting a server must strip its whole footprint here exactly as it
		// does from the CLI; these are the stores that outlive the registry.
		ServerStateForgetters: serverStateForgetters(resolver),
	})
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	l, err := ctlapi.Listen(socket)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	// Crash marker, armed only AFTER the bind above succeeded: a second
	// daemon that lost the race for the socket must not clobber the winner's
	// marker. Resolve happens on the graceful path alone — a process that is
	// SIGKILLed, panics or loses power simply never resolves, and that
	// absence is the signal.
	//
	// Until this call existed the feature was half-wired: `agenthub doctor`
	// read the marker through registry.PreviousShutdown, but nothing in the
	// product ever armed one, so the answer was permanently "unknown (no
	// marker yet)" — including immediately after a crash, which is the one
	// moment it exists to report.
	//
	// Failure direction: a marker that cannot be written costs the NEXT start
	// its diagnosis and nothing else, so it degrades to a warning. Refusing to
	// serve over a diagnostic would be the wrong trade.
	marker, prevShutdown, merr := registry.ArmRunMarker(regDir)
	if merr != nil {
		log.Warn("could not arm the crash marker; the next start cannot report how this run ended",
			"error", merr)
	} else if prevShutdown == registry.ShutdownCrash {
		log.Warn("the previous run did not shut down cleanly")
	}

	// Readiness handshake: written atomically only after the successful
	// bind above (docs/architecture.md §10 — replaces probe-then-spawn TOCTOU).
	info := Info{Endpoint: socket, Pid: os.Getpid(), Version: cfg.Version, Owner: cfg.Owner.PID}
	infoPath := filepath.Join(runDir, InfoFileName)
	if err := writeInfoFile(infoPath, info); err != nil {
		_ = l.Close()
		return fmt.Errorf("daemon: %w", err)
	}

	// Internal goroutines (reaper, watch pump) live on their own context so
	// they keep running through the drain phase and stop at cleanup.
	bgCtx, bgStop := context.WithCancel(context.Background())
	defer bgStop()
	go mgr.Run(bgCtx)

	watcher := startWatch(bgCtx, store, cfg.Watch, bus, log, events)

	// OAuth proactive refresh (docs/modules/oauth.md): the daemon is the component
	// that is always up, so it is the one that renews tokens before they
	// expire. Gateways keep the 401/403 passive path as the safety net for
	// the windows when no daemon is running.
	if dataErr != nil {
		log.Warn("data dir unresolved; running without proactive token refresh", "error", dataErr)
	} else {
		refr := startRefresher(bgCtx, cfg, store, dataDir, tokenStates, events, log)
		// A control-plane refresh joins the daemon's singleflight instead of
		// racing it: a one-time refresh token spent twice cannot be undone.
		srv.SetRefresher(refr.Coordinator())
		// Same coordinator, same reason, for the self-test endpoint's
		// late-bound accessor (see coordRef above).
		coordRef.Store(refr.Coordinator())
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(l) }()

	// Teardown, shared by every exit below — a clean stop, a serve failure,
	// and a data plane that could not come up. It is defined before the
	// endpoint it closes because that last path needs it too: when the two
	// were written out separately, the startup-failure copy removed the run
	// files with no ownership check at all.
	var endpoint *httpEndpoint
	cleanup := func() {
		// The data plane goes first: it holds downstream processes, and they
		// must be released before the coordination state they report into.
		endpoint.Close()
		if watcher != nil {
			watcher.Close()
		}
		bgStop()
		// Both paths are SHARED, and by the time this runs they may belong
		// to somebody else. Shutdown closes the listener before it drains,
		// so for up to ShutdownGrace a replacement daemon can start, find
		// no stale socket, bind, and write its own daemon.json — and the
		// removes below would then unlink a LIVE control socket and delete
		// a readiness file that had nothing to do with this process. The
		// replacement keeps running with its registry watch and refresher,
		// unreachable and invisible, and the next `daemon start` binds a
		// fresh socket beside it.
		//
		// daemon.json still naming this pid is the proof of ownership, and
		// it covers the socket too: a replacement writes the file as part
		// of coming up, so a foreign pid means the paths are no longer
		// ours. Anything else — unreadable, missing, another pid — leaves
		// both alone. The listener close already unlinked the socket on
		// every ordinary path; this remove was only ever a backstop.
		if !ownsRunFiles(runDir) {
			log.Info("run directory no longer belongs to this daemon; leaving the socket and daemon.json alone",
				"socket", socket, "info", infoPath)
			return
		}
		_ = os.Remove(socket)
		if err := os.Remove(infoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("removing daemon.json failed", "error", err)
		}
	}

	// Data plane (opt-in; see Config.HTTPAddr — nothing listens by default).
	// It starts AFTER the control socket is serving because every gateway it
	// assembles registers over that socket: session listing and overlays
	// then work for an HTTP caller exactly as they do for an stdio one.
	// The credential collaborators are deliberately absent from this literal:
	// a gateway builds its own vault chain exactly when it is handed neither,
	// and httpPlaneDeps carries the argument for what that chain does and what
	// injecting either field silently costs.
	endpoint, herr := startHTTPPlane(bgCtx, cfg, httpPlaneDeps{
		Resolver: resolver,
		Log:      log,
		Events:   events,
		Version:  cfg.Version,
		Registry: store,
		Dial:     cfg.Dial,
	}, tokens, store.Snapshot())
	if herr != nil {
		// Same teardown as every other exit, in the same order: stop serving,
		// then release what was assembled. Nothing is draining here — the
		// daemon never announced itself — so the force-close is the whole of
		// phase one.
		_ = srv.Close()
		<-serveErr
		cleanup()
		return herr
	}
	if endpoint != nil && cfg.OnHTTPReady != nil {
		cfg.OnHTTPReady(endpoint.Addrs())
	}

	// "owner" is on the line on every start, including the headless 0. A hub
	// that stopped on its own is diagnosed by asking who it was watching, and
	// a field that only appears when there is an owner makes its absence
	// unreadable: nothing in the log then distinguishes "headless" from "this
	// build predates the owner watch".
	events.Emit(log, eventlog.Record{
		Scope: eventlog.ScopeDaemon, Kind: eventlog.KindDaemonStarted,
		Detail: cfg.Version,
	}, "daemon ready", "socket", socket, logx.PID(), "version", cfg.Version, "owner", cfg.Owner.PID)
	if endpoint != nil {
		for _, addr := range endpoint.Addrs() {
			events.Emit(log, eventlog.Record{
				Scope: eventlog.ScopeDaemon, Kind: eventlog.KindListenerBound,
				Detail: addr,
			}, "data plane listening", "addr", addr)
		}
	}

	if cfg.OnReady != nil {
		cfg.OnReady(info)
	}

	select {
	case <-ctx.Done():
		// Graceful stop: end the long-lived streams, stop accepting, drain
		// in-flight requests, then force-close whatever is still left.
		var reason string
		if cause := context.Cause(ctx); cause != nil {
			reason = cause.Error()
		}
		events.Emit(log, eventlog.Record{
			Scope: eventlog.ScopeDaemon, Kind: eventlog.KindDaemonStopping,
			Detail: reason,
		}, "daemon stopping", "reason", reason)
		// The data plane goes first, for the reason cleanup states, and it
		// goes here rather than only in cleanup so that the downstream
		// processes are released before the drain rather than after it.
		// Close is idempotent; cleanup calls it again below.
		endpoint.Close()
		// Then the streams, and this call is what makes the drain a drain.
		// Every long-lived connection on the control socket — each stdio
		// gateway's link, plus whatever holds /v1/events — is parked until
		// its client hangs up, and Shutdown cannot interrupt them: it waits
		// for handlers to return and never cancels their request contexts.
		// Without it the drain below has nothing it can finish, so it spends
		// the full ShutdownGrace and then force-closes exactly the
		// connections it spent that period waiting for.
		//
		// This used to be attempted by closing the data plane, on the belief
		// that the gateway links hung off it. They do not: `endpoint` is the
		// MCP data plane (opt-in — nothing listens by default), while the
		// links are served by `srv` on the control socket. Every measured
		// stop took the whole grace period, and the gateways logged `control
		// link stream ended` at the force-close rather than here.
		srv.CloseStreams()
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Warn("graceful drain incomplete; force-closing connections", "error", err)
			_ = srv.Close()
		}
		<-serveErr // Serve returned (nil after Shutdown/Close)
		cleanup()
		// The LAST step of a graceful stop, and only on this branch: the
		// serve-failure path below must leave the marker armed, because a run
		// that ended by failing did not end cleanly. Resolve is nil-safe, so
		// a daemon that could not arm one still shuts down normally.
		if err := marker.Resolve(); err != nil {
			log.Warn("could not resolve the crash marker; the next start will report this clean stop as a crash",
				"error", err)
		}
		log.Info("daemon stopped")
		return nil
	case err := <-serveErr:
		cleanup()
		if err != nil {
			log.Error("daemon serve failed", "error", err)
			return fmt.Errorf("daemon: serve: %w", err)
		}
		return nil
	}
}

// buildLogger returns cfg.Log or assembles the production text+JSON pair.
// A JSON file failure downgrades to text-only: a daemon that cannot write
// its log file should still coordinate.
func buildLogger(cfg Config, logsDir string) (*slog.Logger, func() error, error) {
	noop := func() error { return nil }
	if cfg.Log != nil {
		return cfg.Log, noop, nil
	}
	lc := logx.Config{TextEnabled: true, TextWriter: cfg.LogWriter}
	sink, err := jsonl.NewLineWriter(filepath.Join(logsDir, LogFileName),
		jsonl.WriterOptions{KeepSegments: jsonl.DefaultKeepSegments})
	if err != nil {
		return logx.Setup(lc), noop, nil
	}
	lc.JSON = sink
	return logx.Setup(lc), sink.Close, nil
}

// startWatch wires registry watching into the bus: on every external change
// the store is reloaded (the Change event is a notification, never a
// snapshot) and, per the Applier criterion (read generation >= applied,
// canonical.md §5c #2), the change is announced on the bus:
//
//   - ctlapi.TopicRegistry (payload registry.Change) → forwarded to every
//     gateway link as a `registry` SSE frame;
//   - "server.registry" for servers-document changes → drives the frontend
//     `servers` SSE topic (coalesced re-read in ctlapi).
//
// Watch failure degrades: the daemon still runs, changes surface on the
// next explicit reload. Self-writes are already suppressed inside registry.
func startWatch(ctx context.Context, store *registry.Store, opts registry.WatchOptions, bus *event.Bus, log *slog.Logger, events *eventlog.Stream) *registry.Watcher {
	w, err := store.WatchWith(opts)
	if err != nil {
		log.Warn("registry watch unavailable; external changes will not be noticed", "error", err)
		return nil
	}
	go func() {
		var applier registry.Applier
		for {
			var ch registry.Change
			var ok bool
			select {
			case <-ctx.Done():
				return
			case ch, ok = <-w.Events():
				if !ok {
					return
				}
			}
			snap, rerr := store.Reload(ctx)
			if snap == nil {
				// The counterpart of config_reloaded below, and the reason
				// the stream needs both: with only the success, a timeline
				// shows every reload that worked and none that stopped
				// working, so the last one that landed reads as the
				// configuration in force. It is not — this daemon keeps
				// serving the previous snapshot while every reader of the
				// file on disk sees the new one. Same kind and same reason
				// as a gateway's; see internal/gateway/hotreload.go.
				// rerr is non-nil here: every path in Reload that returns a
				// nil snapshot returns a joined error with it. Dereferenced
				// rather than guarded because a guard for a branch that
				// cannot be taken reads as one that can.
				events.Emit(log, eventlog.Record{
					Scope: eventlog.ScopeDaemon, Kind: eventlog.KindRegistryReloadFailed,
					Detail: rerr.Error(),
				}, "registry reload failed; keeping previous snapshot",
					"kind", string(ch.Kind), "error", rerr)
				continue
			}
			if rerr != nil {
				log.Warn("registry reloaded with quarantined documents", "error", rerr)
			}
			applied, _ := applier.Apply(snap.Generation, func() error { return nil })
			if !applied {
				continue
			}
			events.Emit(log, eventlog.Record{
				Scope: eventlog.ScopeDaemon, Kind: eventlog.KindConfigReloaded,
				Detail: string(ch.Kind), Rev: snap.Generation,
			}, "registry change applied", "kind", string(ch.Kind))
			bus.Publish(event.Event{
				Topic:   ctlapi.TopicRegistry,
				Key:     string(ch.Kind),
				Payload: registry.Change{Kind: ch.Kind, Rev: snap.Generation},
			})
			if ch.Kind == registry.DocServers {
				// Prefix "server." maps onto the coalesced `servers` SSE
				// topic; the payload is rebuilt server-side at fire time.
				bus.Publish(event.Event{Topic: "server.registry", Key: string(ch.Kind)})
			}
		}
	}()
	return w
}

// ownsRunFiles reports whether daemon.json at path still names THIS
// process, which is what makes the shared run-directory paths ours to
// delete.
//
// Failure direction: FAIL-CLOSED, in the sense that matters here — every
// doubt (unreadable, unparsable, missing, a different pid) answers false
// and leaves the files in place. Deleting a live daemon's socket is
// unrecoverable for that daemon; leaving a stale file behind costs the
// next start one cleanup pass, and removeStaleSocket already handles it.
func ownsRunFiles(runDir string) bool {
	info, err := ReadInfo(runDir)
	if err != nil {
		return false
	}
	return info.Pid == os.Getpid()
}

// writeInfoFile atomically writes daemon.json (0600): temp file in the same
// directory, fsync-free rename — readers see either the old file or the
// complete new one, never a torn write.
func writeInfoFile(path string, info Info) error {
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("encoding daemon.json: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, InfoFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing daemon.json: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod daemon.json: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing daemon.json: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing daemon.json: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("committing daemon.json: %w", err)
	}
	return nil
}

// ReadInfo loads run/daemon.json. The file is written atomically, so a
// well-formed read means the endpoint was live at write time. Callers must
// still ping before trusting it (the daemon may have been killed since).
func ReadInfo(runDir string) (Info, error) {
	var info Info
	b, err := os.ReadFile(filepath.Join(runDir, InfoFileName))
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, fmt.Errorf("daemon: parsing %s: %w", InfoFileName, err)
	}
	return info, nil
}

// openEvents opens the control-plane event stream when governance wants it.
//
// Fail-open in every direction: the switch unset means ON, an unreadable
// registry means ON (a stream is the thing you want when the config itself
// is in doubt), and a file that will not open means nil — which is silent by
// contract, so no caller distinguishes it from "switched off".
func openEvents(logsDir string, log *slog.Logger, store *registry.Store) *eventlog.Stream {
	if store != nil {
		if snap := store.Snapshot(); !snap.Governance.V.EventsEnabled() {
			return nil
		}
	}
	st, err := eventlog.Open(filepath.Join(logsDir, eventlog.FileName), eventlog.Options{})
	if err != nil {
		log.Warn("event stream unavailable; daemon state changes will not be recorded", "error", err)
		return nil
	}
	return st
}
