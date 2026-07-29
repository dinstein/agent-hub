package httpbridge

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/tier"
)

// Storage layout under the data directory. The key file is a dotfile and
// sits BESIDE the token list rather than inside it: an operator who copies
// tokens.json around (to a bug report, to another machine) does not carry
// the ability to verify its digests with it.
const (
	// TokensFileName holds the token records (0600).
	TokensFileName = "tokens.json"
	// KeyFileName holds the HMAC key, 32 random bytes as hex (0600).
	KeyFileName = ".token_key"
	// lockFileName guards the read-modify-write transaction across
	// processes (CLI and daemon both write).
	lockFileName = ".tokens.lock"
)

// MaxTokens bounds the store. The limit is not a resource guard — 64 records
// are nothing — it is a governance guard: an unbounded credential list is a
// list nobody audits, and every token past the first few is one a human
// stopped reasoning about.
const MaxTokens = 64

// MaxTokenNameLen bounds a token name.
const MaxTokenNameLen = 64

// DefaultLockTimeout bounds how long a transaction waits for the file lock.
const DefaultLockTimeout = 5 * time.Second

// Store errors. They are sentinels so the CLI can map them to exit codes
// without matching on message text.
var (
	// ErrTokenExists means the name is already taken — including by a
	// REVOKED token, whose name stays reserved so audit records keep
	// resolving to exactly one credential.
	ErrTokenExists = errors.New("httpbridge: a token with that name already exists")
	// ErrTokenNotFound means no stored token carries that name.
	ErrTokenNotFound = errors.New("httpbridge: no token with that name")
	// ErrTooManyTokens means the store is at MaxTokens.
	ErrTooManyTokens = errors.New("httpbridge: token limit reached")
	// ErrInvalidName means the name is empty, too long or not [A-Za-z0-9._-].
	ErrInvalidName = errors.New("httpbridge: invalid token name")
	// ErrInvalidTier means the tier is not read | write | destructive.
	ErrInvalidTier = errors.New("httpbridge: invalid token tier")
	// ErrAlreadyRevoked means the token was revoked before this call.
	ErrAlreadyRevoked = errors.New("httpbridge: token is already revoked")
)

// tokensFile is the on-disk document. It is a plain envelope rather than a
// registry Doc[T]: the token list is a security artefact written by exactly
// two commands, not user-editable configuration that must survive round
// trips through older binaries.
type tokensFile struct {
	Tokens []Token `json:"tokens"`
}

// Store persists agent tokens. Safe for concurrent use inside one process
// (mu) and across processes (an flock'd sibling file around every
// read-modify-write). Uniqueness and the MaxTokens ceiling are checked
// INSIDE that transaction — checking them against a snapshot read earlier
// would let two concurrent `token create` calls both win.
type Store struct {
	dir     string
	timeout time.Duration

	mu  sync.Mutex
	key []byte
}

// OpenStore opens (and, on first use, initialises) the token store rooted at
// dir — the agenthub data directory. It creates the HMAC key if absent.
//
// Failure direction: an unreadable or malformed key file is a hard error.
// Regenerating the key would silently invalidate every issued token, which
// looks exactly like a working store to the operator and like a service
// outage to every agent.
func OpenStore(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("httpbridge: token store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("httpbridge: token store dir: %w", err)
	}
	s := &Store{dir: dir, timeout: DefaultLockTimeout}
	if _, err := s.hmacKey(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetLockTimeout overrides the transaction lock timeout (tests shrink it).
func (s *Store) SetLockTimeout(d time.Duration) {
	if d > 0 {
		s.timeout = d
	}
}

// Path returns the token file path (diagnostics and doctor output).
func (s *Store) Path() string { return filepath.Join(s.dir, TokensFileName) }

// KeyPath returns the HMAC key file path.
func (s *Store) KeyPath() string { return filepath.Join(s.dir, KeyFileName) }

// hmacKey loads the key, creating it on first use. The create path is
// O_EXCL: two processes racing to initialise the store must not each write a
// different key, with the loser's tokens silently unverifiable.
func (s *Store) hmacKey() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil {
		return s.key, nil
	}
	path := s.KeyPath()
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, derr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if derr != nil || len(key) < tokenBytes {
			return nil, fmt.Errorf("httpbridge: %s is malformed; refusing to regenerate it "+
				"(that would invalidate every issued token)", path)
		}
		s.key = key
		return key, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("httpbridge: reading %s: %w", path, err)
	}

	key := make([]byte, tokenBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("httpbridge: generating token key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Lost the initialisation race: the winner's key is the truth.
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil, fmt.Errorf("httpbridge: reading %s: %w", path, rerr)
			}
			decoded, derr := hex.DecodeString(strings.TrimSpace(string(raw)))
			if derr != nil || len(decoded) < tokenBytes {
				return nil, fmt.Errorf("httpbridge: %s is malformed", path)
			}
			s.key = decoded
			return decoded, nil
		}
		return nil, fmt.Errorf("httpbridge: creating %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return nil, fmt.Errorf("httpbridge: writing %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("httpbridge: writing %s: %w", path, err)
	}
	s.key = key
	return key, nil
}

