package httpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// DefaultPath is the MCP endpoint path.
const DefaultPath = "/mcp"

// SessionHeader carries the Streamable HTTP session id.
const SessionHeader = "Mcp-Session-Id"

// MethodHeader and NameHeader are required on every POST once 2026-07-28 is
// in play: the JSON-RPC method, and the tool/resource/prompt name when the
// params carry one. A header that disagrees with the body is rejected with
// CodeHeaderMismatch (-32020) and HTTP 400.
const (
	MethodHeader = "Mcp-Method"
	NameHeader   = "Mcp-Name"
)

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
//	       notification). initialize binds a session and returns its id
//	       (≤ 2025-11-25); the stateless 2026-07-28 shapes — server/discover,
//	       or any request carrying the per-request _meta — pass with no
//	       session and mint none (see handleRequest).
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
		if e := checkMcpHeaders(r, m.Method, m.Params); e != nil {
			s.replyRPCError(w, http.StatusBadRequest, m.ID, e)
			return
		}
		s.handleRequest(w, r, c, m)
	case *mcp.Notification:
		if e := checkMcpHeaders(r, m.Method, m.Params); e != nil {
			// A notification has no id to bind a JSON-RPC error to.
			s.fail(w, r, &httpError{http.StatusBadRequest, CodeBadRequest, e.Message})
			return
		}
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

// handleRequest answers one JSON-RPC request. Two shapes may arrive WITHOUT
// a session id: initialize (≤ 2025-11-25, it is what mints one) and the
// stateless 2026-07-28 shapes — server/discover, or any request carrying
// the per-request _meta. 2026 removed the session header from the wire, and
// nothing here needs one: the per-caller state behind the dispatcher is
// keyed by the authenticated identity, never by this header.
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request, c *Caller, req *mcp.Request) {
	var sess *Session
	switch {
	case req.Method == mcp.MethodInitialize && r.Header.Get(SessionHeader) == "":
		created, err := s.sessions.create(c)
		if err != nil {
			s.fail(w, r, asHTTPError(err))
			return
		}
		sess = created
		w.Header().Set(SessionHeader, sess.ID)
		s.log.Info("http session bound",
			"session", sess.ID, "caller", string(c.Kind), "token", c.Token, "tier", string(c.Tier))
	case r.Header.Get(SessionHeader) == "" && stateless2026Request(req):
		// Sessionless by protocol. The declared _meta version is validated
		// by the gateway behind the dispatcher (one enforcement point), so
		// a wrong version still earns its -32022 rather than a 404 here.
		sess = nil
	default:
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

// stateless2026Request reports whether req is a 2026-07-28 stateless shape:
// server/discover, or a request whose params carry the per-request _meta
// with a declared protocol version. The version's VALUE is deliberately not
// checked here — the gateway behind the dispatcher owns that rejection
// (-32022), and a second copy of the check would be a second place for it
// to be wrong.
func stateless2026Request(req *mcp.Request) bool {
	return req.Method == mcp.MethodDiscover || carries2026Meta(req.Params)
}

// carries2026Meta reports whether params carry the per-request _meta with a
// declared protocol version. It is the ONE decode of that question in this
// file, for the same reason the version's value is decoded in none of them:
// the predicate is asked twice — here, for routing a request that brings no
// session, and in checkMcpHeaders, for deciding which requests owe an
// Mcp-Method header — and two decodes are two places to disagree about what
// "carries _meta" means.
//
// A params blob that will not unmarshal counts as NOT carrying it. That is
// the safe direction: such a request is refused a few frames later by the
// dispatcher that actually needs the params, with an error about the params
// rather than about a header it was never obliged to send.
func carries2026Meta(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var probe struct {
		Meta *mcp.RequestMeta `json:"_meta"`
	}
	if err := json.Unmarshal(params, &probe); err != nil || probe.Meta == nil {
		return false
	}
	return probe.Meta.ProtocolVersion != ""
}

// checkMcpHeaders enforces the 2026-07-28 Mcp-Method / Mcp-Name headers
// against the message body. Rules, in the order they can fail:
//
//   - A present Mcp-Method that differs from the body's method is -32020,
//     whatever the session's protocol generation — a lying header is never
//     tolerable.
//   - A request that carries the 2026 per-request _meta MUST carry
//     Mcp-Method (the spec requires it on every POST); its absence is
//     -32020. Requests without _meta (stateful sessions) owe no headers.
//   - A present Mcp-Name that differs from the params' top-level name is
//     -32020. Params without a name owe no Mcp-Name.
//
// nil means the headers are acceptable.
func checkMcpHeaders(r *http.Request, method string, params json.RawMessage) *mcp.Error {
	hdrMethod := r.Header.Get(MethodHeader)
	if hdrMethod != "" && hdrMethod != method {
		return &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: fmt.Sprintf(
			"%s header %q disagrees with the body method %q", MethodHeader, hdrMethod, method)}
	}
	if hdrMethod == "" && carries2026Meta(params) {
		return &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: fmt.Sprintf(
			"2026-07-28 requires the %s header on every request", MethodHeader)}
	}
	var probe struct {
		Name string `json:"name"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &probe) // non-object params carry no name
	}
	if hdrName := r.Header.Get(NameHeader); hdrName != "" && probe.Name != "" && hdrName != probe.Name {
		return &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: fmt.Sprintf(
			"%s header %q disagrees with the params name %q", NameHeader, hdrName, probe.Name)}
	}
	return nil
}

// replyRPCError writes a JSON-RPC error response with the given HTTP
// status: the 2026-07-28 header rejections are protocol-level errors that
// still bind to the request id.
func (s *Server) replyRPCError(w http.ResponseWriter, status int, id mcp.ID, e *mcp.Error) {
	raw, err := json.Marshal(mcp.NewErrorResponse(id, e))
	if err != nil {
		s.fail(w, nil, &httpError{http.StatusInternalServerError, CodeBadRequest, "response could not be encoded"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
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
