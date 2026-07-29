package ctlapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/audit"
	"github.com/dinstein/agent-hub/internal/scope"
	"github.com/dinstein/agent-hub/internal/session"
)

// maxBodyBytes bounds control-plane request bodies. Scope mutations are
// small; anything near this limit is malformed or hostile.
const maxBodyBytes = 1 << 20

// serverList synthesizes the api.Server list from the registry snapshot and
// the runtime state source. It is shared by GET /v1/servers and the lazily
// built `servers` SSE payload, so push and pull are byte-identical
// (docs/modules/controlplane.md).
func (s *Server) serverList() []api.Server {
	snap := s.opts.Registry.Snapshot()
	out := make([]api.Server, 0, len(snap.Servers.V.Servers))
	for id, doc := range snap.Servers.V.Servers {
		entry := doc.V
		var rt ServerRuntime
		var haveRT bool
		if s.opts.States != nil {
			rt, haveRT = s.opts.States.ServerRuntime(id)
		}
		admin := api.AdminStateEnabled
		switch {
		case rt.Quarantined:
			// Quarantine outranks the enabled flag: an isolated server must
			// display as isolated even if still enabled in the registry.
			admin = api.AdminStateQuarantined
		case !entry.Enabled:
			admin = api.AdminStateDisabled
		}
		state := string(rt.Conn)
		if !haveRT || rt.Conn == ConnUnknown {
			state = "unknown"
		}
		out = append(out, api.Server{
			ID:        id,
			Transport: entry.Transport,
			Enabled:   entry.Enabled,
			State:     state,
			Tools:     rt.Tools,
			Source:    entry.Source,
			Health: ComputeHealth(HealthInput{
				AdminState:       admin,
				MissingSecrets:   rt.MissingSecrets,
				OAuthConfigError: rt.OAuthConfigError,
				Conn:             rt.Conn,
				ConnDetail:       rt.ConnDetail,
				CallAuthFailed:   rt.CallAuthFailed,
				Token:            rt.Token,
			}),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// handleServers implements GET /v1/servers.
func (s *Server) handleServers(w http.ResponseWriter, _ *http.Request) {
	writeOK(w, http.StatusOK, s.serverList())
}

// sessionList maps live sessions to their wire form (shared by
// GET /v1/sessions and the SSE resume path).
func (s *Server) sessionList() []api.SessionInfo {
	infos := s.opts.Sessions.List()
	out := make([]api.SessionInfo, len(infos))
	for i, in := range infos {
		out[i] = apiSessionInfo(in)
	}
	return out
}

// apiSessionInfo converts one session.Info to the wire DTO. ProfileName is
// left empty until scope resolution is wired into the daemon (a later M1
// task); OverlaySummary is a minimal version digest for now.
func apiSessionInfo(in session.Info) api.SessionInfo {
	root := ""
	if len(in.Roots) > 0 {
		root = in.Roots[0]
	}
	summary := ""
	if in.OverlayVersion > 0 {
		summary = fmt.Sprintf("overlay v%d", in.OverlayVersion)
	}
	return api.SessionInfo{
		ID:             string(in.ID),
		ClientID:       in.ClientID,
		Origin:         in.Origin.String(),
		Root:           root,
		OverlaySummary: summary,
		LastSeen:       in.LastSeen,
	}
}

// handleSessions implements GET /v1/sessions.
func (s *Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	writeOK(w, http.StatusOK, s.sessionList())
}

// scopePathID matches POST /v1/sessions/{id}/scope and extracts the session
// id. It works on the ESCAPED path so an id containing %2F cannot smuggle
// extra path segments, then unescapes the single segment.
func scopePathID(r *http.Request) (string, bool) {
	return sessionPathID(r, "/scope")
}

// killPathID matches POST /v1/sessions/{id}/kill.
func killPathID(r *http.Request) (string, bool) {
	return sessionPathID(r, "/kill")
}

// sessionPathID extracts the session id from /v1/sessions/{id}<suffix>.
// Same escaping discipline as scopePathID: the match runs on the ESCAPED
// path so a %2F inside an id cannot smuggle an extra path segment past the
// router, and the single segment is unescaped afterwards.
func sessionPathID(r *http.Request, suffix string) (string, bool) {
	p := r.URL.EscapedPath()
	rest, ok := strings.CutPrefix(p, "/v1/sessions/")
	if !ok {
		return "", false
	}
	seg, ok := strings.CutSuffix(rest, suffix)
	if !ok || seg == "" || strings.Contains(seg, "/") {
		return "", false
	}
	id, err := url.PathUnescape(seg)
	if err != nil || id == "" {
		return "", false
	}
	return id, true
}

// handleKillSession implements POST /v1/sessions/{id}/kill: force-disconnect
// one live session (`agenthub session kill`).
//
// Existence is checked BEFORE Close because Close is idempotent by contract
// (closing an unknown id is a no-op): without the check the CLI could not
// tell "killed" from "there was never such a session", and a typo'd id
// would report success. An unknown id answers the uniform 404, exactly like
// an unknown route (anti-probing, same rule as handleSetScope).
func (s *Server) handleKillSession(w http.ResponseWriter, r *http.Request, id string) {
	sess, ok := s.opts.Sessions.Get(session.SessionID(id))
	if !ok {
		writeNotFound(w, r)
		return
	}
	clientID := sess.ClientID
	start := time.Now()
	s.opts.Sessions.Close(session.SessionID(id))
	s.auditKill(r, id, clientID, time.Since(start))
	writeOK(w, http.StatusOK, KillResult{SessionID: id, ClientID: clientID, Killed: true})
}

// KillResult is the body of a successful session kill.
type KillResult struct {
	SessionID string `json:"session_id"`
	ClientID  string `json:"client_id,omitempty"`
	Killed    bool   `json:"killed"`
}

// auditKill records the control-plane write. Killing a session is a
// governance action (it revokes a live agent's whole connection), so it is
// audited on the same stream as scope mutations.
func (s *Server) auditKill(r *http.Request, sessionID, clientID string, dur time.Duration) {
	if s.opts.Audit == nil {
		return
	}
	s.opts.Audit.Append(audit.Record{
		Actor:     actorFrom(r.Context()),
		Client:    clientID,
		Session:   sessionID,
		Tool:      "sessions/kill",
		Decision:  audit.DecisionAllowed,
		DurMs:     dur.Milliseconds(),
		RequestID: requestIDFrom(r.Context()),
	})
}

// handleSetScope implements POST /v1/sessions/{id}/scope: a NARROW-ONLY
// overlay mutation applied through SessionManager.Mutate. The tighten-only
// validation lives in internal/session; any widening (including Reset that
// would undo a narrowing) is rejected 403 — the human-grant path that may
// loosen is the approval flow (M1-C), never this endpoint.
func (s *Server) handleSetScope(w http.ResponseWriter, r *http.Request, id string) {
	reqID := requestIDFrom(r.Context())
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}
	var narrow ScopeNarrowWire
	if err := json.Unmarshal(body, &narrow); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "decoding scope body: "+err.Error(), "", reqID)
		return
	}
	if narrow.Discovery != "" && !validDiscoveryMode(narrow.Discovery) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"unknown discovery mode "+narrow.Discovery, "want lazy, grouped or full", reqID)
		return
	}

	start := time.Now()
	err = s.opts.Sessions.Mutate(r.Context(), session.SessionID(id), func(ov *scope.Overlay) {
		applyNarrow(ov, narrow)
	})
	s.auditScope(r, id, body, err, time.Since(start))

	switch {
	case err == nil:
		writeOK(w, http.StatusOK, struct{}{})
	case errors.Is(err, session.ErrNotFound):
		// Uniform 404: an unknown session reads exactly like an unknown
		// route (anti-probing).
		writeNotFound(w, r)
	case errors.Is(err, session.ErrLoosening):
		writeErr(w, http.StatusForbidden, CodeTightenOnly, err.Error(),
			"scope overlays may only narrow; widening requires a human grant", reqID)
	default:
		writeErr(w, http.StatusInternalServerError, CodeInternal, err.Error(), "", reqID)
	}
}

