package shaping

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// dirName is the shaping cache subdirectory under <data>/cache.
const dirName = "shaping"

// entrySuffix is the retained-entry filename extension.
const entrySuffix = ".json"

// fileRecordVersion is the on-disk schema version. A record with an unknown
// version is treated exactly like a corrupt one: skipped on read, dropped by
// Sweep. Cursors are a cache, so forward compatibility costs one re-run of
// the tool call, never data.
const fileRecordVersion = 1

// tempGrace is how long a leftover temp file is spared by Sweep, so a sweep
// running concurrently with a Put cannot delete that Put's in-flight file.
const tempGrace = 5 * time.Minute

// fileRecord is the frozen on-disk encoding of an Entry. Field order and
// names are golden-tested; TTL is spelled in seconds so the file never
// depends on time.Duration's internal unit.
type fileRecord struct {
	Version     int       `json:"v"`
	ID          string    `json:"id"`
	Owner       string    `json:"owner"`
	CreatedAt   time.Time `json:"createdAt"`
	TTLSeconds  int64     `json:"ttlSeconds"`
	BudgetBytes int       `json:"budgetBytes"`
	Full        string    `json:"full"`
}

// FileStore retains cursors under <data>/cache/shaping. It is the daemon
// HTTP face's store: a session outlives the daemon process, so its cursors
// must too (docs/flows.md — valid across a restart for the session TTL).
//
// Layout: <root>/<sha256(owner)>/<cursor>.json, one file per cursor, written
// atomically. See doc.go for why this is plain files and not an embedded
// database. The owner-hash directory is path hygiene, not the isolation —
// Entry.Owner is verified on every read.
type FileStore struct {
	// Clock overrides time.Now for expiry checks (test seam). Set it before
	// the store is shared across goroutines.
	Clock func() time.Time

	root string
	seq  atomic.Uint64
}

// DefaultDir resolves <data>/cache/shaping using res (nil = the real
// process environment).
func DefaultDir(res *platform.Resolver) (string, error) {
	if res == nil {
		res = platform.Default()
	}
	cache, err := res.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, dirName), nil
}

// NewFileStore opens (creating if needed) the cache at root and performs the
// startup sweep: expired and unreadable entries are dropped before the store
// serves anything, so a crashed daemon never leaves a growing cache behind.
//
// The id sequence resumes above the highest surviving id, so a restart
// cannot mint an id that would overwrite a live cursor.
func NewFileStore(root string) (*FileStore, error) {
	return NewFileStoreAt(root, time.Now())
}

// NewFileStoreAt is NewFileStore with an explicit sweep instant (tests).
func NewFileStoreAt(root string, now time.Time) (*FileStore, error) {
	if root == "" {
		return nil, errors.New("shaping: empty cache root")
	}
	if err := platform.EnsureDir(root); err != nil {
		return nil, err
	}
	s := &FileStore{root: root}
	if _, err := s.Sweep(context.Background(), now); err != nil {
		return nil, err
	}
	high, err := s.highestID()
	if err != nil {
		return nil, err
	}
	s.seq.Store(high)
	return s, nil
}

// Root returns the cache root directory.
func (s *FileStore) Root() string { return s.root }

// NextID mints the next cursor id.
func (s *FileStore) NextID() string { return formatID(s.seq.Add(1)) }

// Put retains e atomically: same-directory temp file, 0600, fsync, rename.
// A reader therefore sees either the previous content or the new content,
// never a half-written record.
func (s *FileStore) Put(ctx context.Context, e Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(e.ID) {
		return ErrNotFound
	}
	dir := s.ownerDir(e.Owner)
	if err := platform.EnsureDir(dir); err != nil {
		return err
	}
	data, err := json.Marshal(fileRecord{
		Version:     fileRecordVersion,
		ID:          e.ID,
		Owner:       string(e.Owner),
		CreatedAt:   e.CreatedAt.UTC(),
		TTLSeconds:  int64(e.TTL / time.Second),
		BudgetBytes: e.Budget.Bytes,
		Full:        e.Full,
	})
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, e.ID+entrySuffix), data)
}