// CreateSpec describes a token to mint.
type CreateSpec struct {
	Name string
	Tier tier.Tier
	// Servers is the allowlist; nil = every server. An empty non-nil slice
	// is accepted and allows nothing (see Token.AllowsServer).
	Servers []string
	// Profile pins the token to a profile ("" = no pin).
	Profile string
	// ExpiresAt is the hard deadline (zero = never).
	ExpiresAt time.Time
	// Now overrides the clock (tests).
	Now time.Time
}

// Create mints one token, stores its HMAC and returns the record together
// with the PLAINTEXT. The plaintext is returned exactly once and is never
// recoverable afterwards: nothing in the store can reproduce it.
func (s *Store) Create(ctx context.Context, spec CreateSpec) (Token, string, error) {
	if err := validateName(spec.Name); err != nil {
		return Token{}, "", err
	}
	if !tier.Valid(spec.Tier) {
		return Token{}, "", fmt.Errorf("%w: %q (want read, write or destructive)", ErrInvalidTier, string(spec.Tier))
	}
	key, err := s.hmacKey()
	if err != nil {
		return Token{}, "", err
	}
	now := spec.Now
	if now.IsZero() {
		now = time.Now()
	}
	plaintext, err := mint()
	if err != nil {
		return Token{}, "", err
	}
	tok := Token{
		Name:      spec.Name,
		Hash:      hashToken(key, plaintext),
		Prefix:    displayPrefix(plaintext),
		Tier:      spec.Tier,
		Servers:   normalizeServers(spec.Servers),
		Profile:   strings.TrimSpace(spec.Profile),
		CreatedAt: now.UTC().Truncate(time.Second),
	}
	if !spec.ExpiresAt.IsZero() {
		tok.ExpiresAt = spec.ExpiresAt.UTC().Truncate(time.Second)
	}

	err = s.transact(ctx, func(f *tokensFile) error {
		// Uniqueness and the ceiling are checked HERE, inside the lock, over
		// the list this same transaction is about to write back.
		for _, t := range f.Tokens {
			if t.Name == tok.Name {
				return fmt.Errorf("%w: %q", ErrTokenExists, tok.Name)
			}
		}
		if len(f.Tokens) >= MaxTokens {
			return fmt.Errorf("%w: %d tokens stored (max %d)", ErrTooManyTokens, len(f.Tokens), MaxTokens)
		}
		f.Tokens = append(f.Tokens, tok)
		return nil
	})
	if err != nil {
		return Token{}, "", err
	}
	return tok, plaintext, nil
}

