package secrets

import (
	"context"
	"errors"
	"fmt"
)

// This file exposes the two PERSISTENT backends of the chain as individual
// Stores. It exists for exactly one caller shape: moving a credential
// between backends (Migrate), which must never be handed a *Chain.
//
// Why not a *Chain: Chain.Get consults the environment levels first, so an
// AGENTHUB_SECRET_<KEY> variable would satisfy Migrate's read-back
// verification while the destination backend held nothing at all. The
// verification would pass and the source entry would then be deleted — the
// one outcome the whole read-back step exists to prevent. Migrate's doc says
// "pass backend-level stores"; these are those stores.
//
// The environment levels are deliberately absent here and have no Store: they
// are per-process INPUT, not storage. There is nothing to write and nothing
// to delete, so a credential can never be migrated into or out of them.

// BackendKind names a persistent backend. These strings are the CLI's
// --from/--to spellings and are FROZEN: an operator's script and a stored
// diagnostic both name backends this way.
type BackendKind string

const (
	// BackendKeyring is the OS keyring (level 4).
	BackendKeyring BackendKind = "keyring"
	// BackendEncFile is secrets.enc (level 3).
	BackendEncFile BackendKind = "enc-file"
)

// BackendKinds lists the migratable backends in a stable order, for help
// text and error messages.
func BackendKinds() []BackendKind { return []BackendKind{BackendKeyring, BackendEncFile} }

// ErrBackendUnavailable reports a backend that cannot serve on this machine
// right now: no OS keyring, or no key with which to open secrets.enc.
var ErrBackendUnavailable = errors.New("secrets: backend unavailable")

// Backend returns one persistent backend as a Store.
//
// Availability is resolved EAGERLY rather than at first use, because the
// caller (migration) needs to fail before moving anything: discovering
// halfway through that the destination cannot be written is how a
// half-migrated vault happens. An unavailable backend yields
// ErrBackendUnavailable and no Store.
func (c *Chain) Backend(ctx context.Context, kind BackendKind) (Store, error) {
	switch kind {
	case BackendKeyring:
		if !c.kr.available(ctx) {
			return nil, fmt.Errorf("%w: no OS keyring on this machine", ErrBackendUnavailable)
		}
		return &keyringStore{c: c}, nil
	case BackendEncFile:
		c.mu.Lock()
		enc, active, err := c.encForRead()
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if !active {
			// encForRead activates level 3 only with AGENTHUB_SECRET_KEY set,
			// or when a dev-key file already exists. Without either there is
			// no key, and inventing one here would write a file the operator
			// never asked for.
			return nil, fmt.Errorf(
				"%w: secrets.enc is not active (set AGENTHUB_SECRET_KEY, or AGENTHUB_DEV_SECRETS=1 to use the dev key)",
				ErrBackendUnavailable)
		}
		return &encStore{c: c, enc: enc}, nil
	default:
		return nil, fmt.Errorf("secrets: unknown backend %q", kind)
	}
}

// keyringStore is the OS keyring as a Store. It maintains the key registry
// alongside every mutation, exactly as Chain.Set/Delete do — the keyring
// cannot enumerate, so a value written without its registry entry is
// invisible to `secret ls` and to any later migration.
type keyringStore struct{ c *Chain }

var _ Store = (*keyringStore)(nil)

func (s *keyringStore) Get(ctx context.Context, ref Ref) (string, bool, error) {
	if err := ref.Validate(); err != nil {
		return "", false, err
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	v, err := s.c.kr.get(ctx, ref.StorageKey())
	switch {
	case err == nil:
		return v, true, nil
	case errors.Is(err, ErrKeyringNotFound):
		return "", false, nil
	default:
		// Fail-closed, matching Chain.Get: a broken keychain is an error,
		// never a miss.
		return "", false, err
	}
}

// The mutating methods below take the cross-process vault lock for the same
// reason Chain.Set and Chain.Delete do — they are the same whole-file
// read-modify-write cycles over the same files. If anything they need it
// more: their one caller is Migrate, which reads from one backend, writes to
// the other, verifies, and only then deletes the source. A concurrent writer
// that clobbers the destination between the write and the read-back turns
// that verified handover into a delete of the last remaining copy.
//
// The Get methods stay unlocked, matching Chain.Get: writers publish by
// rename, so a reader sees one whole version or the other.

func (s *keyringStore) Set(ctx context.Context, ref Ref, val string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return s.c.withVaultLock(ctx, func() error {
		if err := s.c.kr.set(ctx, ref.StorageKey(), val); err != nil {
			return err
		}
		return s.c.registryAdd(ref.StorageKey())
	})
}

func (s *keyringStore) Delete(ctx context.Context, ref Ref) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	sk := ref.StorageKey()
	return s.c.withVaultLock(ctx, func() error {
		if err := s.c.kr.del(ctx, sk); err != nil && !errors.Is(err, ErrKeyringNotFound) {
			return err
		}
		return s.c.registryRemove(sk)
	})
}

// encStore is secrets.enc as a Store, bound to the key that encForRead
// selected when the store was built.
type encStore struct {
	c   *Chain
	enc *encFile
}

var _ Store = (*encStore)(nil)

func (s *encStore) Get(_ context.Context, ref Ref) (string, bool, error) {
	if err := ref.Validate(); err != nil {
		return "", false, err
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	m, err := s.enc.load()
	if err != nil {
		return "", false, err
	}
	v, ok := m[ref.StorageKey()]
	return v, ok, nil
}

func (s *encStore) Set(ctx context.Context, ref Ref, val string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return s.c.withVaultLock(ctx, func() error {
		m, err := s.enc.load()
		if err != nil {
			return err
		}
		m[ref.StorageKey()] = val
		return s.enc.save(m)
	})
}

func (s *encStore) Delete(ctx context.Context, ref Ref) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return s.c.withVaultLock(ctx, func() error {
		m, err := s.enc.load()
		if err != nil {
			return err
		}
		if _, ok := m[ref.StorageKey()]; !ok {
			return nil // deleting an absent entry is a no-op, matching Chain.Delete
		}
		delete(m, ref.StorageKey())
		return s.enc.save(m)
	})
}
