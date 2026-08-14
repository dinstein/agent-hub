package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	// selfWriteSlots bounds the suppression set. One slot per in-flight
	// write; 64 comfortably covers a burst of multi-document updates while
	// keeping the set trivially bounded.
	selfWriteSlots = 64

	// selfWriteTTL is how long a registered fingerprint stays valid. A
	// watcher that has not observed the write within 10s (docs/conventions.md#the-hot-reload-path)
	// no longer suppresses it.
	selfWriteTTL = 10 * time.Second
)

// selfWriteSet is the bounded TTL set of payload fingerprints backing
// self-write suppression (docs/conventions.md#the-hot-reload-path #1): the write
// path registers the fingerprint of each payload BEFORE writing it (and
// withdraws it if the write fails); the watcher consumes a matching
// fingerprint and skips the event. Generation answers "did it change";
// this set answers "was it me who changed it".
//
// Failure direction: suppression is best-effort and fails OPEN toward
// reloading — an expired TTL, an evicted slot, or a fingerprint mismatch
// causes at worst one spurious (no-op) reload of our own write. It can never
// hide an external change: a content change whose fingerprint is not in the
// set is treated as external, and any external change clears the whole set.
type selfWriteSet struct {
	mu      sync.Mutex
	now     func() time.Time // test hook; time.Now when nil
	entries []selfWriteEntry // FIFO by registration time, len <= selfWriteSlots
}

type selfWriteEntry struct {
	fp string
	at time.Time
}

func (s *selfWriteSet) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// purgeLocked drops expired entries. Caller holds s.mu.
func (s *selfWriteSet) purgeLocked(now time.Time) {
	live := s.entries[:0]
	for _, e := range s.entries {
		if now.Sub(e.at) < selfWriteTTL {
			live = append(live, e)
		}
	}
	s.entries = live
}

// register records fp as an imminent self-write. Multi-slot: each call takes
// its own slot, so N in-flight writes of identical payloads need N consumes.
// When full, the oldest entry is evicted (fail-open: the evicted write may
// cause one spurious reload).
func (s *selfWriteSet) register(fp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	s.purgeLocked(now)
	if len(s.entries) >= selfWriteSlots {
		s.entries = s.entries[1:]
	}
	s.entries = append(s.entries, selfWriteEntry{fp: fp, at: now})
}

// withdraw removes the most recently registered entry for fp. Called when the
// write that registered it failed — a fingerprint for content that never hit
// disk must not suppress a future external write of identical content.
func (s *selfWriteSet) withdraw(fp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].fp == fp {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return
		}
	}
}

// consume reports whether fp is registered (and not expired), removing the
// oldest matching slot on a hit. The watcher calls this once per observed
// content change: hit = our own write, skip the event.
func (s *selfWriteSet) consume(fp string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(s.clock())
	for i, e := range s.entries {
		if e.fp == fp {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return true
		}
	}
	return false
}

// clear drops every entry. Called when the watcher observes an external
// change: after someone else touched the registry the pending self-write
// fingerprints no longer describe the on-disk lineage, and suppressing on
// them could mask an external overwrite that happens to match a stale
// payload.
func (s *selfWriteSet) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = nil
}

// fingerprint hashes a document payload for the suppression set. Content is
// canonicalized first so the fingerprint is stable across formatting-only
// differences between what was written and what is read back. Canonicalize
// failure falls back to hashing the raw bytes: a mismatch then costs at most
// one spurious reload (fail-open, see selfWriteSet).
func fingerprint(data []byte) string {
	c, err := canonicalize(data)
	if err != nil {
		c = data
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:])
}