// Get returns the entry for (owner, id). Unknown id, malformed id, expired
// entry, wrong owner, unreadable file and corrupt record all collapse into
// ErrNotFound — the caller must not be able to tell them apart.
func (s *FileStore) Get(ctx context.Context, owner Owner, id string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if !validID(id) {
		return Entry{}, ErrNotFound
	}
	e, err := readEntry(filepath.Join(s.ownerDir(owner), id+entrySuffix))
	if err != nil {
		return Entry{}, ErrNotFound
	}
	if e.ID != id || !e.ownedBy(owner) || e.Expired(s.now()) {
		return Entry{}, ErrNotFound
	}
	return e, nil
}

// Sweep removes entries expired at now plus every unreadable or corrupt
// record, and prunes owner directories left empty. A corrupt record costs
// exactly one cursor: that is the property that made per-file storage the
// ruling in doc.go.
func (s *FileStore) Sweep(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	owners, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, od := range owners {
		if !od.IsDir() {
			continue
		}
		dir := filepath.Join(s.root, od.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		live := 0
		for _, f := range files {
			name := f.Name()
			path := filepath.Join(dir, name)
			// Temp files from an interrupted write are garbage by
			// definition: the rename never happened. A young one may
			// belong to a Put running right now, so it gets a grace
			// window instead of being yanked out from under the writer.
			if !strings.HasSuffix(name, entrySuffix) {
				if info, err := f.Info(); err == nil && now.Sub(info.ModTime()) < tempGrace {
					continue
				}
				if err := os.Remove(path); err == nil {
					removed++
				}
				continue
			}
			e, err := readEntry(path)
			if err != nil || e.Expired(now) {
				if err := os.Remove(path); err == nil {
					removed++
				}
				continue
			}
			live++
		}
		if live == 0 {
			_ = os.Remove(dir) // best effort: fails if raced, retried next sweep
		}
	}
	return removed, nil
}

func (s *FileStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// ownerDir maps an owner to its directory. Hashing keeps arbitrary session
// identities (which may contain ':' and other separators) off the
// filesystem path.
func (s *FileStore) ownerDir(owner Owner) string {
	sum := sha256.Sum256([]byte(owner))
	return filepath.Join(s.root, hex.EncodeToString(sum[:]))
}

// highestID returns the largest cursor sequence currently on disk, so a
// fresh process resumes above it instead of colliding with live cursors.
func (s *FileStore) highestID() (uint64, error) {
	owners, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var high uint64
	for _, od := range owners {
		if !od.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.root, od.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			id := strings.TrimSuffix(f.Name(), entrySuffix)
			if !validID(id) {
				continue
			}
			n, err := strconv.ParseUint(strings.TrimPrefix(id, "rc-"), 10, 64)
			if err == nil && n > high {
				high = n
			}
		}
	}
	return high, nil
}

// readEntry decodes one record. Any failure (missing, unreadable, malformed
// JSON, unknown version) is returned as an error and the caller maps it to
// ErrNotFound.
func readEntry(path string) (Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	var rec fileRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return Entry{}, err
	}
	if rec.Version != fileRecordVersion || !validID(rec.ID) {
		return Entry{}, errors.New("shaping: unusable record")
	}
	return Entry{
		ID:        rec.ID,
		Owner:     Owner(rec.Owner),
		CreatedAt: rec.CreatedAt,
		TTL:       time.Duration(rec.TTLSeconds) * time.Second,
		Budget:    Budget{Bytes: rec.BudgetBytes},
		Full:      rec.Full,
	}, nil
}

// atomicWrite persists data to path via a same-directory temp file:
// chmod 0600 → write → fsync → rename. The rename is the atomic step; the
// directory fsync afterwards is durability only, so its failure is ignored
// (a lost rename after a power cut costs one cursor, never a torn file).
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
