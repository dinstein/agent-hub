package ctlapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/session"
)

// Wire error codes originated by this server. api.ErrCodeBadResponse is the
// only client-synthesized code; everything else a client sees comes from
// this list.
const (
	// CodeNotFound is the single, uniform 404 code. Every unknown path,
	// unknown method AND unknown resource id returns the identical body
	// (anti-probing: a 404 must not reveal whether the route exists).
	CodeNotFound = "E_NOT_FOUND"
	// CodeBadRequest covers malformed bodies and query parameters.
	CodeBadRequest = "E_BAD_REQUEST"
	// CodeAPIVersion rejects an incompatible X-Agenthub-Api-Version.
	CodeAPIVersion = "E_API_VERSION"
	// CodeTightenOnly rejects a scope mutation that would widen (A.1 #8).
	CodeTightenOnly = "E_TIGHTEN_ONLY"
	// CodeConflict rejects a second attach on a single-shot gateway link.
	CodeConflict = "E_CONFLICT"
	// CodeInternal is the generic 500 (including recovered panics).
	CodeInternal = "E_INTERNAL"
)

// notFoundMessage is the frozen uniform 404 text (anti-probing: identical
// for every miss, asserted byte-for-byte by tests).
const notFoundMessage = "not found"

// HeaderActor is the request header carrying the control-plane actor
// identity for audit records: "cli", "gui" or "gateway:<sid>"
// (docs/architecture.md §2). Absent or unrecognized values fall back to "cli".
const HeaderActor = "X-Agenthub-Actor"

// ServerRuntime is the runtime state one server contributes on top of its
// registry entry. Produced by the injected ServerStateSource — in production
// *GatewayStates, folding the reports of every registered stdio gateway
// (gatewaystate.go); tests fake it.
type ServerRuntime struct {
	Conn       ConnState
	ConnDetail string
	// Tools is the size of the server's current tool catalog (0 when not
	// connected / not yet listed).
	Tools int
	// MissingSecrets lists unresolved secret names blocking the server.
	MissingSecrets []string
	// OAuthConfigError describes a broken OAuth configuration ("" = none).
	OAuthConfigError string
	// CallAuthFailed reports auth failures observed on tool calls.
	CallAuthFailed bool
	// Token is the OAuth token lifecycle state.
	Token TokenState
	// Quarantined reports the integrity subsystem has isolated the server;
	// it overrides the registry enabled flag in AdminState derivation.
	Quarantined bool
}

// ServerStateSource supplies runtime state for /v1/servers and the servers
// SSE payload. ok=false means the source has no runtime knowledge of the
// server (static registry entry only) — with the gateway-reported source
// that is the normal state of a server no connected client is using, not an
// error.
type ServerStateSource interface {
	ServerRuntime(id string) (ServerRuntime, bool)
}

// Options carries the Server's injected dependencies. Registry, Sessions
// and Bus are required; the rest defaults sensibly. The daemon assembles a
// full set in its own milestone task.
type Options struct {
	// Version is reported by /v1/ping ("dev" when empty).
	Version string
	// Registry is the configuration store (generation + servers document).
	Registry *registry.Store
	// Sessions is the live session registry.
	Sessions session.SessionManager
	// Bus is the daemon event bus feeding /v1/events.
	Bus *event.Bus
	// States supplies per-server runtime state (nil = static entries only).
	States ServerStateSource
	// ServerReports receives the gateway runtime reports that feed States.
	// nil disables POST /v1/gateway/{sid}/servers (uniform 404). The daemon
	// injects ONE *GatewayStates as both this and States; keeping the two
	// fields apart keeps the read side free of the writer's identity.
	ServerReports ServerStateSink
	// Logger receives server-side diagnostics (nil = discard).
	Logger *slog.Logger
	// CoalesceWindow overrides the servers-topic coalescing window
	// (0 = event.CoalesceWindow). Tests shrink it.
	CoalesceWindow time.Duration
	// SettleWindow overrides the settled-debounce window applied to
	// scan-style topics (0 = event.SettleWindow). Tests shrink it.
	SettleWindow time.Duration
	// KeepAlive is the SSE keep-alive comment interval (0 = 15s;
	// negative = disabled).
	KeepAlive time.Duration
	// LinkAttachTimeout bounds registration-to-link-attach before the
	// session is presumed dead (0 = DefaultLinkAttachTimeout).
	LinkAttachTimeout time.Duration
	// StateDir is <data>/state: the integrity stores and the tool-override
	// file. "" disables the tool-governance and quarantine endpoints, which
	// then answer the uniform 404 — a half-wired daemon must not pick a
	// directory of its own, and "unavailable on this daemon" is a shape
	// frontends already handle.
	StateDir string
	// StateLockTimeout bounds the state stores' cross-process locks
	// (0 = the store default).
	StateLockTimeout time.Duration
	// LogsDir is <data>/logs: the JSONL governance streams read back by
	// /v1/audit and /v1/security. "" disables both (uniform 404).
	LogsDir string
	// ToolLookup resolves a tool the operator names but that no approval
	// record covers yet, out of the gateway's persisted catalog cache. It is
	// what lets the kill switch work WITHOUT first starting the suspicious
	// server. nil means "no cache": disabling an unobserved tool answers the
	// uniform 404 instead.
	ToolLookup confops.ToolSnapshotFunc
	// ServerStateForgetters clear the out-of-registry stores keyed by server
	// id when DELETE /v1/servers/{id} removes one — integrity baselines,
	// quarantine entries, the cached tool list.
	//
	// They are INJECTED rather than opened here so this package keeps its
	// distance from the concrete stores; the daemon assembles them next to
	// the ones it already owns. An empty slice leaves that state behind and
	// is a half-wired daemon, not a supported mode: the two front ends must
	// agree on what deleting a server means (see handleServerDelete).
	ServerStateForgetters []confops.StateForgetter
	// NonRegistry carries the collaborators of the non-registry endpoints
	// (secrets / skills / tokens / clients / auth). Each of its fields is
	// independently optional; an absent one disables its endpoints, which
	// then answer the uniform 404. See nonreg.go.
	NonRegistry NonRegistryDeps
}

