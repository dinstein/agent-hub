package secrets

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// memStore is a plain in-memory Store for migration tests.
type memStore struct {
	mu     sync.Mutex
	data   map[string]string
	setErr error
	delErr error
	// getOverride, when non-nil, hijacks Get (simulates a lying store
	// for read-back verification failures).
	getOverride func(ref Ref) (string, bool, error)
}

func newMemStore() *memStore { return &memStore{data: map[string]string{}} }

func (m *memStore) Get(_ context.Context, ref Ref) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getOverride != nil {
		return m.getOverride(ref)
	}
	v, ok := m.data[ref.StorageKey()]
	return v, ok, nil
}

func (m *memStore) Set(_ context.Context, ref Ref, val string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setErr != nil {
		return m.setErr
	}
	m.data[ref.StorageKey()] = val
	return nil
}

func (m *memStore) Delete(_ context.Context, ref Ref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.delErr != nil {
		return m.delErr
	}
	delete(m.data, ref.StorageKey())
	return nil
}

func (m *memStore) has(ref Ref) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[ref.StorageKey()]
	return ok
}

func TestMigrateMovesAndDeletesOld(t *testing.T) {
	ctx := context.Background()
	ref := Ref{ServerID: "srv", Key: "token"}
	from, to := newMemStore(), newMemStore()
	if err := from.Set(ctx, ref, "v"); err != nil {
		t.Fatal(err)
	}
	res := Migrate(ctx, from, to, []Ref{ref})
	if len(res) != 1 || res[0].Err != nil || !res[0].Migrated {
		t.Fatalf("results = %+v", res)
	}
	if from.has(ref) {
		t.Fatal("old entry not deleted")
	}
	v, ok, _ := to.Get(ctx, ref)
	if !ok || v != "v" {
		t.Fatalf("new store Get = (%q, %v)", v, ok)
	}
}

func TestMigrateAbsentRefIsNoop(t *testing.T) {
	ctx := context.Background()
	ref := Ref{ServerID: "srv", Key: "token"}
	res := Migrate(ctx, newMemStore(), newMemStore(), []Ref{ref})
	if len(res) != 1 || res[0].Err != nil || res[0].Migrated {
		t.Fatalf("results = %+v", res)
	}
}

// TestMigrateVerifyFailureKeepsOld: when read-back does not return the
// written value, the old entry must survive (fail-closed).
func TestMigrateVerifyFailureKeepsOld(t *testing.T) {
	ctx := context.Background()
	ref := Ref{ServerID: "srv", Key: "token"}
	from, to := newMemStore(), newMemStore()
	if err := from.Set(ctx, ref, "v"); err != nil {
		t.Fatal(err)
	}
	to.getOverride = func(Ref) (string, bool, error) { return "corrupted", true, nil }
	res := Migrate(ctx, from, to, []Ref{ref})
	if res[0].Err == nil || res[0].Migrated {
		t.Fatalf("results = %+v, want verification error", res)
	}
	if !from.has(ref) {
		t.Fatal("old entry deleted despite failed verification")
	}
}

func TestMigrateSetFailureKeepsOld(t *testing.T) {
	ctx := context.Background()
	ref := Ref{ServerID: "srv", Key: "token"}
	from, to := newMemStore(), newMemStore()
	if err := from.Set(ctx, ref, "v"); err != nil {
		t.Fatal(err)
	}
	to.setErr = errors.New("disk full")
	res := Migrate(ctx, from, to, []Ref{ref})
	if res[0].Err == nil || res[0].Migrated {
		t.Fatalf("results = %+v, want write error", res)
	}
	if !from.has(ref) {
		t.Fatal("old entry lost on write failure")
	}
}

// TestMigrateDeleteOldFailureIsSurfaced: the value is safe (duplicated)
// but the result must say so.
func TestMigrateDeleteOldFailureIsSurfaced(t *testing.T) {
	ctx := context.Background()
	ref := Ref{ServerID: "srv", Key: "token"}
	from, to := newMemStore(), newMemStore()
	if err := from.Set(ctx, ref, "v"); err != nil {
		t.Fatal(err)
	}
	from.delErr = errors.New("keychain denied")
	res := Migrate(ctx, from, to, []Ref{ref})
	if res[0].Err == nil || res[0].Migrated {
		t.Fatalf("results = %+v, want delete error", res)
	}
	if !strings.Contains(res[0].Err.Error(), "duplicated") {
		t.Fatalf("error should mention duplication: %v", res[0].Err)
	}
	if v, ok, _ := to.Get(ctx, ref); !ok || v != "v" {
		t.Fatal("new store must still hold the value")
	}
}

// TestMigrateMixedBatch: per-ref isolation — one failure does not stop
// the batch.
func TestMigrateMixedBatch(t *testing.T) {
	ctx := context.Background()
	good := Ref{ServerID: "a", Key: "k"}
	absent := Ref{ServerID: "b", Key: "k"}
	from, to := newMemStore(), newMemStore()
	if err := from.Set(ctx, good, "v"); err != nil {
		t.Fatal(err)
	}
	res := Migrate(ctx, from, to, []Ref{absent, good})
	if len(res) != 2 {
		t.Fatalf("len(res) = %d", len(res))
	}
	if res[0].Migrated || res[0].Err != nil {
		t.Fatalf("absent ref result = %+v", res[0])
	}
	if !res[1].Migrated || res[1].Err != nil {
		t.Fatalf("good ref result = %+v", res[1])
	}
}
