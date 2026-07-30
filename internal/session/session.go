// Package session implements session identity and lifecycle (docs/architecture.md §7
// /4.4): the daemon-side session registry whose stdio gateways are
// remote members and whose HTTP sessions are local.
//
// Identity ruling (A.1 #7): short IDs "client:seq" for humans, a random
// 128-bit token on the protocol side of HTTP sessions.
//
// A session is identity and liveness only. What it may see is resolved from
// the registry every time it is asked, so there is nothing here to mutate
// and no way for a live session's surface to be changed.
package session

import (
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/scope"
)

// SessionID is the daemon-assigned short session identity, e.g.
// "claude-code:17" (client ID + per-client monotonic seq). It aliases
// scope.SessionID so resolver keys and session identities are one type;
// this package owns minting.
type SessionID = scope.SessionID

// Origin tells which host a session lives in (dual-mode
// gateway).
type Origin uint8

const (
	// OriginStdioGateway: an independent gateway process registered over the
	// control connection. The daemon holds the session identity; the
	// gateway mirrors and executes it.
	OriginStdioGateway Origin = iota
	// OriginHTTP: a session living inside the daemon's HTTP face
	// (Mcp-Session-Id), mutated directly in memory.
	OriginHTTP
)

// String returns the origin name for diagnostics and audit output.
func (o Origin) String() string {
	switch o {
	case OriginStdioGateway:
		return "stdio"
	case OriginHTTP:
		return "http"
	default:
		return "invalid"
	}
}

// ClientCaps records the client capabilities relevant to session handling,
// captured at initialize/registration time.
type ClientCaps struct {
	// ToolsListChanged: the client accepts notifications/tools/list_changed,
	// so scope changes can be pushed instead of waiting for the next list.
	ToolsListChanged bool
	// Roots: the client supports the MCP roots capability (project routing).
	Roots bool
}

// ControlLink is the daemon's handle to one stdio gateway's control
// connection. Implemented by the daemon layer (over ctl.sock); this package
// only defines the contract.
type ControlLink interface {
	// Close tears the control connection down. Idempotent.
	Close() error
}

// GatewayHello is what a stdio gateway reports when registering over the
// control socket.
type GatewayHello struct {
	ClientID string
	Roots    []string
	Caps     ClientCaps
}

// SessionHello is what the daemon's HTTP face reports when an initialize
// mints a new session.
type SessionHello struct {
	ClientID string
	Roots    []string
	Caps     ClientCaps
}

// Session is one live session. Identity fields (ID..StartedAt) are
// immutable after creation; mutable state (roots, overlay, last-seen) is
// accessed through methods and safe for concurrent use. Sessions must not
// be copied (contain atomics and a mutex).
type Session struct {
	ID        SessionID
	ClientID  string
	Seq       uint64
	Origin    Origin
	Caps      ClientCaps
	StartedAt time.Time
	// Link is the control connection for stdio sessions; nil for HTTP
	// (local, direct memory access).
	Link ControlLink

	// token is the HTTP protocol-side secret (getrandom 128-bit); all zero
	// for stdio sessions. Compared only in constant time via MatchToken.
	token [16]byte

	// lastSeen is unix nanoseconds; TTL reaping and touch use it.
	lastSeen atomic.Int64

	// mu serializes roots updates per session.
	mu    sync.Mutex
	roots []string
}

// Roots returns a copy of the session's current roots.
func (s *Session) Roots() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneStrings(s.roots)
}

// SetRoots replaces the session's roots (roots/list_changed). The root is a
// mutable ATTRIBUTE, never part of the ID (docs/architecture.md §7).
func (s *Session) SetRoots(roots []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roots = cloneStrings(roots)
}

// LastSeen returns the last activity time.
func (s *Session) LastSeen() time.Time { return time.Unix(0, s.lastSeen.Load()) }

func (s *Session) touch(now time.Time) { s.lastSeen.Store(now.UnixNano()) }

// TokenHex returns the protocol-side token as lowercase hex (the
// Mcp-Session-Id value), or "" for stdio sessions which have none.
func (s *Session) TokenHex() string {
	if s.Origin != OriginHTTP {
		return ""
	}
	return hex.EncodeToString(s.token[:])
}

// MatchToken reports whether tokenHex matches this session's protocol
// token, comparing in constant time. Failure direction: any malformed
// input, wrong length, or non-HTTP session yields false (deny) — never a
// partial or early-exit comparison.
func (s *Session) MatchToken(tokenHex string) bool {
	if s.Origin != OriginHTTP {
		return false
	}
	raw, err := hex.DecodeString(tokenHex)
	if err != nil || len(raw) != len(s.token) {
		return false
	}
	return subtle.ConstantTimeCompare(raw, s.token[:]) == 1
}

// Info is the read-only listing view of a session (CLI `session ls`).
type Info struct {
	ID        SessionID
	ClientID  string
	Origin    Origin
	Roots     []string
	Caps      ClientCaps
	StartedAt time.Time
	LastSeen  time.Time
}

// info snapshots the session for listing/events.
func (s *Session) info() Info {
	return Info{
		ID:        s.ID,
		ClientID:  s.ClientID,
		Origin:    s.Origin,
		Roots:     s.Roots(),
		Caps:      s.Caps,
		StartedAt: s.StartedAt,
		LastSeen:  s.LastSeen(),
	}
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
