package httpbridge

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Session bounds: TTL plus a capacity cap (docs/modules/controlplane.md).
const (
	// SessionIDBytes is the entropy of one session id. The HTTP side uses a
	// random token rather than the human-readable `client:seq` of the stdio
	// side (ruling A.1 #7: CLI ids are for typing, protocol ids are for not
	// being guessed).
	SessionIDBytes = 16
	// DefaultSessionTTL expires an idle session.
	DefaultSessionTTL = 30 * time.Minute
	// DefaultMaxSessions bounds the table. Past it, creation fails rather
	// than evicting: silently dropping somebody else's live session to make
	// room for a new one turns a load spike into data-plane errors that
	// point at the wrong caller.
	DefaultMaxSessions = 256
)

// Session is one bound MCP session on the HTTP face.
type Session struct {
	// ID is the value carried in the Mcp-Session-Id header.
	ID string
	// Caller is the identity that created the session. It is re-checked on
	// every request as a WHOLE (Caller.Identity): a token narrowed or
	// revoked after binding must not keep its old authority.
	Caller *Caller

	created  time.Time
	lastSeen time.Time
	// owner is the frozen Caller.Identity() fingerprint of the creator.
	owner string
}

// Created reports when the session was bound.
func (s *Session) Created() time.Time { return s.created }

// sessions is the bounded, TTL'd session table.
type sessions struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	now  func() time.Time
	byID map[string]*Session
}

func newSessions(ttl time.Duration, max int, now func() time.Time) *sessions {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	if max <= 0 {
		max = DefaultMaxSessions
	}
	if now == nil {
		now = time.Now
	}
	return &sessions{ttl: ttl, max: max, now: now, byID: make(map[string]*Session)}
}

// create binds a new session to c. It sweeps expired entries first, so a
// table that filled up with abandoned sessions heals without an external
// reaper goroutine.
func (s *sessions) create(c *Caller) (*Session, error) {
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	if len(s.byID) >= s.max {
		return nil, errOverloaded
	}
	sess := &Session{
		ID:       id,
		Caller:   c,
		created:  now,
		lastSeen: now,
		owner:    c.Identity(),
	}
	s.byID[id] = sess
	return sess, nil
}

// get resolves an id for a caller. Every miss — unknown, expired, or owned
// by a different identity — returns the same false, and the handler answers
// all of them with the one frozen 404 body (anti-probing).
func (s *sessions) get(id string, c *Caller) (*Session, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	if now.Sub(sess.lastSeen) > s.ttl {
		delete(s.byID, id)
		return nil, false
	}
	if sess.owner != c.Identity() {
		// Deliberately NOT deleted: a foreign probe must not be able to
		// destroy somebody else's session by guessing its id.
		return nil, false
	}
	sess.lastSeen = now
	// The live caller replaces the stored one on every request; the
	// fingerprint above already proved they are equivalent, and this keeps
	// the session from holding a stale *Caller alive.
	sess.Caller = c
	return sess, true
}

// drop terminates a session the caller owns. It reports whether anything was
// removed, so DELETE of an unknown id answers 404 like every other miss.
func (s *sessions) drop(id string, c *Caller) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok || sess.owner != c.Identity() {
		return false
	}
	delete(s.byID, id)
	return true
}

// len reports the live session count (tests, diagnostics).
func (s *sessions) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

// sweepLocked removes expired sessions. Caller holds mu.
func (s *sessions) sweepLocked(now time.Time) {
	for id, sess := range s.byID {
		if now.Sub(sess.lastSeen) > s.ttl {
			delete(s.byID, id)
		}
	}
}

// newSessionID mints a random session id.
func newSessionID() (string, error) {
	buf := make([]byte, SessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
