package httpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// DefaultPath is the MCP endpoint path.
const DefaultPath = "/mcp"

// SessionHeader carries the Streamable HTTP session id.
const SessionHeader = "Mcp-Session-Id"

// Dispatcher is the seam between this transport face and the MCP logic
// behind it (the daemon's shared downstream pool + internal/pipeline).
//
// It exists so that this package owns exactly one thing — the hardened HTTP
// ingress and the credential layer — and cannot grow a second copy of the
// gate chain. Implementations receive the authenticated Caller and MUST
// carry Caller.Tier into pipeline.CallRequest.CallerTier; that is where the
// second defence line of docs/architecture.md §9 is actually enforced.
type Dispatcher interface {
	// Dispatch answers one request. Returning a nil response is a protocol
	// violation and is reported as an internal error.
	Dispatch(ctx context.Context, c *Caller, s *Session, req *mcp.Request) *mcp.Response
	// Notify handles one notification. It never answers.
	Notify(ctx context.Context, c *Caller, s *Session, n *mcp.Notification)
}

// Options configures a Server.
type Options struct {
	// Dispatcher answers MCP messages (required).
	Dispatcher Dispatcher
	// Auth resolves credentials (required).
	Auth *Authenticator
	// Path overrides DefaultPath.
	Path string
	// SessionTTL / MaxSessions override the session table bounds.
	SessionTTL  time.Duration
	MaxSessions int
	// MaxInFlight overrides the in-flight ceiling.
	MaxInFlight int
	// Logger receives rejection and lifecycle records (nil = discard).
	Logger *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Server is the MCP Streamable HTTP exposure face.
//
// It answers exactly three verbs on one path:
//
//	POST   one JSON-RPC message in, one JSON-RPC message out (or 202 for a
//	       notification). initialize binds a session and returns its id.
//	DELETE terminate the session named by Mcp-Session-Id.
//	GET    405. canonical.md §5b freezes the transport asymmetry: agenthub
//	       never grows a new SSE exposure face, so there is no server-
//	       initiated stream to open here.
type Server struct {
	dispatcher Dispatcher
	auth       *Authenticator
	path       string
	sessions   *sessions
	inflight   semaphore
	log        *slog.Logger
	now        func() time.Time
}

// New builds the server. It fails rather than defaulting when a required
// collaborator is missing: an MCP endpoint with no authenticator is the
// exact fail-open this package exists to prevent.
func New(opts Options) (*Server, error) {
	if opts.Dispatcher == nil {
		return nil, errors.New("httpbridge: Options.Dispatcher is required")
	}
	if opts.Auth == nil {
		return nil, errors.New("httpbridge: Options.Auth is required")
	}
	path := opts.Path
	if path == "" {
		path = DefaultPath
	}
	inflight := opts.MaxInFlight
	if inflight <= 0 {
		inflight = MaxInFlight
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{
		dispatcher: opts.Dispatcher,
		auth:       opts.Auth,
		path:       path,
		sessions:   newSessions(opts.SessionTTL, opts.MaxSessions, now),
		inflight:   newSemaphore(inflight),
		log:        log,
		now:        now,
	}, nil
}

// Handler returns the http.Handler for this server. Mount it directly; the
// per-request limits live inside, not in middleware a caller could forget.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// HTTPServer wraps Handler in an http.Server carrying the head-side ingress
// limits (MaxHeaderBytes, ReadHeaderTimeout). Those two CANNOT be enforced
// from inside a handler — by the time a handler runs, the head has already
// been read — which is why assemblies must use this constructor rather than
// building their own http.Server.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		MaxHeaderBytes:    MaxHeaderBytes,
		ReadHeaderTimeout: HeaderReadTimeout,
		ErrorLog:          nil,
	}
}

// Sessions reports the number of live sessions (diagnostics, tests).
func (s *Server) Sessions() int { return s.sessions.len() }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != s.path {
		// Same frozen body as an unknown session: an endpoint that answers
		// "no such path" differently from "no such session" is a map.
		s.fail(w, r, errNotFound)
		return
	}
	// Shed BEFORE authenticating: the point of the in-flight ceiling is to
	// bound work an unauthenticated caller can cause.
	if !s.inflight.acquire() {
		s.fail(w, r, errOverloaded)
		return
	}
	defer s.inflight.release()

	if err := checkFetchMetadata(r); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := checkOrigin(r); err != nil {
		s.fail(w, r, err)
		return
	}

	caller, err := s.auth.Authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agenthub"`)
		s.fail(w, r, errUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r, caller)
	case http.MethodDelete:
		s.handleDelete(w, r, caller)
	case http.MethodGet:
		// No SSE exposure face (canonical.md §5b). Clients treat 405 as
		// "this server offers no notification stream" and carry on.
		s.fail(w, r, errNoStream)
	default:
		s.fail(w, r, errMethod)
	}
}

