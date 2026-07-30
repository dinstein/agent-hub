package api

import (
	"context"
	"net/http"
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
