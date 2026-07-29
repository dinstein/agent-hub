package shaping

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fileEntry(id string, owner Owner, created time.Time, ttl time.Duration, full string) Entry {
	return Entry{ID: id, Owner: owner, CreatedAt: created, TTL: ttl, Budget: Budget{Bytes: 256}, Full: full}
}

func ownerPath(root string, owner Owner, id string) string {
	sum := sha256.Sum256([]byte(owner))
	return filepath.Join(root, hex.EncodeToString(sum[:]), id+entrySuffix)
}

// The on-disk record is frozen: it survives a daemon restart, so a silent
// field rename would invalidate every live cursor.
func TestFileStoreRecordGolden(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStoreAt(root, goldenNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), fileEntry(goldenID, goldenOwner, goldenNow, 30*time.Minute, "abc")); err != nil {
		t.Fatal(err)
	}
	path := ownerPath(root, goldenOwner, goldenID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"v":1,"id":"rc-000001","owner":"claude-code:1",` +
		`"createdAt":"2026-07-26T12:00:00Z","ttlSeconds":1800,"budgetBytes":256,"full":"abc"}`
	if string(data) != want {
		t.Errorf("record drifted:\n got %s\nwant %s", data, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("entry mode = %v, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("owner dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}
}

// Atomic write: a replaced entry leaves no temp file behind and the target
// is never observed half-written.
func TestFileStoreAtomicWrite(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStoreAt(root, goldenNow)
	if err != nil {
		t.Fatal(err)
	}
	s.Clock = func() time.Time { return goldenNow }
	for i, full := range []string{strings.Repeat("a", 4000), "short", strings.Repeat("b", 9000)} {
		if err := s.Put(t.Context(), fileEntry(goldenID, goldenOwner, goldenNow, time.Hour, full)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		got, err := s.Get(t.Context(), goldenOwner, goldenID)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if got.Full != full {
			t.Fatalf("put %d: payload = %d bytes, want %d", i, len(got.Full), len(full))
		}
	}
	files, err := os.ReadDir(filepath.Dir(ownerPath(root, goldenOwner, goldenID)))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !strings.HasSuffix(files[0].Name(), entrySuffix) {
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = f.Name()
		}
		t.Errorf("temp files left behind: %v", names)
	}
}

// A corrupt record costs exactly one cursor: it is skipped on read and
// dropped by Sweep, and its neighbours keep working. That property is why
// doc.go rules for per-file storage over an embedded database.
func TestFileStoreCorruptEntrySkipped(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStoreAt(root, goldenNow)
	if err != nil {
		t.Fatal(err)
	}
	s.Clock = func() time.Time { return goldenNow }
	good := fileEntry("rc-000100", goldenOwner, goldenNow, time.Hour, "healthy")
	if err := s.Put(t.Context(), good); err != nil {
		t.Fatal(err)
	}
	corrupt := map[string]string{
		"rc-000101": `{"v":1,"id":"rc-000101",`,             // truncated mid-write
		"rc-000102": ``,                                     // zero length
		"rc-000103": `{"v":99,"id":"rc-000103","full":"x"}`, // unknown schema version
		"rc-000104": `not json at all`,
		"rc-000105": `{"v":1,"id":"nonsense","full":"x"}`, // id does not match its shape
	}
	dir := filepath.Dir(ownerPath(root, goldenOwner, good.ID))
	for id, body := range corrupt {
		if err := os.WriteFile(filepath.Join(dir, id+entrySuffix), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for id := range corrupt {
		if _, err := s.Get(t.Context(), goldenOwner, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("corrupt %s: err = %v, want ErrNotFound", id, err)
		}
		res, ok := Fetch(t.Context(), s, goldenOwner, id, 0)
		if ok || string(res.Content) != goldenNotFound {
			t.Errorf("corrupt %s: fetch must return the frozen miss", id)
		}
	}
	if got, err := s.Get(t.Context(), goldenOwner, good.ID); err != nil || got.Full != "healthy" {
		t.Errorf("healthy neighbour broke: %v / %q", err, got.Full)
	}
	n, err := s.Sweep(t.Context(), goldenNow)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(corrupt) {
		t.Errorf("sweep removed %d, want %d", n, len(corrupt))
	}
	if got, err := s.Get(t.Context(), goldenOwner, good.ID); err != nil || got.Full != "healthy" {
		t.Errorf("sweep took the healthy entry: %v", err)
	}
}

// TTL expiry: an expired entry is unreadable immediately and swept from
// disk, and the startup sweep clears what a crashed daemon left behind.
func TestFileStoreTTLSweep(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStoreAt(root, goldenNow)
	if err != nil {
		t.Fatal(err)
	}
	s.Clock = func() time.Time { return goldenNow }
	live := fileEntry("rc-000001", goldenOwner, goldenNow, time.Hour, "live")
	dead := fileEntry("rc-000002", goldenOwner, goldenNow, time.Minute, "dead")
	other := fileEntry("rc-000003", "claude-code:2", goldenNow, time.Minute, "other")
	for _, e := range []Entry{live, dead, other} {
		if err := s.Put(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}

	later := goldenNow.Add(30 * time.Minute)
	s.Clock = func() time.Time { return later }
	if _, err := s.Get(t.Context(), goldenOwner, dead.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired entry served: %v", err)
	}
	if _, err := s.Get(t.Context(), goldenOwner, live.ID); err != nil {
		t.Errorf("live entry rejected: %v", err)
	}

	// Restart: the startup sweep clears the expired entries (including the
	// other owner's) and prunes the emptied owner directory.
	s2, err := NewFileStoreAt(root, later)
	if err != nil {
		t.Fatal(err)
	}
	s2.Clock = func() time.Time { return later }
	if _, err := os.Stat(ownerPath(root, goldenOwner, dead.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Error("startup sweep left the expired entry on disk")
	}
	if _, err := os.Stat(filepath.Dir(ownerPath(root, "claude-code:2", other.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Error("startup sweep left an empty owner directory")
	}
	// A cursor survives the restart within its TTL (docs/flows.md).
	got, err := s2.Get(t.Context(), goldenOwner, live.ID)
	if err != nil || got.Full != "live" {
		t.Errorf("live cursor did not survive restart: %v / %q", err, got.Full)
	}
	// The sequence resumes above the highest surviving id, so the restarted
	// process cannot mint an id that overwrites a live cursor.
	if next := s2.NextID(); next != "rc-000002" {
		t.Errorf("resumed sequence = %q, want rc-000002", next)
	}
}

// A non-positive TTL is expired on sight: an entry with no life left must
// never be served.
func TestEntryExpiry(t *testing.T) {
	e := Entry{CreatedAt: goldenNow, TTL: time.Minute}
	if e.Expired(goldenNow) {
		t.Error("fresh entry reported expired")
	}
	if !e.Expired(goldenNow.Add(time.Minute)) {
		t.Error("entry must expire at exactly CreatedAt+TTL")
	}
	if !(Entry{CreatedAt: goldenNow, TTL: 0}).Expired(goldenNow) {
		t.Error("zero TTL must count as expired")
	}
	if !(Entry{CreatedAt: goldenNow, TTL: -time.Minute}).Expired(goldenNow) {
		t.Error("negative TTL must count as expired")
	}
}

// A malformed id never reaches the filesystem: FileStore turns ids into
// paths, so traversal must be rejected before the join.
func TestFileStoreRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStoreAt(root, goldenNow)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "secret.json")
	if err := os.WriteFile(secret, []byte(`{"v":1,"id":"rc-000001","full":"leak"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../secret", "..%2Fsecret", "rc-000001/../../secret", "/etc/passwd"} {
		if _, err := s.Get(t.Context(), goldenOwner, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) err = %v, want ErrNotFound", id, err)
		}
		if err := s.Put(t.Context(), Entry{ID: id, Owner: goldenOwner, TTL: time.Hour}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Put(%q) err = %v, want ErrNotFound", id, err)
		}
	}
}