// handlePost is the message path: read the bounded body, parse ONE JSON-RPC
// message, bind or resolve the session, dispatch.
func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !acceptsJSON(r) {
		s.fail(w, r, &httpError{http.StatusNotAcceptable, CodeBadRequest,
			"this endpoint answers application/json"})
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	msg, perr := mcp.ParseMessage(body)
	if perr != nil {
		// A malformed frame is a JSON-RPC-level failure with no id to bind
		// an answer to, so it is reported at the HTTP level.
		s.fail(w, r, &httpError{http.StatusBadRequest, CodeBadRequest, "malformed JSON-RPC message"})
		return
	}

	switch m := msg.(type) {
	case *mcp.Request:
		s.handleRequest(w, r, c, m)
	case *mcp.Notification:
		sess, ok := s.resolveSession(w, r, c, false)
		if !ok {
			return
		}
		s.dispatcher.Notify(r.Context(), c, sess, m)
		w.WriteHeader(http.StatusAccepted)
	default:
		// A response arriving upstream-to-downstream has no meaning on this
		// face: there are no server-initiated requests without a stream.
		s.fail(w, r, &httpError{http.StatusBadRequest, CodeBadRequest,
			"only JSON-RPC requests and notifications are accepted here"})
	}
}

// handleRequest answers one JSON-RPC request. initialize is the only method
// that may arrive WITHOUT a session id — it is what mints one.
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request, c *Caller, req *mcp.Request) {
	var sess *Session
	if req.Method == mcp.MethodInitialize && r.Header.Get(SessionHeader) == "" {
		created, err := s.sessions.create(c)
		if err != nil {
			s.fail(w, r, asHTTPError(err))
			return
		}
		sess = created
		w.Header().Set(SessionHeader, sess.ID)
		s.log.Info("http session bound",
			"session", sess.ID, "caller", string(c.Kind), "token", c.Token, "tier", string(c.Tier))
	} else {
		resolved, ok := s.resolveSession(w, r, c, true)
		if !ok {
			return
		}
		sess = resolved
	}

	res := s.dispatcher.Dispatch(r.Context(), c, sess, req)
	if res == nil {
		res = mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code: mcp.CodeInternalError, Message: "agenthub produced no response",
		})
	}
	raw, err := json.Marshal(res)
	if err != nil {
		s.fail(w, r, &httpError{http.StatusInternalServerError, CodeBadRequest, "response could not be encoded"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handleDelete terminates a session.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, c *Caller) {
	id := r.Header.Get(SessionHeader)
	if id == "" || !s.sessions.drop(id, c) {
		s.fail(w, r, errNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveSession looks up the session named by the request header. When
// required is false a missing header is allowed (a notification sent before
// any session was bound is simply unbound), but a header naming a session
// that does not resolve is ALWAYS a 404 — presenting an id we reject is a
// different thing from presenting none.
func (s *Server) resolveSession(w http.ResponseWriter, r *http.Request, c *Caller, required bool) (*Session, bool) {
	id := r.Header.Get(SessionHeader)
	if id == "" {
		if required {
			s.fail(w, r, errNotFound)
			return nil, false
		}
		return nil, true
	}
	sess, ok := s.sessions.get(id, c)
	if !ok {
		s.fail(w, r, errNotFound)
		return nil, false
	}
	return sess, true
}

// fail writes one rejection. The body shape is frozen:
// {"error":{"code":"...","message":"..."}}.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	he := asHTTPError(err)
	s.log.Info("http request rejected",
		"method", r.Method, "path", r.URL.Path, "status", he.Status, "code", he.Code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(he.Status)
	body, merr := json.Marshal(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: he.Code, Message: he.Message}})
	if merr != nil {
		return
	}
	_, _ = w.Write(body)
}

// asHTTPError normalises any error into a rejection. Anything unrecognised
// becomes a 500 with no detail: an error string from deeper in the stack
// must never become an information leak on an unauthenticated surface.
func asHTTPError(err error) *httpError {
	var he *httpError
	if errors.As(err, &he) {
		return he
	}
	return &httpError{http.StatusInternalServerError, "internal", "internal error"}
}
