package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// Store is the persistence face of the secrets subsystem.
type Store interface {
	Get(ctx context.Context, ref Ref) (val string, ok bool, err error)
	Set(ctx context.Context, ref Ref, val string) error
	Delete(ctx context.Context, ref Ref) error
}

// Resolver is the narrow face injected into internal/downstream: resolve
// one ref, nothing else. Chain.Resolver produces one.
type Resolver func(ctx context.Context, ref Ref) (string, bool, error)

// DefaultKeyringTimeout bounds every keyring operation. A hung keychain
// unlock prompt otherwise wedges the caller indefinitely.
const DefaultKeyringTimeout = 3 * time.Second

// defaultService is the keyring service name (frozen identifier).
const defaultService = "agenthub"

// ChainConfig configures NewChain. The zero value uses the real process
// environment, the real OS keyring and the default secrets directory
// (<data>/secrets).
type ChainConfig struct {
	// Dir is the directory holding secrets.enc, secrets.enc.key and the
	// keyring key registry. Empty resolves <data>/secrets via
	// internal/platform on first use.
	Dir string
	// LookupEnv overrides os.LookupEnv (tests inject a map here; the
	// chain itself never mutates the process environment).
	LookupEnv func(key string) (string, bool)
	// Keyring overrides the OS keyring backend. nil selects the real
	// zalando-backed one. Tests MUST inject a fake — only manual smoke
	// runs under AGENTHUB_TEST_REAL_KEYRING=1 touch the real keychain.
	Keyring Backend
	// KeyringTimeout overrides DefaultKeyringTimeout when > 0.
	KeyringTimeout time.Duration
	// Service overrides the keyring service name (tests / multi-instance).
	Service string
}

// Chain is the four-level resolution chain (see the package doc). It
// implements Store; List and Resolver are extra faces on the concrete
// type.
//
// The mutex serializes every enc-file read-modify-write and registry
// update in-process; cross-process coordination is the caller's concern.
type Chain struct {
	cfg ChainConfig
	kr  *hardKeyring

	mu sync.Mutex

	dirOnce sync.Once
	dir     string
	dirErr  error
}

// NewChain builds the chain. It never fails: directory resolution and
// backend probing are deferred to first use so a chain can be constructed
// unconditionally at wiring time.
//
// The concrete *Chain is returned rather than the Store interface so
// callers can reach List and Resolver; *Chain satisfies Store.
func NewChain(cfg ChainConfig) *Chain {
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = os.LookupEnv
	}
	if cfg.Service == "" {
		cfg.Service = defaultService
	}
	if cfg.KeyringTimeout <= 0 {
		cfg.KeyringTimeout = DefaultKeyringTimeout
	}
	b := cfg.Keyring
	if b == nil {
		b = systemBackend{}
	}
	return &Chain{cfg: cfg, kr: newHardKeyring(b, cfg.Service, cfg.KeyringTimeout)}
}

var _ Store = (*Chain)(nil)

// Resolver returns the narrow resolution face for downstream injection.
func (c *Chain) Resolver() Resolver { return c.Get }

// Get resolves ref through the four-level chain; first hit wins.
//
// Failure directions: an unreadable/undecryptable enc file and keyring
// errors other than not-found are surfaced as errors, never treated as a
// miss (fail-closed — a wrong AGENTHUB_SECRET_KEY or a broken keychain
// must be visible, not silently degrade to "secret not set"). A keyring
// whose availability probe failed is skipped without error: that machine
// simply has no keyring level (ruling A.6 #5 sends its writes to the enc
// file instead).
func (c *Chain) Get(ctx context.Context, ref Ref) (string, bool, error) {
	if err := ref.Validate(); err != nil {
		return "", false, err
	}
	// Levels 1–2: environment.
	if v, ok := c.envValue(ref.Key); ok {
		return v, true, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Level 3: encrypted file.
	enc, active, err := c.encForRead()
	if err != nil {
		return "", false, err
	}
	if active {
		m, err := enc.load()
		if err != nil {
			return "", false, err
		}
		if v, ok := m[ref.StorageKey()]; ok {
			return v, true, nil
		}
	}
	// Level 4: OS keyring.
	if c.kr.available(ctx) {
		v, err := c.kr.get(ctx, ref.StorageKey())
		switch {
		case err == nil:
			return v, true, nil
		case errors.Is(err, ErrKeyringNotFound):
			// miss — fall through
		default:
			return "", false, err
		}
	}
	return "", false, nil
}

// Set writes ref to the active persistent backend:
//
//  1. AGENTHUB_SECRET_KEY set → secrets.enc under the derived key
//  2. AGENTHUB_DEV_SECRETS=1 → secrets.enc under the dev key
//  3. keyring available       → OS keyring (+ key registry)
//  4. keyring probe failed    → secrets.enc under the dev key (A.6 #5)
func (c *Chain) Set(ctx context.Context, ref Ref, val string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	enc, useEnc, err := c.encForWrite(ctx)
	if err != nil {
		return err
	}
	if useEnc {
		m, err := enc.load()
		if err != nil {
			return err
		}
		m[ref.StorageKey()] = val
		return enc.save(m)
	}
	if err := c.kr.set(ctx, ref.StorageKey(), val); err != nil {
		return err
	}
	return c.registryAdd(ref.StorageKey())
}

// Delete removes ref from every writable backend it may live in (enc file
// and keyring). Deleting an absent secret is a no-op, not an error.
func (c *Chain) Delete(ctx context.Context, ref Ref) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sk := ref.StorageKey()
	enc, active, err := c.encForRead()
	if err != nil {
		return err
	}
	if active {
		m, err := enc.load()
		if err != nil {
			return err
		}
		if _, ok := m[sk]; ok {
			delete(m, sk)
			if err := enc.save(m); err != nil {
				return err
			}
		}
	}
	if c.kr.available(ctx) {
		if err := c.kr.del(ctx, sk); err != nil && !errors.Is(err, ErrKeyringNotFound) {
			return err
		}
		// Registry mutates only alongside a successful keyring mutation.
		return c.registryRemove(sk)
	}
	return nil
}

