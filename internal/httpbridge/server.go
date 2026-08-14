package httpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/logx"
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
	// ProtocolVersionHeader must name a version this server speaks
	// (checkProtocolVersion) and, when the body's _meta declares one, must
	// equal it (checkMcpHeaders). Two rules, two carve-outs, both there.
	ProtocolVersionHeader = "MCP-Protocol-Version"
)

// Dispatcher is the seam between this transport face and the MCP logic
// behind it (the daemon's shared downstream pool + internal/pipeline).
//
// It exists so that this package owns exactly one thing — the hardened HTTP
// ingress and the credential layer — and cannot grow a second copy of the
// gate chain. Implementations receive the authenticated Caller and MUST
// carry Caller.Tier into pipeline.CallRequest.CallerTier; that is where the
// second defence line of docs/architecture.md#what-a-call-passes-through is actually enforced.
type Dispatcher interface {
	// Dispatch answers one request. Returning a nil response is a protocol
	// violation and is reported as an internal error.
	Dispatch(ctx context.Context, c *Caller, s *Session, req *mcp.Request) *mcp.Response
	// Notify handles one notification. It never answers.
	Notify(ctx context.Context, c *Caller, s *Session, n *mcp.Notification)
	// Subscribe opens the server→client direction for one caller, so this
	// face can carry a notification the MCP logic produced.
	//
	// accept narrows what the client asked for and may be nil, meaning every
	// method. It is the CLIENT's own filter, never a gate: what may be
	// produced for this caller was already decided by the scope its
	// credential resolves to, on the other side of this seam.
	Subscribe(ctx context.Context, c *Caller, s *Session, accept func(method string) bool) (Subscription, error)
}

// Subscription is one open server→client notification channel.
//
// Next blocks until a notification arrives; ok=false means none ever will,
// because ctx ended or the channel closed underneath. The caller MUST Close
// it — that is what returns the stream quota slot and the resources behind
// the seam.
type Subscription interface {
	Next(ctx context.Context) (*mcp.Notification, bool)
	Close()
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
	// MaxStreams overrides the open-notification-stream ceiling.
	MaxStreams int
	// Logger receives rejection and lifecycle records (nil = discard).
	Logger *slog.Logger
	// Events receives the session lifecycle as records (nil = prose only).
	//
	// This face is the one place a single process holds many sessions at
	// once, so "which sessions were live at 11:03" is a question only it can
	// answer — and before these records a session that timed out left no
	// trace anywhere, which is indistinguishable from one that was never
	// opened.
	Events *eventlog.Stream
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
//	GET    the ≤ 2025-11-25 notification stream, as text/event-stream
//	       (stream.go). A GET whose Accept rules that out is still a 405.
//
// The GET stream is streamable-HTTP's OWN server→client channel, and adding
// it did not reopen ruling #29: legacy HTTP+SSE — the 2024-11-05 two-endpoint
// transport with its `endpoint` event — remains a read-side transport that
// this face never offers.
type Server struct {
	dispatcher Dispatcher
	auth       *Authenticator
	path       string
	sessions   *sessions
	inflight   semaphore
	// streams is the second quota, and deliberately not a slice of the
	// first: see MaxStreams for why an open stream and an executing request
	// cannot share a ceiling.
	streams semaphore
	log     *slog.Logger
	events  *eventlog.Stream
	now     func() time.Time
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
	streams := opts.MaxStreams
	if streams <= 0 {
		streams = MaxStreams
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := &Server{
		dispatcher: opts.Dispatcher,
		auth:       opts.Auth,
		path:       path,
		sessions:   newSessions(opts.SessionTTL, opts.MaxSessions, now),
		inflight:   newSemaphore(inflight),
		streams:    newSemaphore(streams),
		log:        log,
		events:     opts.Events,
		now:        now,
	}
	srv.sessions.closed = srv.recordSessionClosed
	return srv, nil
}

// recordSessionClosed reports one session leaving the table, whichever way it
// left. The table calls it with its lock released.
func (s *Server) recordSessionClosed(sess *Session, reason string) {
	s.events.Emit(s.log, eventlog.Record{
		Scope: eventlog.ScopeGateway, Kind: eventlog.KindSessionClosed,
		Session: sess.ID, Detail: reason,
		DurMs: s.now().Sub(sess.created).Milliseconds(),
	}, "http session ended", logx.Session(sess.ID), "reason", reason,
		"lifetime", s.now().Sub(sess.created).Round(time.Second))
}

// Handler returns the http.Handler for this server. The per-request limits
// live inside it rather than in middleware a caller could forget — but the two
// HEAD-side ones do not, and cannot: MaxHeaderBytes and ReadHeaderTimeout are
// http.Server fields, and by the time any handler runs the head is already
// read.
//
// This comment used to say "mount it directly", which is the one instruction
// that quietly drops those two. Mounting it bare is a decision to serve
// without them; the production path does not, and neither should an assembly
// bringing its own listener. Serve goes through HTTPServer below. The single
// caller that does mount this bare is a test on httptest.Server, where the
// head comes from the test itself.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// HTTPServer wraps Handler in an http.Server carrying the head-side ingress
// limits (MaxHeaderBytes, ReadHeaderTimeout). Those two CANNOT be enforced
// from inside a handler — by the time a handler runs, the head has already
// been read — which is why an assembly bringing its own listener must build
// through this constructor rather than an http.Server of its own. Serve is
// the normal path and already does.
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
	// Held through the ordinary paths and handed back early by a request
	// that stops being one: a notification stream releases this before
	// parking on the stream quota instead. Release is idempotent, so this
	// defer is correct in both cases.
	held := &slot{sem: s.inflight}
	defer held.release()

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
		s.handlePost(w, r, caller, held)
	case http.MethodDelete:
		s.handleDelete(w, r, caller)
	case http.MethodGet:
		s.handleGet(w, r, caller, held)
	default:
		s.fail(w, r, errMethod)
	}
}