func TestMemStoreBoundEvictsOldest(t *testing.T) {
	s := NewMemStore(3)
	s.Clock = func() time.Time { return goldenNow }
	ids := []string{"rc-000001", "rc-000002", "rc-000003", "rc-000004", "rc-000005"}
	for _, id := range ids {
		if err := s.Put(t.Context(), fileEntry(id, goldenOwner, goldenNow, time.Hour, id)); err != nil {
			t.Fatal(err)
		}
	}
	if s.Len() != 3 {
		t.Fatalf("len = %d, want 3", s.Len())
	}
	for _, gone := range ids[:2] {
		if _, err := s.Get(t.Context(), goldenOwner, gone); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s should have been evicted", gone)
		}
	}
	for _, kept := range ids[2:] {
		if _, err := s.Get(t.Context(), goldenOwner, kept); err != nil {
			t.Errorf("%s should have been kept: %v", kept, err)
		}
	}
}

func TestMemStoreSweep(t *testing.T) {
	s := NewMemStore(0)
	s.Clock = func() time.Time { return goldenNow }
	if err := s.Put(t.Context(), fileEntry("rc-000001", goldenOwner, goldenNow, time.Hour, "live")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), fileEntry("rc-000002", goldenOwner, goldenNow, time.Minute, "dead")); err != nil {
		t.Fatal(err)
	}
	n, err := s.Sweep(t.Context(), goldenNow.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || s.Len() != 1 {
		t.Errorf("sweep removed %d leaving %d, want 1 / 1", n, s.Len())
	}
}

// Sweep must not delete a temp file a concurrent Put is still writing.
func TestSweepSparesYoungTempFiles(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStoreAt(root, goldenNow)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(ownerPath(root, goldenOwner, goldenID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	young := filepath.Join(dir, "rc-000001.json.tmp-fresh")
	if err := os.WriteFile(young, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sweep(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("sweep deleted an in-flight temp file: %v", err)
	}
	// Far enough in the future, the same file is garbage.
	if _, err := s.Sweep(t.Context(), time.Now().Add(2*tempGrace)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(young); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale temp file survived the sweep")
	}
}
