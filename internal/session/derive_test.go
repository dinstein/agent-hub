package session

import (
	"context"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
)

// TestSessionDeriveKey covers the session half of docs/modules/dataplane.md: which
// derivation key a session contributes per mode, including the two "use the
// base instance" answers that must not become invented keys.
func TestSessionDeriveKey(t *testing.T) {
	t.Parallel()
	m := NewMemoryManager(Options{})
	s, err := m.OpenHTTP(context.Background(), SessionHello{
		ClientID: "cursor",
		Roots:    []string{"/w/app/"},
	})
	if err != nil {
		t.Fatalf("OpenHTTP: %v", err)
	}

	if got := s.DeriveKey(downstream.DeriveNone); got != "" {
		t.Fatalf("DeriveNone key = %q, want the base instance", got)
	}
	if got, want := s.DeriveKey(downstream.DeriveRoot), downstream.RootDeriveKey("/w/app"); got != want {
		t.Fatalf("root key = %q, want %q (normalized, trailing slash dropped)", got, want)
	}
	if got, want := s.DeriveKey(downstream.DeriveSession), downstream.SessionDeriveKey(string(s.ID)); got != want {
		t.Fatalf("session key = %q, want %q", got, want)
	}

	// A rootless session derives nothing on the root mode: inventing a key
	// from its id would give it private state the operator asked to key by
	// project, and would spawn one process per rootless session.
	rootless, err := m.OpenHTTP(context.Background(), SessionHello{ClientID: "cursor"})
	if err != nil {
		t.Fatalf("OpenHTTP: %v", err)
	}
	if got := rootless.DeriveKey(downstream.DeriveRoot); got != "" {
		t.Fatalf("rootless root key = %q, want the base instance", got)
	}

	// The root is a mutable ATTRIBUTE: moving it moves the derivation.
	s.SetRoots([]string{"/w/other"})
	if got, want := s.DeriveKey(downstream.DeriveRoot), downstream.RootDeriveKey("/w/other"); got != want {
		t.Fatalf("root key after SetRoots = %q, want %q", got, want)
	}
}

// TestSessionCascadeKeys: a closing session takes only its PRIVATE
// (session-keyed) instances down. A root-keyed instance is shared with
// every other session on that root, so tearing it down here would kill a
// live neighbour's connection.
func TestSessionCascadeKeys(t *testing.T) {
	t.Parallel()
	m := NewMemoryManager(Options{})
	s, err := m.OpenHTTP(context.Background(), SessionHello{ClientID: "cursor", Roots: []string{"/w/app"}})
	if err != nil {
		t.Fatalf("OpenHTTP: %v", err)
	}
	keys := s.CascadeKeys()
	if len(keys) != 1 || keys[0] != downstream.SessionDeriveKey(string(s.ID)) {
		t.Fatalf("CascadeKeys = %v, want only the session-keyed derivation", keys)
	}
	for _, k := range keys {
		if k == downstream.RootDeriveKey("/w/app") {
			t.Fatal("a shared root-keyed instance must not be cascaded")
		}
	}
}
