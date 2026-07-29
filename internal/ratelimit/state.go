package ratelimit

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Frozen on-disk names under <data>/state. The lock file is SEPARATE from
// the data file on purpose: the data file is replaced by rename on every
// write, and an flock held on the replaced inode guards nothing.
const (
	StateFileName = "ratelimits.json"
	LockFileName  = "ratelimits.lock"
	// quarantineSuffix is appended (with a timestamp) when a corrupt state
	// file is set aside. Kept for forensics; never read back.
	quarantineSuffix = ".corrupt"
)

// stateVersion is the counter-file schema version. A file carrying an
// unknown version is treated exactly like a corrupt one: quarantined and
// restarted from empty, never half-interpreted.
const stateVersion = 1

// tokenScale converts tokens to the integer milli-tokens actually stored.
// Integers, not floats: the bytes on disk are then identical on every
// platform and the file can be golden-tested, and read-modify-write merges
// from several processes cannot drift by float rounding.
const tokenScale = 1000

// Bucket bounds keep the file small without an eviction daemon. They apply
// on every commit.
const (
	// idleTTL drops a bucket nobody has touched for this long. Any bucket
	// that idle has refilled to full anyway, so dropping it is equivalent
	// to keeping it.
	idleTTL = time.Hour
	// maxBuckets caps the file. On overflow the LEAST recently updated
	// buckets are dropped — dropping a stale bucket is safe (it re-appears
	// full), dropping a hot one would forgive an active abuser.
	maxBuckets = 4096
)

// bucket is one token bucket: milli-tokens remaining as of Updated.
type bucket struct {
	Tokens  int64 `json:"tokens"`  // milli-tokens
	Updated int64 `json:"updated"` // unix milliseconds
}

// state is the whole counter file.
type state struct {
	Version int               `json:"version"`
	Buckets map[string]bucket `json:"buckets"`
}

// encodeKey builds the on-disk counter key: rule id + the concrete key
// dimensions, "|"-joined. Components are percent-escaped so a "|" inside a
// server id can never fabricate a different key (identifiers are already
// restricted upstream — this is belt and braces, and it keeps the encoding
// total).
func encodeKey(ruleID string, k Key) string {
	var b strings.Builder
	for i, part := range [4]string{ruleID, k.Client, k.Server, k.Tool} {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(escapeComponent(part))
	}
	return b.String()
}

func escapeComponent(s string) string {
	if !strings.ContainsAny(s, "%|") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '%':
			b.WriteString("%25")
		case '|':
			b.WriteString("%7C")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Store owns the shared counter file: the flock, the read-modify-write
// cycle and the atomic replace.
//
// Concurrency: flock(2) is per open-file-description, so it already
// excludes goroutines of the same process that opened their own fd; mu is
// kept anyway so one process serializes on a mutex instead of on file
// descriptors and syscalls.
type Store struct {
	path     string
	lockPath string

	mu sync.Mutex
}

// NewStore returns a Store over dir (normally <data>/state). The directory
// is created 0700 if missing.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("ratelimit: state dir must not be empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ratelimit: ensure state dir: %w", err)
	}
	return &Store{
		path:     filepath.Join(dir, StateFileName),
		lockPath: filepath.Join(dir, LockFileName),
	}, nil
}

// Path returns the counter file path (diagnostics and tests).
func (s *Store) Path() string { return s.path }

// corruptionError reports a state file that could not be interpreted. The
// caller quarantines the file and continues from empty — fail-open, see the
// package doc.
type corruptionError struct{ err error }

func (e *corruptionError) Error() string { return "ratelimit: corrupt counter file: " + e.err.Error() }
func (e *corruptionError) Unwrap() error { return e.err }