// List enumerates every stored ref: the union of the enc-file map and the
// self-managed keyring key registry (environment levels are per-process
// input, not storage, and are not listed). Undecodable keys are errors,
// not silently dropped.
func (c *Chain) List(_ context.Context) ([]Ref, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := map[string]struct{}{}
	enc, active, err := c.encForRead()
	if err != nil {
		return nil, err
	}
	if active {
		m, err := enc.load()
		if err != nil {
			return nil, err
		}
		for k := range m {
			keys[k] = struct{}{}
		}
	}
	regPath, err := c.registryPath()
	if err != nil {
		return nil, err
	}
	regKeys, err := loadKeyRegistry(regPath)
	if err != nil {
		return nil, err
	}
	for _, k := range regKeys {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	refs := make([]Ref, 0, len(sorted))
	for _, k := range sorted {
		ref, err := ParseStorageKey(k)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// --- backend selection ----------------------------------------------------

// encForRead decides whether level 3 is active for reads and with which
// key. AGENTHUB_SECRET_KEY activates it explicitly; otherwise the dev
// fallback (A.6 #5) activates it when both the enc file and the dev key
// file already exist — data the dev backend wrote must stay resolvable
// even after the keyring probe starts succeeding again.
func (c *Chain) encForRead() (*encFile, bool, error) {
	if v, ok := c.cfg.LookupEnv(EnvEncKey); ok && strings.TrimSpace(v) != "" {
		dir, err := c.baseDir()
		if err != nil {
			return nil, false, err
		}
		return &encFile{path: filepath.Join(dir, encFileName), key: deriveKey(v)}, true, nil
	}
	dir, err := c.baseDir()
	if err != nil {
		return nil, false, err
	}
	encPath := filepath.Join(dir, encFileName)
	keyPath := filepath.Join(dir, devKeyFileName)
	if fileExists(encPath) && fileExists(keyPath) {
		key, err := readDevKey(keyPath)
		if err != nil {
			return nil, false, err
		}
		return &encFile{path: encPath, key: key}, true, nil
	}
	return nil, false, nil
}

// encForWrite decides the write target: (enc, true) when the enc file is
// the destination, (nil, false) when the keyring is.
func (c *Chain) encForWrite(ctx context.Context) (*encFile, bool, error) {
	if v, ok := c.cfg.LookupEnv(EnvEncKey); ok && strings.TrimSpace(v) != "" {
		dir, err := c.baseDir()
		if err != nil {
			return nil, false, err
		}
		return &encFile{path: filepath.Join(dir, encFileName), key: deriveKey(v)}, true, nil
	}
	dev := false
	if v, ok := c.cfg.LookupEnv(EnvDevSecrets); ok && v == "1" {
		dev = true
	}
	if !dev && c.kr.available(ctx) {
		return nil, false, nil
	}
	// Dev mode requested, or keyring probe failed (ruling A.6 #5):
	// fall back to the enc file under an auto-generated persisted key.
	dir, err := c.baseDir()
	if err != nil {
		return nil, false, err
	}
	key, err := loadOrCreateDevKey(filepath.Join(dir, devKeyFileName))
	if err != nil {
		return nil, false, err
	}
	return &encFile{path: filepath.Join(dir, encFileName), key: key}, true, nil
}

// baseDir resolves and creates (0700) the secrets directory once.
func (c *Chain) baseDir() (string, error) {
	c.dirOnce.Do(func() {
		dir := c.cfg.Dir
		if dir == "" {
			data, err := platform.DataDir()
			if err != nil {
				c.dirErr = fmt.Errorf("secrets: resolve data dir: %w", err)
				return
			}
			dir = filepath.Join(data, "secrets")
		}
		if err := platform.EnsureDir(dir); err != nil {
			c.dirErr = err
			return
		}
		c.dir = dir
	})
	return c.dir, c.dirErr
}

func (c *Chain) registryPath() (string, error) {
	dir, err := c.baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, keyRegistryFileName), nil
}

func (c *Chain) registryAdd(sk string) error {
	path, err := c.registryPath()
	if err != nil {
		return err
	}
	keys, err := loadKeyRegistry(path)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k == sk {
			return nil
		}
	}
	return saveKeyRegistry(path, append(keys, sk))
}

func (c *Chain) registryRemove(sk string) error {
	path, err := c.registryPath()
	if err != nil {
		return err
	}
	keys, err := loadKeyRegistry(path)
	if err != nil {
		return err
	}
	out := keys[:0]
	found := false
	for _, k := range keys {
		if k == sk {
			found = true
			continue
		}
		out = append(out, k)
	}
	if !found {
		return nil
	}
	return saveKeyRegistry(path, out)
}
