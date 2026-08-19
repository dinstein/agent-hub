package httpbridge

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Session bounds: TTL plus a capacity cap (docs/subsystems/controlplane.md).
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

// Reasons a session left the table, as they appear in the closed record.
const (
	reasonClosed  = "closed by the client"
	reasonExpired = "idle past the session ttl"
)

// sessions is the bounded, TTL'd session table.
type sessions struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	now  func() time.Time
	byID map[string]*Session
	// closed is called after a session leaves the table, ALWAYS outside the
	// lock: it writes a record and a log line, and reaching a file handler
	// while holding the lock every request contends on is how a slow disk
	// becomes a stalled data plane. A nil callback is usable.
	closed func(sess *Session, reason string)
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
	expired := s.sweepLocked(now)
	if len(s.byID) >= s.max {
		s.mu.Unlock()
		s.notifyClosed(expired, reasonExpired)
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
	s.mu.Unlock()
	s.notifyClosed(expired, reasonExpired)
	return sess, nil
}

// notifyClosed reports every session that left the table. Called with the
// lock released.
func (s *sessions) notifyClosed(gone []*Session, reason string) {
	if s.closed == nil {
		return
	}
	for _, sess := range gone {
		s.closed(sess, reason)
	}
}

// get resolves an id for a caller. Every miss — unknown, expired, or owned
// by a different identity — returns the same false, and the handler answers
// all of them with the one frozen 404 body (anti-probing).
func (s *sessions) get(id string, c *Caller) (*Session, bool) {
	now := s.now()
	s.mu.Lock()
	sess, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return nil, false
	}
	if now.Sub(sess.lastSeen) > s.ttl {
		delete(s.byID, id)
		s.mu.Unlock()
		s.notifyClosed([]*Session{sess}, reasonExpired)
		return nil, false
	}
	if sess.owner != c.Identity() {
		// Deliberately NOT deleted: a foreign probe must not be able to
		// destroy somebody else's session by guessing its id.
		s.mu.Unlock()
		return nil, false
	}
	sess.lastSeen = now
	// The live caller replaces the stored one on every request; the
	// fingerprint above already proved they are equivalent, and this keeps
	// the session from holding a stale *Caller alive.
	sess.Caller = c
	s.mu.Unlock()
	return sess, true
}

// touch refreshes a session's idle clock without carrying a request.
//
// A notification stream is why this exists. The TTL advances in get, on the
// way past an incoming request — and a client that is being PUSHED to sends
// none, so its session would expire underneath its own open stream. An
// unknown id is a no-op rather than an error: the session may have been
// deleted while the stream was writing, and that race resolves when the
// stream's next write fails.
//
// No ownership check, unlike get: the only caller already holds the *Session
// it resolved through get, so there is no id here that was not proven to
// belong to this caller.
func (s *sessions) touch(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.byID[id]; ok {
		sess.lastSeen = s.now()
	}
}

// drop terminates a session the caller owns. It reports whether anything was
// removed, so DELETE of an unknown id answers 404 like every other miss.
func (s *sessions) drop(id string, c *Caller) bool {
	s.mu.Lock()
	sess, ok := s.byID[id]
	if !ok || sess.owner != c.Identity() {
		s.mu.Unlock()
		return false
	}
	delete(s.byID, id)
	s.mu.Unlock()
	s.notifyClosed([]*Session{sess}, reasonClosed)
	return true
}

// len reports the live session count (tests, diagnostics).
func (s *sessions) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

// sweepLocked removes expired sessions and returns them, so the caller can
// report each one after releasing the lock. Caller holds mu.
func (s *sessions) sweepLocked(now time.Time) []*Session {
	var gone []*Session
	for id, sess := range s.byID {
		if now.Sub(sess.lastSeen) > s.ttl {
			delete(s.byID, id)
			gone = append(gone, sess)
		}
	}
	return gone
}

// newSessionID mints a random session id.
func newSessionID() (string, error) {
	buf := make([]byte, SessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
