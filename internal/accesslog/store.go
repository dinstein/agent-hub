package accesslog

import (
	"crypto/rand"
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

// Store owns one process's payload packs and its handles to the shared daily
// event files. It is safe for concurrent calls.
type Store struct {
	root       string
	key        []byte
	keyID      string
	durability Durability
	maxPack    int64
	clock      func() time.Time
	bootID     string

	mu     sync.Mutex
	closed bool
	days   map[string]*dayWriter
}

type dayWriter struct {
	events *os.File
	pack   *packWriter
}

// DefaultDir resolves <data>/audit.
func DefaultDir(res *platform.Resolver) (string, error) {
	if res == nil {
		res = platform.Default()
	}
	data, err := res.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(data, DirectoryName), nil
}

// Open opens a process-local store over the shared audit root.
func Open(opts Options) (*Store, error) {
	if opts.Root == "" {
		return nil, errors.New("accesslog: empty root")
	}
	if len(opts.Key) != 32 {
		return nil, ErrBadKey
	}
	if opts.KeyID == "" {
		return nil, errors.New("accesslog: empty key id")
	}
	if opts.Durability == "" {
		opts.Durability = DurabilitySync
	}
	if opts.Durability != DurabilitySync && opts.Durability != DurabilityWrite {
		return nil, fmt.Errorf("accesslog: unknown durability %q", opts.Durability)
	}
	if opts.MaxPackBytes <= 0 {
		opts.MaxPackBytes = DefaultMaxPackBytes
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if err := platform.EnsureDir(opts.Root); err != nil {
		return nil, err
	}
	boot := make([]byte, 8)
	if _, err := rand.Read(boot); err != nil {
		return nil, fmt.Errorf("accesslog: boot id: %w", err)
	}
	key := append([]byte(nil), opts.Key...)
	return &Store{
		root: opts.Root, key: key, keyID: opts.KeyID, durability: opts.Durability,
		maxPack: opts.MaxPackBytes, clock: opts.Clock, bootID: hex.EncodeToString(boot),
		days: map[string]*dayWriter{},
	}, nil
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
		return "", fmt.Errorf("accesslog: call id: %w", err)
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
	dir := filepath.Join(s.root, day)
	if err := platform.EnsureDir(dir); err != nil {
		return nil, err
	}
	events, err := os.OpenFile(filepath.Join(dir, EventFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("accesslog: open events: %w", err)
	}
	pack, err := newPackWriter(dir, s.bootID, s.key, s.keyID, s.maxPack)
	if err != nil {
		_ = events.Close()
		return nil, err
	}
	d := &dayWriter{events: events, pack: pack}
	s.days[day] = d
	return d, nil
}

// PutPayload compresses and encrypts raw, returning a durable reference when
// the store runs in sync mode.
func (s *Store) PutPayload(ts time.Time, callID string, kind PayloadKind, raw []byte) (PayloadRef, error) {
	if s == nil {
		return PayloadRef{}, ErrClosed
	}
	if len(raw) > MaxPayloadBytes {
		return PayloadRef{}, fmt.Errorf("%w: %d bytes", ErrPayloadTooBig, len(raw))
	}
	if len(callID) == 0 || len(callID) > 128 {
		return PayloadRef{}, errors.New("accesslog: invalid call id")
	}
	switch kind {
	case PayloadRequest, PayloadEffectiveArgs, PayloadResult:
	default:
		return PayloadRef{}, fmt.Errorf("accesslog: unknown payload kind %q", kind)
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
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("accesslog: encode event: %w", err)
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
	if _, err := d.events.Write(line); err != nil {
		return fmt.Errorf("accesslog: append event: %w", err)
	}
	if s.durability == DurabilitySync {
		if err := d.events.Sync(); err != nil {
			return fmt.Errorf("accesslog: sync event: %w", err)
		}
	}
	return nil
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
	for _, d := range s.days {
		if err := d.pack.close(); err != nil {
			errs = append(errs, err)
		}
		if err := d.events.Sync(); err != nil {
			errs = append(errs, err)
		}
		if err := d.events.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for i := range s.key {
		s.key[i] = 0
	}
	return errors.Join(errs...)
}
