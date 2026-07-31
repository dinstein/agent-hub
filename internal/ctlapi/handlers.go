package ctlapi

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/api"
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
		if !entry.Enabled {
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
	slices.SortFunc(out, func(a, b api.Server) int { return strings.Compare(a.ID, b.ID) })
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
// task).
func apiSessionInfo(in session.Info) api.SessionInfo {
	root := ""
	if len(in.Roots) > 0 {
		root = in.Roots[0]
	}
	return api.SessionInfo{
		ID:       string(in.ID),
		ClientID: in.ClientID,
		Origin:   in.Origin.String(),
		Root:     root,
		LastSeen: in.LastSeen,
	}
}

// handleSessions implements GET /v1/sessions.
func (s *Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	writeOK(w, http.StatusOK, s.sessionList())
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
// an unknown route (anti-probing).
func (s *Server) handleKillSession(w http.ResponseWriter, r *http.Request, id string) {
	sess, ok := s.opts.Sessions.Get(session.SessionID(id))
	if !ok {
		writeNotFound(w, r)
		return
	}
	clientID := sess.ClientID
	s.opts.Sessions.Close(session.SessionID(id))
	writeOK(w, http.StatusOK, KillResult{SessionID: id, ClientID: clientID, Killed: true})
}

// KillResult is the body of a successful session kill.
type KillResult struct {
	SessionID string `json:"session_id"`
	ClientID  string `json:"client_id,omitempty"`
	Killed    bool   `json:"killed"`
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
