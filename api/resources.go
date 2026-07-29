package api

import (
	"context"
	"net/http"
	"net/url"
)

// ServersService accesses the configured downstream servers.
type ServersService struct{ c *Client }

// List returns all configured servers with their live state and Health
// display contract (docs/modules/controlplane.md). The same payload is pushed on the
// `servers` SSE topic — either source is authoritative.
func (s *ServersService) List(ctx context.Context) ([]Server, error) {
	var out []Server
	if err := s.c.do(ctx, http.MethodGet, "/servers", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SessionsService accesses live sessions registered with the daemon.
// Sessions are runtime objects (never persisted), so all calls require a
// running daemon.
type SessionsService struct{ c *Client }

// List returns all live sessions.
func (s *SessionsService) List(ctx context.Context) ([]SessionInfo, error) {
	var out []SessionInfo
	if err := s.c.do(ctx, http.MethodGet, "/sessions", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ScopeNarrow tightens a session's scope overlay. The public API is
// narrow-only (ruling #8): agents may only shrink their own visibility or
// restore the static baseline; temporary widening is a human grant that
// goes through the approval flow, never through this call. The daemon
// rejects any request that would widen scope (fail-closed on its side).
type ScopeNarrow struct {
	// DisableServers removes whole servers from the session's view.
	DisableServers []string `json:"disable_servers,omitempty"`
	// Tools restricts a server to the given tool subset
	// (serverID -> tool names).
	Tools map[string][]string `json:"tools,omitempty"`
	// Reset drops the overlay and restores the static scope baseline.
	Reset bool `json:"reset,omitempty"`
}

// SetScope applies a narrow-only scope overlay to the session with the
// short id "client:seq". Overlays are volatile: they disappear on daemon
// restart and are never persisted (ruling #6).
func (s *SessionsService) SetScope(ctx context.Context, sessionID string, narrow ScopeNarrow) error {
	return s.c.do(ctx, http.MethodPost,
		"/sessions/"+url.PathEscape(sessionID)+"/scope", nil, narrow, nil)
}