// Revoke marks a token revoked. The record survives (see Token.RevokedAt).
func (s *Store) Revoke(ctx context.Context, name string, now time.Time) (Token, error) {
	if now.IsZero() {
		now = time.Now()
	}
	var out Token
	err := s.transact(ctx, func(f *tokensFile) error {
		for i := range f.Tokens {
			if f.Tokens[i].Name != name {
				continue
			}
			if f.Tokens[i].Revoked() {
				return fmt.Errorf("%w: %q", ErrAlreadyRevoked, name)
			}
			f.Tokens[i].RevokedAt = now.UTC().Truncate(time.Second)
			out = f.Tokens[i]
			return nil
		}
		return fmt.Errorf("%w: %q", ErrTokenNotFound, name)
	})
	return out, err
}

// List returns every stored token, sorted by name. Records carry the HMAC,
// not the plaintext; callers that render them must print Prefix only.
func (s *Store) List() ([]Token, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	sort.Slice(f.Tokens, func(i, j int) bool { return f.Tokens[i].Name < f.Tokens[j].Name })
	return f.Tokens, nil
}

// ActiveCount reports how many stored tokens could authenticate at now. It
// is what AuthorizeBind consults: "is there anybody who could legitimately
// connect to this listener".
func (s *Store) ActiveCount(now time.Time) (int, error) {
	toks, err := s.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range toks {
		if t.Active(now) {
			n++
		}
	}
	return n, nil
}

// Lookup resolves a presented plaintext to its stored token. ok=false covers
// every miss identically — unknown, revoked, expired — because the caller
// answers all of them with the same 401 (an authentication surface that
// distinguishes "wrong" from "expired" is an oracle).
//
// The comparison runs over the whole list with hmac.Equal and does not
// short-circuit on the first mismatch: the loop's duration must not depend
// on WHERE a match sits.
func (s *Store) Lookup(plaintext string, now time.Time) (Token, bool, error) {
	key, err := s.hmacKey()
	if err != nil {
		return Token{}, false, err
	}
	f, err := s.load()
	if err != nil {
		return Token{}, false, err
	}
	want := []byte(hashToken(key, plaintext))
	var found Token
	ok := false
	for _, t := range f.Tokens {
		if hmac.Equal([]byte(t.Hash), want) && t.Active(now) {
			found, ok = t, true
		}
	}
	return found, ok, nil
}

// load reads the token file. A missing file is an empty store (first run),
// never an error. A malformed file IS an error: silently treating corrupt
// credential storage as "no tokens" would fail OPEN for bind authorization.
func (s *Store) load() (*tokensFile, error) {
	raw, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return &tokensFile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("httpbridge: reading %s: %w", s.Path(), err)
	}
	var f tokensFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("httpbridge: %s is malformed: %w", s.Path(), err)
	}
	return &f, nil
}

// transact runs fn over the token file under the cross-process lock and
// writes the result back atomically. fn returning an error aborts the whole
// transaction — nothing is written.
func (s *Store) transact(ctx context.Context, fn func(*tokensFile) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	lock, err := acquireLock(ctx, filepath.Join(s.dir, lockFileName), s.timeout)
	if err != nil {
		return err
	}
	defer lock.release()

	f, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	sort.Slice(f.Tokens, func(i, j int) bool { return f.Tokens[i].Name < f.Tokens[j].Name })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("httpbridge: encoding tokens: %w", err)
	}
	return atomicWrite(s.Path(), append(data, '\n'))
}

// validateName enforces the token-name charset. Names appear in audit
// records and CLI arguments, so they stay to a boring charset rather than
// being quoted everywhere downstream.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name must not be empty", ErrInvalidName)
	}
	if len(name) > MaxTokenNameLen {
		return fmt.Errorf("%w: %q is longer than %d characters", ErrInvalidName, name, MaxTokenNameLen)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("%w: %q contains %q (allowed: letters, digits, '.', '_', '-')",
				ErrInvalidName, name, string(r))
		}
	}
	return nil
}

// normalizeServers trims and de-duplicates an allowlist, preserving the
// nil-vs-empty distinction (nil = no restriction, empty = nothing allowed).
// A list containing the wildcard collapses to exactly the wildcard.
func normalizeServers(in []string) []string {
	if in == nil {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if s == ServerWildcard {
			return []string{ServerWildcard}
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