// handlePost is the message path: read the bounded body, parse ONE JSON-RPC
// message, bind or resolve the session, dispatch.
//
// The Accept check runs AFTER the parse, which it did not used to. One method
// on this path is answered with text/event-stream rather than JSON —
// subscriptions/listen, whose response IS the stream — so "will this client
// read my answer?" cannot be settled before knowing which answer is owed.
// Reading the body first costs nothing that was not already bounded:
// readBody applies MaxBodyBytes and the read deadline regardless of what the
// message turns out to be.
func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, c *Caller, held *slot) {
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

	// The one shape whose answer is not JSON, taken before the Accept gate
	// below. It is answered by this face rather than through Dispatch
	// because that seam returns ONE response, and this method's response is
	// a body that stays open — which shape a reply takes is a transport
	// decision, and a transport is what this package is.
	if req, ok := msg.(*mcp.Request); ok && req.Method == mcp.MethodSubscriptionsListen {
		if e := checkMcpHeaders(r, req.Method, req.Params); e != nil {
			s.replyRPCError(w, r, http.StatusBadRequest, req.ID, e)
			return
		}
		s.handleListen(w, r, c, req, held)
		return
	}
	if !acceptsJSON(r) {
		s.fail(w, r, &httpError{http.StatusNotAcceptable, CodeBadRequest,
			"this endpoint answers application/json"})
		return
	}

	switch m := msg.(type) {
	case *mcp.Request:
		if e := checkMcpHeaders(r, m.Method, m.Params); e != nil {
			s.replyRPCError(w, r, http.StatusBadRequest, m.ID, e)
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
		s.events.Emit(s.log, eventlog.Record{
			Scope: eventlog.ScopeGateway, Kind: eventlog.KindSessionOpened,
			Session: sess.ID, Detail: string(c.Kind),
		}, "http session bound",
			logx.Session(sess.ID), "caller", string(c.Kind), "token", c.Token, "tier", string(c.Tier))
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
	w.WriteHeader(statusForResponse(res, req))
	_, _ = w.Write(raw)
}

// statusForResponse maps a JSON-RPC answer onto the HTTP status the
// streamable-HTTP binding requires for it. A JSON-RPC error is still a
// successful HTTP exchange unless the binding says otherwise, so anything
// the specification does not assign a status to stays 200.
//
// Two it does assign:
//
//   - -32601 method-not-found is 404. The JSON-RPC body is what tells that
//     404 apart from a legacy HTTP+SSE server's "no endpoint here".
//   - -32022 is 400. This one has teeth: the backward-compatibility flow
//     has a client inspect the body only ON a 400, so answering 200 means a
//     client following it never reads the supported-version list it was
//     told to retry with.
//
// The 404 is gated on the request's generation, and that is not caution for
// its own sake. The rule lives in the 2026-07-28 binding; a ≤ 2025-11-25
// client never agreed to it, and agenthub's own downstream client reads an
// HTTP 404 as a dropped session and re-initializes — so answering a legacy
// caller's unknown method with 404 would turn "no such method" into a
// reconnect loop. -32022 needs no such gate: it can only arise from a
// request that declared 2026 in its _meta.
func statusForResponse(res *mcp.Response, req *mcp.Request) int {
	if res == nil || res.Error == nil {
		return http.StatusOK
	}
	switch res.Error.Code {
	case mcp.CodeUnsupportedProtocolVersion,
		mcp.CodeMissingRequiredClientCapability,
		mcp.CodeHeaderMismatch:
		return http.StatusBadRequest
	case mcp.CodeMethodNotFound:
		if stateless2026Request(req) {
			return http.StatusNotFound
		}
	}
	return http.StatusOK
}

// handleDelete terminates a session. The version rule binds "all subsequent
// requests", and this verb is one — it just has no body, so it checks the
// header on its own rather than through checkMcpHeaders, and reports the
// refusal at the HTTP level because there is no JSON-RPC id to bind one to.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, c *Caller) {
	if e := checkProtocolVersion(r); e != nil {
		s.fail(w, r, &httpError{http.StatusBadRequest, CodeBadRequest, e.Message})
		return
	}
	id := r.Header.Get(SessionHeader)
	if id == "" {
		// Same split as resolveSession, for the same reason: no header is
		// a malformed request, an id that does not resolve is a miss.
		s.fail(w, r, errSessionRequired)
		return
	}
	if !s.sessions.drop(id, c) {
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
	return metaProtocolVersion(params) != ""
}

// metaProtocolVersion returns the protocol version the params' _meta
// declares, or "" when there is none. It is the one decode of that value on
// this face: carries2026Meta asks whether there is one, checkMcpHeaders
// compares it to the header, and neither judges the value itself — the
// gateway behind the dispatcher owns that rejection (-32022), and a second
// copy of the check would be a second place for it to be wrong.
func metaProtocolVersion(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var probe struct {
		Meta *mcp.RequestMeta `json:"_meta"`
	}
	if err := json.Unmarshal(params, &probe); err != nil || probe.Meta == nil {
		return ""
	}
	return probe.Meta.ProtocolVersion
}

// checkProtocolVersion refuses an MCP-Protocol-Version header naming a
// version this server does not speak. Every revision that defines the header
// requires this, and each one words it as a MUST:
//
//   - 2025-06-18 and 2025-11-25, basic/transports.mdx §"Protocol Version
//     Header": "If the server receives a request with an invalid or
//     unsupported MCP-Protocol-Version, it MUST respond with 400 Bad
//     Request."
//   - 2026-07-28 keeps the status and adds a body: the refusal MUST carry
//     UnsupportedProtocolVersionError listing what the server does support.
//
// So the answer is one shape for all three — -32022 with the supported list,
// which the binding renders as 400 — rather than a bare 400 for the older
// callers. A pre-2026 client was promised nothing about the body and reads
// an unknown code, which costs it nothing; a 2026 client gets exactly what
// its revision asks for. Two shapes would be two places to keep the list
// right.
//
// ABSENCE is not refused, and that is a separate rule from this one: the
// header postdates 2025-03-26, and the specification tells a server to read
// its absence as that version. What this function closes is the case the
// _meta comparison in checkMcpHeaders could not see — a header carrying a
// version no body declares, which is every ≤ 2025-11-25 request, since only
// 2026 sends the per-request _meta that comparison needs.
//
// The list is mcp.SupportedVersions, the same promise server/discover
// advertises and initialize echoes. A version this server would negotiate
// must not be refused in a header, and one it would not negotiate must not
// be accepted in a header; reading the answer from one place is how those
// two stay the same answer.
func checkProtocolVersion(r *http.Request) *mcp.Error {
	v := r.Header.Get(ProtocolVersionHeader)
	if v == "" || slices.Contains(mcp.SupportedVersions, v) {
		return nil
	}
	return mcp.NewUnsupportedVersionError(v, mcp.SupportedVersions, fmt.Sprintf(
		"%s header %q names a protocol version this server does not speak",
		ProtocolVersionHeader, v))
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
//   - An Mcp-Name whose value, decoded, differs from the params' name (or
//     uri) is -32020. Params carrying neither owe no Mcp-Name.
//   - An MCP-Protocol-Version header that differs from the version the
//     body's _meta declares is -32020. Only a MISMATCH is refused there: an
//     ABSENT header is allowed, because this server also serves clients
//     from before 2025-06-18 defined the header at all, and the
//     specification lets such a server read absence as 2025-03-26.
//
// A header naming a version this server does not speak is refused too, but
// by checkProtocolVersion below rather than here — that rule binds DELETE as
// well, which carries no body for these checks to read.
//
// The version rule is the same one this project's own client was getting
// wrong until recently, and the same one test/mcpstub enforces with the
// comment that a stub which skips it certifies nothing. The threat is the
// one already argued for Mcp-Method: an intermediary routing on the header
// while the server executes on the body, which agenthub's non-loopback
// token-authed bind makes reachable.
//
// The decode is not a convenience. A name outside the header-safe set
// travels under the base64 sentinel, so comparing the raw header text would
// reject exactly the conformant clients that had to encode; and a
// sentinel-shaped value that does not decode is malformed rather than
// literal, so it is refused instead of compared (fail closed).
//
// nil means the headers are acceptable.
func checkMcpHeaders(r *http.Request, method string, params json.RawMessage) *mcp.Error {
	if e := checkProtocolVersion(r); e != nil {
		return e
	}
	hdrMethod := r.Header.Get(MethodHeader)
	if hdrMethod != "" && hdrMethod != method {
		return &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: fmt.Sprintf(
			"%s header %q disagrees with the body method %q", MethodHeader, hdrMethod, method)}
	}
	if hdrMethod == "" && carries2026Meta(params) {
		return &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: fmt.Sprintf(
			"2026-07-28 requires the %s header on every request", MethodHeader)}
	}
	if hdrVersion := r.Header.Get(ProtocolVersionHeader); hdrVersion != "" {
		if declared := metaProtocolVersion(params); declared != "" && hdrVersion != declared {
			return &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: fmt.Sprintf(
				"%s header %q disagrees with the protocol version the body's _meta declares (%q)",
				ProtocolVersionHeader, hdrVersion, declared)}
		}
	}
	var probe struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &probe) // non-object params carry no name
	}
	bodyName := probe.Name
	if bodyName == "" {
		bodyName = probe.URI
	}
	hdrName := r.Header.Get(NameHeader)
	if hdrName == "" || bodyName == "" {
		return nil
	}
	decoded, ok := mcp.DecodeHeaderValue(hdrName)
	if !ok {
		return &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: fmt.Sprintf(
			"%s header %q claims the base64 sentinel encoding but does not decode", NameHeader, hdrName)}
	}
	if decoded != bodyName {
		return &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: fmt.Sprintf(
			"%s header %q disagrees with the params name %q", NameHeader, decoded, bodyName)}
	}
	return nil
}

