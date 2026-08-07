package calllog

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// KeyID returns a non-secret stable identifier for one payload key.
func KeyID(key []byte) (string, error) {
	if len(key) != 32 {
		return "", ErrBadKey
	}
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8]), nil
}

// Store owns one process's payload packs and its handles to the shared daily
// event files. It is safe for concurrent calls.
type Store struct {
	root       string
	key        []byte
	keyID      string
	durability Durability
	maxPack    int64
	retention  int
	maxBytes   int64
	minFree    int64
	clock      func() time.Time
	bootID     string
	lock       *os.File

	// frames is the fail-open half: a queue and one goroutine writing this
	// process's own frame file. It is deliberately outside mu — a frame must
	// never contend with the lock a lifecycle write holds.
	frames *frames

	mu     sync.Mutex
	closed bool
	days   map[string]*dayWriter
}

type dayWriter struct {
	events *os.File
	pack   *packWriter
}

func (d *dayWriter) close() error {
	if d == nil {
		return nil
	}
	var errs []error
	if err := d.pack.close(); err != nil {
		errs = append(errs, err)
	}
	if err := d.events.Sync(); err != nil {
		errs = append(errs, err)
	}
	if err := d.events.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// DefaultDir resolves <data>/calls, migrating the pre-rename <data>/audit
// into it on the way.
func DefaultDir(res *platform.Resolver) (string, error) {
	if res == nil {
		res = platform.Default()
	}
	data, err := res.DataDir()
	if err != nil {
		return "", err
	}
	return DirFor(data), nil
}

// DirFor is the ledger root under one data directory, migrating the
// pre-rename <data>/audit into it on the way.
//
// It exists because the daemon holds a data dir rather than a resolver, and
// composed the path itself — `filepath.Join(dataDir, "audit")`. The rename
// moved the constant and left that string behind, so the CLI read
// <data>/calls while the control plane read a directory that the CLI's own
// migration had just renamed away: the GUI's Calls page went empty against a
// ledger full of records. One function, so there is no second place for the
// name to live.
func DirFor(dataDir string) string {
	dir := filepath.Join(dataDir, DirectoryName)
	migrateLegacyDir(filepath.Join(dataDir, LegacyDirectoryName), dir)
	return dir
}

// migrateLegacyDir renames <data>/audit to <data>/calls, once, on the first
// resolution after the upgrade.
//
// A rename rather than a copy: the ledger is authenticated, and two roots
// holding the same events would let a reader pick whichever one it was
// pointed at. It is silent by design — every failure leaves the old directory
// exactly where it was, and the new root simply starts empty, which is the
// one outcome that cannot lose anything.
func migrateLegacyDir(legacy, current string) {
	if legacy == current {
		return
	}
	if _, err := os.Stat(current); err == nil || !os.IsNotExist(err) {
		return
	}
	if fi, err := os.Stat(legacy); err != nil || !fi.IsDir() {
		return
	}
	_ = os.Rename(legacy, current)
}

// Open opens a process-local store over the shared ledger root.
func Open(opts Options) (*Store, error) {
	if opts.Root == "" {
		return nil, errors.New("calllog: empty root")
	}
	// A store with NO key is legitimate and is the ordinary case: it records
	// metadata, which is what makes the data plane observable by default, and
	// refuses to store payloads because there is nothing to seal them with.
	// The alternative — requiring a key before anything at all is recorded —
	// is what left an installation with no evidence AND no timeline until
	// somebody went looking for a switch.
	if len(opts.Key) > 0 {
		if len(opts.Key) != 32 {
			return nil, ErrBadKey
		}
		if opts.KeyID == "" {
			return nil, errors.New("calllog: empty key id")
		}
		keyID, err := KeyID(opts.Key)
		if err != nil {
			return nil, err
		}
		if keyID != opts.KeyID {
			return nil, fmt.Errorf("calllog: key id %q does not match key %q", opts.KeyID, keyID)
		}
	} else if opts.KeyID != "" {
		return nil, errors.New("calllog: key id without a key")
	}
	if opts.Durability == "" {
		opts.Durability = DurabilitySync
	}
	if opts.Durability != DurabilitySync && opts.Durability != DurabilityWrite {
		return nil, fmt.Errorf("calllog: unknown durability %q", opts.Durability)
	}
	if opts.MaxPackBytes <= 0 {
		opts.MaxPackBytes = DefaultMaxPackBytes
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.RetentionDays < 0 || opts.MaxBytes < 0 || opts.MinFreeBytes < 0 {
		return nil, errors.New("calllog: retention and capacity limits cannot be negative")
	}
	if (opts.RetentionDays > 0 || opts.MaxBytes > 0 || opts.MinFreeBytes > 0) && !crossProcessLockSupported {
		return nil, errors.New("calllog: bounded retention requires a cross-process lock on this platform")
	}
	if err := platform.EnsureDir(opts.Root); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(opts.Root, ".calls.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("calllog: open capacity lock: %w", err)
	}
	boot := make([]byte, 8)
	if _, err := rand.Read(boot); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("calllog: boot id: %w", err)
	}
	key := append([]byte(nil), opts.Key...)
	s := &Store{
		root: opts.Root, key: key, keyID: opts.KeyID, durability: opts.Durability,
		maxPack: opts.MaxPackBytes, retention: opts.RetentionDays, maxBytes: opts.MaxBytes,
		minFree: opts.MinFreeBytes, clock: opts.Clock, bootID: hex.EncodeToString(boot), lock: lock,
		days: map[string]*dayWriter{},
	}
	s.frames = newFrames(s)
	return s, nil
}

// BootID is the random identity stamped on this process's events and pack names.
func (s *Store) BootID() string {
	if s == nil {
		return ""
	}
	return s.bootID
}

// NewCallID returns a random, non-guessable id for one tools/call lifecycle.
func NewCallID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("calllog: call id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (s *Store) dayLocked(day string) (*dayWriter, error) {
	if s.closed {
		return nil, ErrClosed
	}
	if d := s.days[day]; d != nil {
		return d, nil
	}
	// A store writes one UTC day at a time. Close older handles before the
	// capacity pass can prune their partitions: on Unix, unlinking an open
	// pack would otherwise hide its still-allocated bytes from Inspect until
	// the process exits.
	for name, d := range s.days {
		if err := d.close(); err != nil {
			return nil, err
		}
		delete(s.days, name)
	}
	dir := filepath.Join(s.root, day)
	if err := platform.EnsureDir(dir); err != nil {
		return nil, err
	}
	events, err := os.OpenFile(filepath.Join(dir, EventFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("calllog: open events: %w", err)
	}
	// No key, no pack: a metadata-only store seals nothing, and building a
	// cipher out of an empty key is how that used to surface — as an
	// encryption error on a path that was never going to encrypt anything.
	var pack *packWriter
	if s.HasKey() {
		var err error
		pack, err = newPackWriter(dir, s.bootID, s.key, s.keyID, s.maxPack, func(added int64, write func() error) error {
			return s.withCapacityLocked(day, added, write)
		})
		if err != nil {
			_ = events.Close()
			return nil, err
		}
	}
	d := &dayWriter{events: events, pack: pack}
	s.days[day] = d
	return d, nil
}

// HasKey reports whether this store can store payloads at all. A store
// without one records metadata and nothing else.
func (s *Store) HasKey() bool { return s != nil && len(s.key) == 32 }

// PutPayload compresses and encrypts raw, returning a durable reference when
// the store runs in sync mode.
func (s *Store) PutPayload(ts time.Time, callID string, kind PayloadKind, raw []byte) (PayloadRef, error) {
	if s == nil {
		return PayloadRef{}, ErrClosed
	}
	if !s.HasKey() {
		return PayloadRef{}, ErrNoKey
	}
	if len(raw) > MaxPayloadBytes {
		return PayloadRef{}, fmt.Errorf("%w: %d bytes", ErrPayloadTooBig, len(raw))
	}
	if len(callID) == 0 || len(callID) > 128 {
		return PayloadRef{}, errors.New("calllog: invalid call id")
	}
	switch kind {
	case PayloadRequest, PayloadEffectiveArgs, PayloadResult, PayloadFrame:
	default:
		return PayloadRef{}, fmt.Errorf("calllog: unknown payload kind %q", kind)
	}
	day := utcDay(ts)
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.dayLocked(day)
	if err != nil {
		return PayloadRef{}, err
	}
	return d.pack.append(callID, kind, raw, s.durability == DurabilitySync, day)
}

// Append writes one bounded metadata event in a single O_APPEND write.
func (s *Store) Append(e Event) error {
	if s == nil {
		return ErrClosed
	}
	if e.TS.IsZero() {
		e.TS = s.clock().UTC()
	} else {
		e.TS = e.TS.UTC()
	}
	if e.Version == 0 {
		e.Version = Version
	}
	if e.PID == 0 {
		e.PID = os.Getpid()
	}
	if e.BootID == "" {
		e.BootID = s.bootID
	}
	// Unkeyed records carry no MAC and say so by leaving both fields empty.
	// `calls verify` counts them as unauthenticated, which is a different
	// answer from "authentication failed" and must not be confused with it.
	if len(s.key) == 32 {
		e.KeyID = s.keyID
		mac, err := eventMAC(e, s.key)
		if err != nil {
			return err
		}
		e.MAC = mac
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("calllog: encode event: %w", err)
	}
	if len(line)+1 > MaxEventLineBytes {
		return fmt.Errorf("%w: %d bytes", ErrEventTooLarge, len(line)+1)
	}
	line = append(line, '\n')
	day := utcDay(e.TS)
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.dayLocked(day)
	if err != nil {
		return err
	}
	return s.withCapacityLocked(day, int64(len(line)), func() error {
		if _, err := d.events.Write(line); err != nil {
			return fmt.Errorf("calllog: append event: %w", err)
		}
		if s.durability == DurabilitySync {
			if err := d.events.Sync(); err != nil {
				return fmt.Errorf("calllog: sync event: %w", err)
			}
		}
		return nil
	})
}

func eventMAC(e Event, key []byte) (string, error) {
	if len(key) != 32 {
		return "", ErrBadKey
	}
	e.MAC = ""
	raw, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("calllog: encode event mac: %w", err)
	}
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(raw)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Unauthenticated reports whether e was written by a store that held no key,
// and so carries nothing to authenticate. Such a record is the NORMAL shape on
// an installation running the metadata tier alone — which is the default — and
// a verifier that counted it as a failure would tell every stock installation
// that its history had failed authentication.
//
// The condition is BOTH fields empty, never either. Store.append sets `keyId`
// and `mac` together, so one without the other cannot come from this package:
// it is corruption, or a keyed record whose key id was stripped to make it
// look unkeyed. Both belong on the failure side, which is where leaving them
// out of this predicate puts them.
//
// Failure direction: a record this returns true for is REPORTED, never
// silently passed. What it must not be is merged with a MAC that did not check
// out — those are different findings and they lead to different actions.
func Unauthenticated(e Event) bool { return e.KeyID == "" && e.MAC == "" }

// VerifyEvent authenticates one metadata event with its payload key.
func VerifyEvent(e Event, key []byte) error {
	if e.MAC == "" || e.KeyID == "" {
		return errors.New("calllog: event has no authentication tag")
	}
	keyID, err := KeyID(key)
	if err != nil {
		return err
	}
	if keyID != e.KeyID {
		return fmt.Errorf("calllog: event key id %q does not match key %q", e.KeyID, keyID)
	}
	want, err := eventMAC(e, key)
	if err != nil {
		return err
	}
	got, err := hex.DecodeString(e.MAC)
	if err != nil || !hmac.Equal(got, mustDecodeHex(want)) {
		return errors.New("calllog: event authentication failed")
	}
	return nil
}

func mustDecodeHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

// Close fsyncs and closes every open daily writer. It is idempotent.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	// Frames first, and with the lock RELEASED: the pipeline may be inside a
	// PutPayload, which takes this same lock, and the day writers closed
	// below are what that write lands in. s.frames is left in place — a
	// concurrent AppendFrame reads it without the lock and finds a pipeline
	// that refuses rather than a nil.
	s.mu.Unlock()
	if err := s.frames.close(); err != nil {
		errs = append(errs, err)
	}
	s.mu.Lock()
	for _, d := range s.days {
		if err := d.close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.lock != nil {
		if err := s.lock.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for i := range s.key {
		s.key[i] = 0
	}
	return errors.Join(errs...)
}
