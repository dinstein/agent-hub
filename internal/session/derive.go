package session

import (
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/scope"
)

// This file is the SESSION half of derived downstream instances
// (docs/subsystems/downstream.md): which derivation key a given session
// contributes on the connection
// plane. The connection-plane half — what a key means, how instances are
// pooled and reclaimed — lives in internal/downstream (derive.go, pool.go).
//
// The split follows the plane separation of docs/model.md invariant 2: a
// session decides WHICH instance executes its calls; it never decides what
// it can see. Nothing here touches scope, and DeriveKey is not part of any
// scope hash — narrowing a session must not respawn a process, and choosing
// a different instance must not change a single visible tool name.

// DeriveKey returns the connection-plane derivation key of this session for
// one server's derive mode.
//
// The empty key means "use the base instance", and it is returned for
// DeriveNone, for an unknown mode, and — deliberately — for DeriveRoot on a
// session that reports NO root. A session without a root has nothing to
// specialize on; inventing a key from its id instead would silently give it
// private state the operator asked to key by project, and would spawn one
// process per rootless session.
//
// DeriveRoot uses the session's FIRST root, normalized. Sessions sharing a
// root therefore share one instance — the point of the mode: two windows on
// the same repository must not spawn two servers. Multi-root sessions
// (rare; a client may report several) pick the first reported root rather
// than a set digest, because the key must stay readable — it is the vault
// scope name (downstream.Spec.ScopeName) an operator administers secrets
// under.
func (s *Session) DeriveKey(mode downstream.DeriveMode) downstream.DeriveKey {
	if s == nil {
		return ""
	}
	switch mode {
	case downstream.DeriveRoot:
		roots := s.Roots()
		if len(roots) == 0 {
			return ""
		}
		return downstream.RootDeriveKey(scope.NormalizePath(roots[0]))
	case downstream.DeriveSession:
		return downstream.SessionDeriveKey(string(s.ID))
	default:
		return ""
	}
}

// CascadeKeys returns the derivation keys a CLOSING session may take down
// with it ("a derived instance counts toward that session's lifecycle").
//
// Only the SESSION-keyed derivation qualifies. A root-keyed instance is
// shared by every session on that root by construction, so closing it here
// would tear down a live neighbour's connection; those instances belong to
// the pool's idle TTL, which reclaims them once nobody is left. Returning
// the narrower set is the closed direction for availability: at worst an
// instance lives 30 minutes too long, never one call too few.
func (s *Session) CascadeKeys() []downstream.DeriveKey {
	if s == nil {
		return nil
	}
	if k := s.DeriveKey(downstream.DeriveSession); k != "" {
		return []downstream.DeriveKey{k}
	}
	return nil
}