// auditScope records the control-plane write (canonical.md: every control-plane
// action goes through audit). The record binds the exact request body via ArgsHash and carries
// the request id for `agenthub audit tail --request-id` correlation.
func (s *Server) auditScope(r *http.Request, sessionID string, body []byte, mutErr error, dur time.Duration) {
	if s.opts.Audit == nil {
		return
	}
	decision := audit.DecisionAllowed
	if mutErr != nil {
		decision = audit.DecisionDenied
	}
	hash, err := audit.ArgsHash(body)
	if err != nil {
		// The body already json-decoded, so this cannot happen; record an
		// explicit marker rather than dropping the audit line.
		hash = "unhashable"
	}
	clientID := ""
	if sess, ok := s.opts.Sessions.Get(session.SessionID(sessionID)); ok {
		clientID = sess.ClientID
	}
	s.opts.Audit.Append(audit.Record{
		Actor:     actorFrom(r.Context()),
		Client:    clientID,
		Session:   sessionID,
		Server:    "",
		Tool:      "sessions/scope",
		ArgsHash:  hash,
		Decision:  decision,
		DurMs:     dur.Milliseconds(),
		RequestID: requestIDFrom(r.Context()),
	})
}

// readBody reads a bounded request body (limit maxBodyBytes; exceeding it
// is a client error, not a server truncation).
func readBody(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	return b, nil
}

