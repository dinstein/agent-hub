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
//  4. serve until ctx is done (the CLI cancels it on SIGTERM/SIGINT);
//  5. graceful stop: close the listener (no new connections), drain
//     in-flight requests for ShutdownGrace, then force-close stragglers
//     (long-lived SSE links never drain by themselves), remove daemon.json.
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
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/httpbridge"
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
	// loopback authorization server (self-hosted providers, tests). Off by
	// default: an OAuth endpoint on 127.0.0.1 is normally a
	// misconfiguration or an SSRF probe.
	OAuthAllowLoopback bool
	// RefreshScanInterval overrides how often the refresh coordinator
	// rescans for newly stored credentials (0 = 60s). Tests shrink it.
	RefreshScanInterval time.Duration
	// ClientBaseDir is what project-level AI-client configurations are
	// resolved against for /v1/clients ("" = the daemon's working
	// directory). The daemon is normally started from the user's shell, so
	// its cwd is the project they are in.
	ClientBaseDir string

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
		nonReg, tokens = nonRegistryDeps(cfg, dataDir, cfg.Secrets, log, coordRef.Load)
	}

	srv, err := ctlapi.NewServer(ctlapi.Options{
		Version:           cfg.Version,
		Registry:          store,
		Sessions:          mgr,
		Bus:               bus,
		States:            states,
		ServerReports:     states,
		Logger:            log,
		LinkAttachTimeout: cfg.LinkAttachTimeout,
		NonRegistry:       nonReg,
		// Tool governance, quarantine and the audit readers are gated on
		// these directories being known: an empty one silently unregisters
		// the routes, which is indistinguishable from an unimplemented
		// feature to whoever is looking at the GUI.
		StateDir: stateDir,
		LogsDir:  logsDir,
		// Deleting a server must strip its whole footprint here exactly as it
		// does from the CLI; these are the stores that outlive the registry.
		ServerStateForgetters: serverStateForgetters(stateDir, resolver),
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
	info := Info{Endpoint: socket, Pid: os.Getpid(), Version: cfg.Version}
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

	watcher := startWatch(bgCtx, store, cfg.Watch, bus, log)

	// OAuth proactive refresh (docs/modules/oauth.md): the daemon is the component
	// that is always up, so it is the one that renews tokens before they
	// expire. Gateways keep the 401/403 passive path as the safety net for
	// the windows when no daemon is running.
	if dataErr != nil {
		log.Warn("data dir unresolved; running without proactive token refresh", "error", dataErr)
	} else {
		refr := startRefresher(bgCtx, cfg, store, dataDir, log)
		// A control-plane refresh joins the daemon's singleflight instead of
		// racing it: a one-time refresh token spent twice cannot be undone.
		srv.SetRefresher(refr.Coordinator())
		// Same coordinator, same reason, for the self-test endpoint's
		// late-bound accessor (see coordRef above).
		coordRef.Store(refr.Coordinator())
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(l) }()

	// Data plane (opt-in; see Config.HTTPAddr — nothing listens by default).
	// It starts AFTER the control socket is serving because every gateway it
	// assembles registers over that socket: session listing and overlays
	// then work for an HTTP caller exactly as they do for an stdio one.
	endpoint, herr := startHTTPPlane(bgCtx, cfg, httpPlaneDeps{
		Resolver: resolver,
		Log:      log,
		Version:  cfg.Version,
		Registry: store,
		Secrets:  dataPlaneSecrets(cfg.Secrets),
		// The bearer half, chosen from the SAME vault as Secrets above: a
		// dial must not resolve its placeholders against one vault and its
		// bearer against another. Both nil (no vault configured) is the one
		// case where the gateway builds the pair itself.
		//
		// Secrets alone is not enough — it resolves ${SECRET_X} placeholders
		// written into a spec, while an OAuth server never appears in one.
		Auth: planeAuth(cfg.Secrets, coordRef.Load),
		Dial: cfg.Dial,
	}, tokens, store.Snapshot(), log)
	if herr != nil {
		if watcher != nil {
			watcher.Close()
		}
		bgStop()
		_ = srv.Close()
		<-serveErr
		_ = os.Remove(socket)
		_ = os.Remove(infoPath)
		return herr
	}
	if endpoint != nil && cfg.OnHTTPReady != nil {
		cfg.OnHTTPReady(endpoint.Addrs())
	}

	log.Info("daemon ready", "socket", socket, "pid", info.Pid, "version", cfg.Version)
	if cfg.OnReady != nil {
		cfg.OnReady(info)
	}

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

	select {
	case <-ctx.Done():
		// Graceful stop: stop accepting, drain in-flight requests, then
		// force-close whatever is left (SSE links never drain on their own).
		log.Info("daemon stopping", "reason", context.Cause(ctx))
		// The data plane goes FIRST, before the control-plane drain: each of
		// its gateways holds a control link on this very socket, and those
		// links are exactly the connections that never drain by themselves.
		// Closing them here turns the drain below into a short one instead of
		// making every shutdown spend the full grace period. (Close is
		// idempotent; cleanup calls it again on the other branch.)
		endpoint.Close()
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
	if cfg.Log != nil {
		return cfg.Log, func() error { return nil }, nil
	}
	lc := logx.Config{
		TextEnabled: true,
		TextWriter:  cfg.LogWriter,
		JSONPath:    filepath.Join(logsDir, LogFileName),
	}
	log, closeFn, err := logx.Setup(lc)
	if err != nil {
		log, closeFn, err = logx.Setup(logx.Config{TextEnabled: true, TextWriter: cfg.LogWriter})
		if err != nil {
			return nil, nil, fmt.Errorf("daemon: logger setup: %w", err)
		}
	}
	return log, closeFn, nil
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
func startWatch(ctx context.Context, store *registry.Store, opts registry.WatchOptions, bus *event.Bus, log *slog.Logger) *registry.Watcher {
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
				log.Warn("registry reload failed; keeping previous snapshot",
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
			log.Info("registry change applied",
				"kind", string(ch.Kind), "generation", snap.Generation)
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
