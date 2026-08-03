package httpbridge

import (
	"testing"
	"time"
)

// The session table's own lifecycle reporting. These tests are in-package
// because the table is: the callback exists so the HTTP server can record a
// session leaving whichever way it left, and every way it can leave is here.
// A session that times out must leave a record. Before it did, an expiry was
// the one way a session could vanish with nothing anywhere to say so, which
// reads exactly like a session that was never opened.
func TestExpiredSessionIsReportedOnce(t *testing.T) {
	clk := time.Unix(1000, 0)
	s := newSessions(time.Minute, 8, func() time.Time { return clk })
	var closed []string
	s.closed = func(sess *Session, reason string) {
		closed = append(closed, sess.ID+":"+reason)
	}
	c := &Caller{Kind: CallerAgent, Token: "t1"}
	sess, err := s.create(c)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	clk = clk.Add(2 * time.Minute)
	if _, ok := s.get(sess.ID, c); ok {
		t.Fatal("an expired session was resolved")
	}
	if _, ok := s.get(sess.ID, c); ok {
		t.Fatal("an expired session was resolved on the second look")
	}

	if len(closed) != 1 || closed[0] != sess.ID+":"+reasonExpired {
		t.Fatalf("closed = %v, want exactly one expiry for %s", closed, sess.ID)
	}
}

// The explicit end says so, and says it differently: a client that closed its
// session and one that walked away are different facts about the client.
func TestDroppedSessionIsReportedAsClosed(t *testing.T) {
	s := newSessions(time.Minute, 8, func() time.Time { return time.Unix(1000, 0) })
	var closed []string
	s.closed = func(sess *Session, reason string) { closed = append(closed, reason) }
	c := &Caller{Kind: CallerAgent, Token: "t1"}
	sess, err := s.create(c)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if !s.drop(sess.ID, c) {
		t.Fatal("drop refused the owner")
	}

	if len(closed) != 1 || closed[0] != reasonClosed {
		t.Fatalf("closed = %v, want one %q", closed, reasonClosed)
	}
}

// A foreign probe must not be able to end somebody else's session — and must
// not be able to make the timeline claim it did.
func TestForeignDropReportsNothing(t *testing.T) {
	s := newSessions(time.Minute, 8, func() time.Time { return time.Unix(1000, 0) })
	var closed int
	s.closed = func(*Session, string) { closed++ }
	owner := &Caller{Kind: CallerAgent, Token: "t1"}
	sess, err := s.create(owner)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if s.drop(sess.ID, &Caller{Kind: CallerAgent, Token: "t2"}) {
		t.Fatal("a foreign caller dropped somebody else's session")
	}
	if closed != 0 {
		t.Fatalf("a refused drop reported %d closures", closed)
	}
}