// ScopeNarrowWire is the request body of POST /v1/sessions/{id}/scope: the
// public api.ScopeNarrow plus Discovery.
//
// Discovery lives here rather than in api.ScopeNarrow because it is NOT a
// narrowing: it is an experience field (docs/architecture.md §7) that the session
// package's tighten-only check lets move freely, so it does not belong in a
// DTO whose contract is "narrow-only". The CLI posts this shape directly;
// the public api client keeps sending api.ScopeNarrow, which decodes into
// this struct unchanged (embedded, same JSON keys).
type ScopeNarrowWire struct {
	api.ScopeNarrow
	// Discovery overrides the session's discovery mode ("" = no change).
	Discovery string `json:"discovery,omitempty"`
}

// validDiscoveryMode reports whether m is one of the three frozen modes.
// Failure direction: an unknown mode is REFUSED rather than silently
// ignored — silently keeping the old mode would tell the operator the
// override took effect when it did not.
func validDiscoveryMode(m string) bool {
	switch scope.DiscoveryMode(m) {
	case scope.DiscoveryLazy, scope.DiscoveryGrouped, scope.DiscoveryFull:
		return true
	}
	return false
}

// applyNarrow translates the wire request into overlay edits. Every
// security-field edit moves in the NARROWING direction by construction;
// anything the translation cannot express as a narrowing is still caught by
// the session package's tighten-only validation (defense in depth — this
// function shapes, Mutate enforces).
func applyNarrow(ov *scope.Overlay, req ScopeNarrowWire) {
	if req.Reset {
		// Drop all narrowing. If the previous overlay narrowed anything,
		// Mutate classifies this as loosening and rejects without a human
		// grant — restoring the baseline is the approval flow's job (M1-C).
		// Reset runs FIRST so a discovery override sent alongside it
		// survives instead of being wiped by the reset that follows it.
		*ov = scope.Overlay{Version: ov.Version}
	}
	if req.Discovery != "" {
		d := scope.DiscoveryMode(req.Discovery)
		ov.Discovery = &d
	}
	if req.Reset {
		return
	}
	for _, id := range req.DisableServers {
		// Removing a server from view = block-all tool selector (Allow []),
		// plus removal from an existing allow-list. Both are pure
		// narrowings regardless of prior state. Existing Deny entries are
		// preserved: dropping one would read as loosening.
		if ov.Servers != nil {
			ov.Servers = slices.DeleteFunc(ov.Servers, func(s string) bool { return s == id })
		}
		sel := selectorFor(ov, id)
		sel.Allow = []string{}
	}
	for id, tools := range req.Tools {
		sel := selectorFor(ov, id)
		// The requested set REPLACES the allow list. If it is not a subset
		// of the previous one, Mutate rejects the whole mutation
		// (fail-closed: the daemon refuses to widen, it does not silently
		// intersect).
		sel.Allow = append([]string(nil), tools...)
	}
}

// selectorFor returns (creating if needed) the overlay's selector for id,
// preserving any existing Deny entries.
func selectorFor(ov *scope.Overlay, id string) *scope.ToolSelector {
	if ov.Tools == nil {
		ov.Tools = make(map[string]*scope.ToolSelector)
	}
	sel := ov.Tools[id]
	if sel == nil {
		sel = &scope.ToolSelector{}
		ov.Tools[id] = sel
	}
	return sel
}
