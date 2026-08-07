package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The vault's cross-process lock (ruling A.3 #1: shared state across
// processes takes a file lock or an atomic rename, proven by an N-process
// acceptance test).
//
// What it guards. Every write path in this package is a whole-file
// read-modify-write: setLocked loads the entire secrets.enc map, changes one
// key and saves it back; registryAdd/registryRemove do the same to
// keyring-keys.json; and encForWrite's loadOrCreateDevKey is a
// read-then-create of secrets.enc.key. The Chain mutex serializes those
// in-process and nothing serialized them ACROSS processes, so the daemon's
// refresh coordinator rotating a token while an operator ran `agenthub
// secret set` lost one of the two writes — and what was lost was a
// credential, silently.
//
// Why a DEDICATED lock file rather than locking the data. secrets.enc,
// keyring-keys.json and secrets.enc.key are all replaced by rename
// (atomicWrite0600). Locking an inode that a concurrent writer is about to
// swap out from under you protects nothing: the winner renames a new file
// over the path and both processes then hold locks on different inodes.
// internal/ratelimit reached the same conclusion for the counter file and
// this follows it.
//
// Why one vault-wide lock rather than one per file. The keyring branch of
// setLocked has to hold kr.set and registryAdd inside ONE critical section:
// the registry's invariant is that it mutates only alongside a successful
// keyring mutation, and two locks cannot state that. A second lock would
// also introduce an acquisition order to get wrong, for no gain — vault
// writes are rare and short.
//
// What it deliberately does NOT guard: reads, and the credentials.rev
// announcement. Reads are safe unlocked because every writer publishes by
// rename, so a reader sees the whole previous file or the whole next one,
// never a splice — and Get sits on the hot path of every downstream call
// that expands a ${SECRET_X}, which is precisely where announce.go refused
// to put a cross-process lock. A write is not a hint, so it gets the lock;
// a hint is still a hint.

// vaultLockFileName is the dedicated lock file inside the secrets
// directory. It holds no content — only the lock — and is never removed:
// unlinking a lock file races the next process that just opened it.
const vaultLockFileName = "vault.lock"

const (
	// vaultLockTimeout bounds waiting for the vault lock. The longest a
	// holder can spend inside the critical section is one OS keyring
	// operation, and those are capped at DefaultKeyringTimeout by the hard
	// keyring — so three of them is room for two writers already queued
	// ahead on a machine whose keychain is answering slowly, while still
	// bounding a CLI that would otherwise wait forever on a wedged holder.
	vaultLockTimeout = 3 * DefaultKeyringTimeout
	// vaultLockPoll is the retry interval on a busy lock.
	vaultLockPoll = 10 * time.Millisecond
)

// ErrVaultLockUnsupported reports a build with no cross-process file lock.
// It is returned rather than ignored: see flock_stub.go for the failure
// direction and why no platform agenthub runs on can reach it.
var ErrVaultLockUnsupported = errors.New("secrets: no cross-process vault lock on this platform")

// vaultLock is a held cross-process exclusive lock over the whole vault.
type vaultLock struct{ f *os.File }

// VaultLockPath is the lock file for a secrets directory. Exported so a
// diagnostic can name the path a stuck write is waiting on.
func VaultLockPath(dir string) string { return filepath.Join(dir, vaultLockFileName) }

// acquireVaultLock takes the exclusive vault lock, retrying a non-blocking
// attempt until it wins, ctx is done, or vaultLockTimeout expires.
//
// Non-blocking plus poll rather than a blocking flock: every caller reaches
// here through a ctx-carrying Store method, and a blocking lock syscall
// cannot be cancelled — a wedged holder would then wedge the caller past any
// deadline it was given.
//
// Failure direction: FAIL CLOSED. Every error path returns without the lock,
// and callers must abandon the write rather than proceed unserialized.
// Losing a credential write silently — the caller believes the token is
// stored, and authentication fails later with nothing pointing here — is
// worse than a write that reports it could not run.
func acquireVaultLock(ctx context.Context, dir string) (*vaultLock, error) {
	if !crossProcessLockSupported {
		return nil, ErrVaultLockUnsupported
	}
	path := VaultLockPath(dir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("secrets: open vault lock: %w", err)
	}
	deadline := time.Now().Add(vaultLockTimeout)
	for {
		err := flockExclusiveNB(f)
		if err == nil {
			return &vaultLock{f: f}, nil
		}
		if !isWouldBlock(err) {
			_ = f.Close()
			return nil, fmt.Errorf("secrets: lock %s: %w", path, err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("secrets: lock %s: %w", path, ctxErr)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("secrets: timed out after %s waiting for %s", vaultLockTimeout, path)
		}
		time.Sleep(vaultLockPoll)
	}
}

// release drops the lock. Errors are discarded deliberately: the caller's
// write has already succeeded or failed on its own terms, and the lock is
// released by the kernel when the descriptor closes regardless.
func (l *vaultLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = flockUnlock(l.f)
	_ = l.f.Close()
}

// withVaultLock runs fn holding the vault lock. Callers hold c.mu already:
// the in-process mutex is taken OUTSIDE this one, so goroutines of one
// process queue in memory and only one of them ever competes for the file
// lock. The reverse order would have each goroutine open its own descriptor
// and contend through the filesystem — flock is per-open-file-description
// and LockFileEx is per-handle, so they would not even be spared the wait.
func (c *Chain) withVaultLock(ctx context.Context, fn func() error) error {
	dir, err := c.baseDir()
	if err != nil {
		return err
	}
	lock, err := acquireVaultLock(ctx, dir)
	if err != nil {
		return err
	}
	defer lock.release()
	return fn()
}