// replyRPCError writes a JSON-RPC error response with the given HTTP
// status: the 2026-07-28 header rejections are protocol-level errors that
// still bind to the request id.
//
// It takes the request only so its own encode failure can be reported like
// every other rejection on this face. That path is not reachable today — an
// mcp.ID holds validated JSON text and these errors carry no Data — but fail
// reads the request unconditionally, so the one call site that passed nil
// answered an unencodable response with a panic.
func (s *Server) replyRPCError(w http.ResponseWriter, r *http.Request, status int, id mcp.ID, e *mcp.Error) {
	raw, err := json.Marshal(mcp.NewErrorResponse(id, e))
	if err != nil {
		s.fail(w, r, &httpError{http.StatusInternalServerError, CodeBadRequest, "response could not be encoded"})
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
//
// THE TWO MISSES GET DIFFERENT STATUSES, and the specification is explicit
// about which: a server that requires a session id SHOULD answer 400 to a
// request that brings no header, and MUST answer 404 to one naming a session
// it terminated. Both rules are verbatim in 2025-03-26, 2025-06-18 and
// 2025-11-25. Collapsing them into 404 was not cosmetic, because the client
// rule attached to 404 is "start a new session": a client that omits the
// header re-initialized, omitted it again, and looped — filling the session
// table in 256 rounds, after which initialize answered 503 to EVERY caller
// until the TTL swept it. One broken client took the HTTP face down for all
// of them, under a message naming the wrong cause.
//
// The frozen 404 body is not weakened by the split. Its rule is about ids
// that were PRESENTED — unknown, expired, foreign, all one sentence so the
// endpoint cannot be probed for which sessions exist — and a request that
// carries no id probes nothing. The unknown-path 404 in serveHTTP is
// likewise untouched: answering "no such path" differently from "no such
// session" is the map this endpoint must not draw.
func (s *Server) resolveSession(w http.ResponseWriter, r *http.Request, c *Caller, required bool) (*Session, bool) {
	id := r.Header.Get(SessionHeader)
	if id == "" {
		if required {
			s.fail(w, r, errSessionRequired)
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
	body, merr := json.Marshal(rejectionBody{
		Error: rejection{Code: he.Code, Message: he.Message},
	})
	if merr != nil {
		return
	}
	_, _ = w.Write(body)
}

// rejectionBody is the frozen shape of every rejection this face writes:
// {"error":{"code":"…","message":"…"}}. The codes are ABI for anything
// parsing it (see the Code* constants in ingress.go).
//
// Named rather than written inline, where the shape appeared twice — as the
// anonymous type and again as the literal's type. The compiler did hold the
// two copies identical, so the cost was entirely the reader's: eleven lines
// of field declarations in the middle of the error path, and no name for the
// thing they describe. A frozen wire shape is worth being able to point at.
type rejectionBody struct {
	Error rejection `json:"error"`
}

type rejection struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
