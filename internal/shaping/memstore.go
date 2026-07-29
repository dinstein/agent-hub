package shaping

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMemStoreEntries bounds a MemStore's retained cursors. A stdio
// gateway lives as long as its client, so an unbounded cache would grow for
// the whole session; the oldest cursor is evicted instead. The bound is
// entry count, not bytes — a single result is already bounded by the 16MB
// protocol read limit.
const DefaultMemStoreEntries = 64

// MemStore retains cursors in process memory. It is the stdio gateway's
// store: the process IS the session, so cursor lifetime aligns with the
// session by construction and nothing needs to survive a restart.
type MemStore struct {
	// Clock overrides time.Now for expiry checks (test seam). Set it
	// before the store is shared across goroutines.
	Clock func() time.Time

	max int

	seq atomic.Uint64

	mu      sync.RWMutex
	entries map[string]memEntry
	// ord is the insertion counter used for eviction ordering. It is
	// separate from seq because Put may store an entry whose id was minted
	// elsewhere (or replaced), and eviction must follow insertion order.
	ord uint64
}

type memEntry struct {
	entry Entry
	ord   uint64
}

// NewMemStore builds an in-memory store holding at most max entries
// (<= 0 selects DefaultMemStoreEntries).
func NewMemStore(max int) *MemStore {
	if max <= 0 {
		max = DefaultMemStoreEntries
	}
	return &MemStore{max: max, entries: make(map[string]memEntry, max)}
}

// NextID mints the next cursor id.
func (s *MemStore) NextID() string { return formatID(s.seq.Add(1)) }

// Put retains e, evicting the oldest entry when the bound is exceeded.
func (s *MemStore) Put(ctx context.Context, e Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(e.ID) {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ord++
	s.entries[e.ID] = memEntry{entry: e, ord: s.ord}
	for len(s.entries) > s.max {
		oldest, found := "", uint64(0)
		for id, me := range s.entries {
			if oldest == "" || me.ord < found {
				oldest, found = id, me.ord
			}
		}
		delete(s.entries, oldest)
	}
	return nil
}

// Get returns the entry for (owner, id), or ErrNotFound for every failure
// mode alike (see ErrNotFound).
func (s *MemStore) Get(ctx context.Context, owner Owner, id string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if !validID(id) {
		return Entry{}, ErrNotFound
	}
	s.mu.RLock()
	me, ok := s.entries[id]
	s.mu.RUnlock()
	if !ok || !me.entry.ownedBy(owner) || me.entry.Expired(s.now()) {
		return Entry{}, ErrNotFound
	}
	return me.entry, nil
}

func (s *MemStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// Sweep drops entries expired at now.
func (s *MemStore) Sweep(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, me := range s.entries {
		if me.entry.Expired(now) {
			delete(s.entries, id)
			n++
		}
	}
	return n, nil
}

// Len reports the number of retained entries (expired ones included until
// the next Sweep). Diagnostics and tests only.
func (s *MemStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
