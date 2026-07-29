package shaping

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"
)

// Owner is the session a cursor is bound to (session.SessionID as a string).
// It is the ONLY isolation fetch_result has: cursor ids are a guessable
// sequence by design (docs/flows.md).
type Owner string

// ErrNotFound is the single error every failed lookup returns: unknown id,
// expired entry, wrong owner, malformed id, unreadable file. Store
// implementations must not distinguish these — Fetch renders them all as
// notFoundText, and a Store that leaked the difference through error types
// would let a caller probe another session's cursor space.
var ErrNotFound = errors.New("shaping: cursor not found")

// Entry is one retained result. It is what a Store persists.
//
// The on-disk encoding is NOT this struct (see fileRecord in filestore.go):
// the wire form is spelled out separately so a field rename here can never
// silently invalidate every cursor on disk.
type Entry struct {
	// ID is the cursor id.
	ID string
	// Owner binds the entry to a session and is verified on every read.
	Owner Owner
	// CreatedAt is the shaping time (UTC).
	CreatedAt time.Time
	// TTL bounds the entry's life.
	TTL time.Duration
	// Budget governed page 1 and governs every later page, so it travels
	// with the entry instead of being re-derived per fetch.
	Budget Budget
	// Full is the retained payload: the linearized full result text.
	Full string
}

// ExpiresAt is the instant after which the entry is gone.
func (e Entry) ExpiresAt() time.Time { return e.CreatedAt.Add(e.TTL) }

// Expired reports whether the entry is past its TTL at now. A non-positive
// TTL counts as expired: an entry with no life left must never be served.
func (e Entry) Expired(now time.Time) bool {
	if e.TTL <= 0 {
		return true
	}
	return !now.Before(e.ExpiresAt())
}

// ownedBy reports whether the entry belongs to owner, compared in constant
// time. This comparison is the isolation boundary, so it must not
// short-circuit on the first differing byte.
func (e Entry) ownedBy(owner Owner) bool {
	a, b := []byte(e.Owner), []byte(owner)
	return subtle.ConstantTimeCompare(a, b) == 1
}

// Store retains shaped remainders so fetch_result can page through them.
//
// Two implementations ship: MemStore for the stdio gateway (the process IS
// the session, so cursor lifetime is naturally aligned) and FileStore for
// the daemon's HTTP face (cursors must outlive a daemon restart within the
// session TTL). Implementations are safe for concurrent use.
type Store interface {
	// NextID mints the next cursor id. Ids are a plain sequence and are
	// guessable BY DESIGN; owner verification in Get is the isolation.
	NextID() string
	// Put retains an entry, replacing any entry with the same id.
	Put(ctx context.Context, e Entry) error
	// Get returns the entry for (owner, id). Every failure — unknown,
	// expired, wrong owner, unreadable — must return ErrNotFound and
	// nothing more specific.
	Get(ctx context.Context, owner Owner, id string) (Entry, error)
	// Sweep drops entries that expired at now (and, for durable stores,
	// unreadable ones). It returns how many were dropped.
	Sweep(ctx context.Context, now time.Time) (int, error)
}

// Retain hands a cursor's payload to the store. A zero cursor is a no-op,
// so callers can retain unconditionally after Shape.
func Retain(ctx context.Context, store Store, c Cursor) error {
	if store == nil || c.IsZero() {
		return nil
	}
	return store.Put(ctx, c.Entry())
}

// cursorIDFormat is the frozen cursor wire shape: "rc-" plus a zero-padded
// decimal sequence, at least six digits. Golden-tested. The sequence is
// process-global (not per-owner) and deliberately guessable.
const cursorIDFormat = "rc-%06d"

// formatID renders sequence n as a cursor id.
func formatID(n uint64) string { return fmt.Sprintf(cursorIDFormat, n) }

// validID reports whether id could have been minted by formatID. It is a
// path-safety check as much as a shape check: FileStore turns an id into a
// filename, so anything with a separator, a dot segment or an unexpected
// byte must be rejected before it reaches the filesystem.
func validID(id string) bool {
	const prefix = "rc-"
	if len(id) < len(prefix)+6 || id[:len(prefix)] != prefix {
		return false
	}
	for i := len(prefix); i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}
