package secrets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The tests below all use the same lever: hold the vault lock from outside,
// then give the write a context that expires quickly. A write that takes the
// lock cannot proceed and reports an error; a write that skipped it sails
// through. The assertion is therefore "this write is serialized", which is
// what the lock is for — not merely "the lock file exists".

// heldLock takes the vault lock for dir and releases it when the test ends.
func heldLock(t *testing.T, dir string) {
	t.Helper()
	// The directory has to exist before the lock file can be created; a Chain
	// would do it lazily via baseDir, but these tests lock before any Chain
	// has run.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir secrets dir: %v", err)
	}
	lock, err := acquireVaultLock(t.Context(), dir)
	if err != nil {
		t.Fatalf("hold vault lock: %v", err)
	}
	t.Cleanup(lock.release)
}

// briefCtx expires soon enough to keep the suite fast and far short of
// vaultLockTimeout, so a failure means "never waited", not "waited briefly".
func briefCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func TestSetWaitsForTheVaultLock(t *testing.T) {
	dir := t.TempDir()
	heldLock(t, dir)
	c := newTestChain(dir, map[string]string{EnvEncKey: "pass"}, newFakeBackend(nil))
	if err := c.Set(briefCtx(t), Ref{ServerID: "srv", Key: "token"}, "v"); err == nil {
		t.Fatal("Set completed while another process held the vault lock")
	}
}

func TestDeleteWaitsForTheVaultLock(t *testing.T) {
	dir := t.TempDir()
	ref := Ref{ServerID: "srv", Key: "token"}
	seedEnc(t, dir, ref, "v")
	heldLock(t, dir)
	c := newTestChain(dir, map[string]string{EnvEncKey: "pass"}, newFakeBackend(nil))
	if err := c.Delete(briefCtx(t), ref); err == nil {
		t.Fatal("Delete completed while another process held the vault lock")
	}
}

// Migrate's two backend-level stores are the same read-modify-write cycles
// over the same files, and they are the paths where a lost write costs the
// last remaining copy of a credential.
func TestBackendStoreWritesWaitForTheVaultLock(t *testing.T) {
	for _, kind := range BackendKinds() {
		t.Run(string(kind), func(t *testing.T) {
			dir := t.TempDir()
			ref := Ref{ServerID: "srv", Key: "token"}
			// enc-file: a key must be active before Backend hands out a store.
			seedEnc(t, dir, ref, "v")
			c := newTestChain(dir, map[string]string{EnvEncKey: "pass"},
				newFakeBackend(map[string]string{bkey(defaultService, ref.StorageKey()): "v"}))
			store, err := c.Backend(t.Context(), kind)
			if err != nil {
				t.Fatalf("backend %s: %v", kind, err)
			}
			heldLock(t, dir)
			if err := store.Set(briefCtx(t), ref, "next"); err == nil {
				t.Fatalf("%s Set completed while another process held the vault lock", kind)
			}
			if err := store.Delete(briefCtx(t), ref); err == nil {
				t.Fatalf("%s Delete completed while another process held the vault lock", kind)
			}
		})
	}
}

// Reads must NOT take the lock: Get sits on the hot path of every downstream
// call that expands a ${SECRET_X}, and writers publish by rename so a reader
// is never spliced. A regression here would not fail loudly — it would make
// every tool call wait behind a vault write — so it is pinned.
func TestReadsDoNotTakeTheVaultLock(t *testing.T) {
	dir := t.TempDir()
	ref := Ref{ServerID: "srv", Key: "token"}
	seedEnc(t, dir, ref, "v")
	heldLock(t, dir)

	c := newTestChain(dir, map[string]string{EnvEncKey: "pass"}, newFakeBackend(nil))
	got, ok, err := c.Get(briefCtx(t), ref)
	if err != nil || !ok || got != "v" {
		t.Fatalf("Get under a held lock = (%q, %v, %v), want (\"v\", true, nil)", got, ok, err)
	}
	if _, err := c.List(briefCtx(t)); err != nil {
		t.Fatalf("List under a held lock: %v", err)
	}
}

// The dev fallback generates secrets.enc.key on first write with a
// read-then-create, and the file it seals is unreadable if two processes each
// generate one. Key selection therefore has to sit INSIDE the lock, not just
// the map update — so a write that cannot take the lock must leave no key
// file behind at all.
func TestDevKeyIsNotCreatedWithoutTheVaultLock(t *testing.T) {
	dir := t.TempDir()
	heldLock(t, dir)
	c := newTestChain(dir, map[string]string{EnvDevSecrets: "1"}, newFakeBackend(nil))
	if err := c.Set(briefCtx(t), Ref{ServerID: "srv", Key: "token"}, "v"); err == nil {
		t.Fatal("dev-mode Set completed while another process held the vault lock")
	}
	if _, err := os.Stat(filepath.Join(dir, devKeyFileName)); err == nil {
		t.Fatalf("%s was generated outside the vault lock; two processes racing here "+
			"produce two keys and an enc file neither can open", devKeyFileName)
	}
}

// The announcement is deliberately outside the lock (announce.go: a hint is
// not worth a cross-process lock on the token-refresh hot path). Announce is
// reachable while the vault is locked, and that is intended, not an oversight.
func TestAnnounceDoesNotTakeTheVaultLock(t *testing.T) {
	dir := t.TempDir()
	heldLock(t, dir)
	if err := Announce(dir, "srv"); err != nil {
		t.Fatalf("Announce under a held vault lock: %v", err)
	}
}