// update runs one read-decide-write cycle under the exclusive file lock.
//
// The invariant this function exists for: fn ALWAYS sees the state that is
// on disk right now, never a cached copy. That is what turns two racing
// gateway processes from "last writer wins, counters mutually erased" into
// a correct merge.
//
// fn reports whether it changed anything. A false return skips the write
// entirely (a rejected call must not rewrite the file).
//
// A corrupt file is quarantined INSIDE the lock, before fn runs, so the
// fresh state fn produces is never the thing that gets moved aside;
// onCorrupt (may be nil) is notified with the quarantine path ("" if the
// rename failed). The returned error covers only lock and write failures.
func (s *Store) update(now time.Time, onCorrupt func(err error, quarantined string), fn func(st *state) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	lf, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("ratelimit: open lock file: %w", err)
	}
	defer func() { _ = lf.Close() }()
	if err := flockExclusive(lf); err != nil {
		return fmt.Errorf("ratelimit: lock counter file: %w", err)
	}
	defer func() { _ = flockUnlock(lf) }()

	st, readErr := s.read()
	if readErr != nil {
		quarantined := s.quarantine(now)
		if onCorrupt != nil {
			onCorrupt(readErr, quarantined)
		}
	}
	if !fn(st) {
		return nil
	}
	prune(st, now)
	return s.write(st)
}

// read loads the counter file. A missing file is the normal first-run case
// and yields an empty state with no error. A file that exists but cannot be
// interpreted yields an empty state AND a *corruptionError.
func (s *Store) read() (*state, error) {
	empty := &state{Version: stateVersion, Buckets: map[string]bucket{}}
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return empty, nil
	case err != nil:
		return empty, &corruptionError{err}
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return empty, &corruptionError{err}
	}
	if st.Version != stateVersion {
		return empty, &corruptionError{fmt.Errorf("unknown state version %d (want %d)", st.Version, stateVersion)}
	}
	if st.Buckets == nil {
		st.Buckets = map[string]bucket{}
	}
	return &st, nil
}

// write persists st atomically: temp file in the same directory, 0600,
// fsync, rename over the target, fsync of the parent directory. A reader
// racing the write sees either the old file or the new one.
func (s *Store) write(st *state) error {
	st.Version = stateVersion
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("ratelimit: encode counters: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, StateFileName+".tmp-")
	if err != nil {
		return fmt.Errorf("ratelimit: create temp counter file: %w", err)
	}
	tmpName := tmp.Name()
	fail := func(e error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return e
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("ratelimit: chmod temp counter file: %w", err))
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(fmt.Errorf("ratelimit: write counters: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("ratelimit: sync counters: %w", err))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("ratelimit: close temp counter file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("ratelimit: replace counter file: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// quarantine moves a corrupt counter file aside so the next cycle starts
// clean, keeping the bad bytes for forensics. Best effort: if it fails the
// next write replaces the file anyway.
func (s *Store) quarantine(now time.Time) string {
	dst := fmt.Sprintf("%s%s.%d", s.path, quarantineSuffix, now.UnixMilli())
	if err := os.Rename(s.path, dst); err != nil {
		return ""
	}
	return dst
}

// prune bounds the file: drop idle buckets, then cap the map size by
// dropping the least recently updated entries.
func prune(st *state, now time.Time) {
	cutoff := now.Add(-idleTTL).UnixMilli()
	for k, b := range st.Buckets {
		if b.Updated < cutoff {
			delete(st.Buckets, k)
		}
	}
	if len(st.Buckets) <= maxBuckets {
		return
	}
	keys := slices.SortedFunc(maps.Keys(st.Buckets), func(a, b string) int {
		return cmp.Or(
			cmp.Compare(st.Buckets[a].Updated, st.Buckets[b].Updated),
			strings.Compare(a, b),
		)
	})
	for _, k := range keys[:len(keys)-maxBuckets] {
		delete(st.Buckets, k)
	}
}

// refill advances a bucket to now and returns its milli-token balance.
// A bucket that does not exist yet starts FULL: a quota must not punish
// the first call after a restart or a prune.
func refill(b bucket, exists bool, capacity int64, window time.Duration, now time.Time) int64 {
	nowMs := now.UnixMilli()
	if !exists {
		return capacity
	}
	if b.Tokens >= capacity {
		return capacity
	}
	elapsed := nowMs - b.Updated
	if elapsed <= 0 {
		// Clock went backwards (NTP step, or another process with a
		// skewed clock wrote last). Do not invent tokens; keep the
		// balance and let the next call refill from the new clock.
		if b.Tokens < 0 {
			return 0
		}
		return b.Tokens
	}
	windowMs := window.Milliseconds()
	if elapsed >= windowMs {
		return capacity
	}
	// Integer refill: elapsed/window of a full capacity. Exact, platform
	// independent, and monotone in elapsed.
	tokens := b.Tokens + (elapsed*capacity)/windowMs
	if tokens > capacity {
		tokens = capacity
	}
	return tokens
}