// Server is the control-plane HTTP server. Construct with NewServer, bind
// with Listen, then Serve.
type Server struct {
	opts Options
	log  *slog.Logger
	hs   *http.Server

	// eventSeq numbers every SSE frame written by any connection. Ids are
	// globally monotonic so a reconnecting client's Last-Event-ID is
	// comparable across connections (best-effort resume, see sse.go).
	eventSeq atomic.Uint64

	// nonRegMu guards the one non-registry dependency installed after
	// construction (the OAuth refresh coordinator — see SetRefresher).
	nonRegMu sync.Mutex

	// gateways holds the live control link of every registered stdio
	// gateway, keyed by session id (see gateway.go).
	gwMu     sync.Mutex
	gateways map[string]*gatewayLink
}

// NewServer validates opts and builds a Server.
func NewServer(opts Options) (*Server, error) {
	if opts.Registry == nil {
		return nil, errors.New("ctlapi: Options.Registry is required")
	}
	if opts.Sessions == nil {
		return nil, errors.New("ctlapi: Options.Sessions is required")
	}
	if opts.Bus == nil {
		return nil, errors.New("ctlapi: Options.Bus is required")
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.CoalesceWindow <= 0 {
		opts.CoalesceWindow = event.CoalesceWindow
	}
	if opts.SettleWindow <= 0 {
		opts.SettleWindow = event.SettleWindow
	}
	if opts.KeepAlive == 0 {
		opts.KeepAlive = 15 * time.Second
	}
	if opts.LinkAttachTimeout <= 0 {
		opts.LinkAttachTimeout = DefaultLinkAttachTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		opts:     opts,
		log:      log,
		gateways: make(map[string]*gatewayLink),
	}
	s.hs = &http.Server{
		Handler: s.Handler(),
		// No WriteTimeout: /v1/events is long-lived. Header reads are
		// bounded so a stalled dialer cannot pin an accept slot forever.
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Handler returns the full middleware-wrapped handler (exported for tests;
// production traffic must still come through Listen's peer-cred gate).
func (s *Server) Handler() http.Handler {
	return s.withMiddleware(http.HandlerFunc(s.route))
}

// Serve accepts on l until Shutdown or a fatal accept error. l is normally
// the peer-cred-checking listener from Listen.
func (s *Server) Serve(l net.Listener) error {
	err := s.hs.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server: the listener closes (no new
// connections), then in-flight requests are drained until ctx expires.
// Long-lived SSE connections do not drain on their own — callers with a
// bounded grace period follow up with Close.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.hs.Shutdown(ctx)
}

// Close force-closes the listener and every active connection (the in-
// process equivalent of the daemon being killed: no draining, no goodbyes).
// The daemon uses it after Shutdown's grace expires; tests use it as the
// kill -9 injection point (A.3 #2).
func (s *Server) Close() error {
	return s.hs.Close()
}

// route dispatches by hand instead of http.ServeMux so that EVERY miss —
// unknown path, unknown method, unknown resource — funnels into the one
// uniform 404 (ServeMux would emit its own 405/301 bodies and leak route
// shape to probes).
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/ping":
		if r.Method == http.MethodGet {
			s.handlePing(w, r)
			return
		}
	case "/v1/servers":
		switch r.Method {
		case http.MethodGet:
			s.handleServers(w, r)
			return
		case http.MethodPost:
			s.handleServerCreate(w, r)
			return
		}
	case "/v1/profiles":
		switch r.Method {
		case http.MethodGet:
			s.handleProfilesList(w, r)
			return
		case http.MethodPost:
			s.handleProfileCreate(w, r)
			return
		}
	case "/v1/config":
		if r.Method == http.MethodGet {
			s.handleConfigList(w, r)
			return
		}
	case "/v1/catalog":
		if r.Method == http.MethodGet {
			s.handleCatalogList(w, r)
			return
		}
	case "/v1/parse/client-config":
		if r.Method == http.MethodPost {
			s.handleParseClientConfig(w, r)
			return
		}
	case "/v1/tools":
		if r.Method == http.MethodGet {
			s.handleToolsList(w, r)
			return
		}
	case "/v1/quarantine":
		if r.Method == http.MethodGet {
			s.handleQuarantineList(w, r)
			return
		}
	case "/v1/sessions":
		if r.Method == http.MethodGet {
			s.handleSessions(w, r)
			return
		}
	case "/v1/events":
		if r.Method == http.MethodGet {
			s.handleEvents(w, r)
			return
		}
	case "/v1/gateway/register":
		if r.Method == http.MethodPost {
			s.handleGatewayRegister(w, r)
			return
		}
	default:
		if id, ok := killPathID(r); ok && r.Method == http.MethodPost {
			s.handleKillSession(w, r, id)
			return
		}
		if seg, ok := pathSegments(r, "/v1/servers/", 1); ok {
			switch r.Method {
			case http.MethodGet:
				s.handleServerGet(w, r, seg[0])
				return
			case http.MethodPatch:
				s.handleServerPatch(w, r, seg[0])
				return
			case http.MethodDelete:
				s.handleServerDelete(w, r, seg[0])
				return
			}
		}
		if seg, ok := pathSegments(r, "/v1/profiles/", 1); ok {
			switch r.Method {
			case http.MethodPatch:
				s.handleProfilePatch(w, r, seg[0])
				return
			case http.MethodDelete:
				s.handleProfileDelete(w, r, seg[0])
				return
			}
		}
		if seg, ok := pathSegments(r, "/v1/catalog/", 1); ok && r.Method == http.MethodGet {
			s.handleCatalogGet(w, r, seg[0])
			return
		}
		if seg, ok := pathSegments(r, "/v1/catalog/", 2); ok && seg[1] == "add" && r.Method == http.MethodPost {
			s.handleCatalogAdd(w, r, seg[0])
			return
		}
		if seg, ok := pathSegments(r, "/v1/scope/", 1); ok {
			switch r.Method {
			case http.MethodGet:
				s.handleScopeGet(w, r, seg[0])
				return
			case http.MethodPut:
				s.handleScopePut(w, r, seg[0])
				return
			case http.MethodDelete:
				s.handleScopeDelete(w, r, seg[0])
				return
			}
		}
		if seg, ok := pathSegments(r, "/v1/config/", 1); ok && r.Method == http.MethodPut {
			s.handleConfigSet(w, r, seg[0])
			return
		}
		if seg, ok := pathSegments(r, "/v1/tools/", 2); ok && r.Method == http.MethodPut {
			s.handleToolSet(w, r, seg[0], seg[1])
			return
		}
		if seg, ok := pathSegments(r, "/v1/quarantine/", 1); ok && r.Method == http.MethodDelete {
			s.handleQuarantineRelease(w, r, seg[0])
			return
		}
		if sid, action, ok := gatewayPath(r); ok {
			switch {
			case action == "link" && r.Method == http.MethodGet:
				s.handleGatewayLink(w, r, sid)
				return
			case action == "servers" && r.Method == http.MethodPost:
				s.handleGatewayServers(w, r, sid)
				return
			}
		}
	}
	// Non-registry endpoints (secrets / skills / tokens / clients / auth and
	// the server self-test) live in nonreg*.go. They are dispatched LAST and
	// report "not mine" by returning false, so every miss still funnels into
	// the one uniform 404 below.
	if s.routeNonRegistry(w, r) {
		return
	}
	writeNotFound(w, r)
}

// handlePing implements GET /v1/ping.
func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) {
	writeOK(w, http.StatusOK, api.Hello{
		Version:    s.opts.Version,
		Pid:        os.Getpid(),
		Generation: s.opts.Registry.Snapshot().Generation,
	})
}
