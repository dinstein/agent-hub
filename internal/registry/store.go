// Package registry implements the multi-document configuration store that
// every agenthub process (CLI, gateways, daemon) shares on disk.
//
// Layout under the registry directory:
//
//	meta.json        monotonic generation (bumped inside the lock on real writes)
//	servers.json     downstream MCP servers
//	profiles.json    profiles (M0: minimal skeleton, single global tier)
//	clients.json     client bindings (M0: minimal skeleton)
//	governance.json  governance policy (M0: minimal skeleton)
//	.lock            sibling cross-process flock guarding all of the above
//	backups/         5 rolling generations per document (<name>.json.1 .. .5)
//
// Write-path hardening: sibling .lock flock with configurable
// timeout, atomic writes (same-dir temp file, chmod 0600, fsync, rename,
// fsync parent dir), a no-op guard comparing parsed JSON values, 5-generation
// rolling backups, quarantine of unparseable files (never destroyed), and a
// 4x75ms read retry to ride out non-atomic external writers.
//
// Unknown JSON fields are preserved at every nesting level via the Doc[T]
// envelope, so newer-version fields survive load-modify-save by older code.
//
// M1 additions (canonical.md §5c): Watch (fsnotify + debounce with a polling
// fallback, per-document Change{Kind, Rev} events, see watch.go), the Applier
// adoption criterion (generation >= applied, see applier.go), and self-write
// suppression (bounded TTL fingerprint set, see selfwrite.go).
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultLockTimeout bounds how long Open/Update wait for the cross-process
// lock before failing with ErrLockTimeout (CLI maps it to exit code 7).
const DefaultLockTimeout = 5 * time.Second

// Options tunes a Store. The zero value is usable: LockTimeout defaults to
// DefaultLockTimeout.
type Options struct {
	LockTimeout time.Duration
}

// Store is a handle on a registry directory.
//
// Concurrency model: every Update (and the initial Open) performs
// lock → load → modify → commit entirely under the sibling .lock flock, so
// multiple processes — and multiple Stores or goroutines in one process —
// serialize on disk state. The in-memory Snapshot is a convenience view of
// the state as of this Store's last Open/Update; it is never written back
// without a fresh reload under the lock, so a stale snapshot can never
// clobber another process's writes.
type Store struct {
	dir  string
	opts Options

	// selfWrites backs self-write suppression: every payload this Store
	// writes is fingerprinted here before the write so Watchers on the same
	// Store can tell own writes from external ones (canonical.md §5c #1).
	selfWrites selfWriteSet

	mu   sync.Mutex
	snap *Snapshot
}

// Tx exposes the mutable documents to an Update function. The pointers are
// valid only for the duration of the callback; retaining them afterwards is
// a bug. meta.json is not exposed — the generation is store-managed.
type Tx struct {
	Servers    *Doc[ServersDoc]
	Profiles   *Doc[ProfilesDoc]
	Clients    *Doc[ClientsDoc]
	Governance *Doc[GovernanceDoc]

	generation uint64
}

// Generation returns the generation the documents were loaded at (i.e. the
// value before any bump this Update may cause).
func (tx *Tx) Generation() uint64 { return tx.generation }

// Open initializes (creating missing files with defaults) and loads the
// registry at dir with default options.
//
// If one or more documents had to be quarantined as unreadable, Open still
// returns a usable *Store (the affected documents reset to defaults, all
// others intact) together with a non-nil error joining the *UnreadableError
// values — callers decide whether that is fatal for them.
func Open(dir string) (*Store, error) {
	return OpenOptions(dir, Options{})
}

// OpenOptions is Open with explicit Options.
func OpenOptions(dir string, opts Options) (*Store, error) {
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = DefaultLockTimeout
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, opts: opts}
	if _, quar, err := s.loadSnapshot(context.Background()); err != nil {
		return nil, err
	} else if quar != nil {
		return s, quar
	}
	return s, nil
}

// Snapshot returns the immutable view of the registry as of this Store's
// last successful Open or Update. Treat it as read-only.
func (s *Store) Snapshot() *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// Reload re-reads every document from disk under the cross-process lock and
// replaces this Store's in-memory snapshot. Watch consumers call it on a
// Change event — the event is only a notification, the state comes from this
// re-read — and adopt the returned snapshot per the Applier criterion
// (generation >= applied). Like Open, quarantines are reported but not fatal:
// a non-nil error alongside a non-nil snapshot means the snapshot is usable
// with the affected documents reset to defaults.
func (s *Store) Reload(ctx context.Context) (*Snapshot, error) {
	snap, quar, err := s.loadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return snap, quar
}

// loadSnapshot is the read half both Open and Reload perform: take the
// cross-process lock, load every document, release, and adopt the result as
// this Store's snapshot.
//
// The two error returns are not interchangeable, which is the whole reason
// this is one function rather than two copies. err means nothing was adopted
// and the snapshot is nil. quar (non-nil only alongside a usable snapshot)
// means one or more documents were unreadable and have been quarantined and
// reset to their defaults: the registry is serviceable and the caller decides
// whether that is fatal for it. Every failure before adoption joins the
// quarantines it already collected onto the error, so a caller that only looks
// at err still learns what was lost on the way.
func (s *Store) loadSnapshot(ctx context.Context) (snap *Snapshot, quar error, err error) {
	lock, err := acquireLock(ctx, s.dir, s.opts.LockTimeout)
	if err != nil {
		return nil, nil, err
	}
	st, quarErrs, err := loadAll(s.dir, &s.selfWrites)
	lock.release()
	if err != nil {
		return nil, nil, errors.Join(append(quarErrs, err)...)
	}
	snap, err = snapshotFromState(st)
	if err != nil {
		return nil, nil, errors.Join(append(quarErrs, err)...)
	}
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
	if len(quarErrs) > 0 {
		return snap, errors.Join(quarErrs...), nil
	}
	return snap, nil, nil
}

// Update runs fn inside the cross-process lock with a lock→load→modify→save
// cycle:
//
//   - acquire the sibling .lock (typed ErrLockTimeout on timeout)
//   - reload every document from disk (never trusts the in-memory snapshot)
//   - run fn against the freshly loaded documents
//   - for each document whose parsed value actually changed: rotate backups
//     and atomically rewrite it (no-op guard: unchanged docs are untouched)
//   - if anything was written, bump the generation in meta.json — still
//     inside the lock, so generations are strictly monotonic across processes
//
// fn returning an error aborts the update with nothing written. Quarantined
// documents (unreadable on load) do not block the update of other documents;
// their *UnreadableError values are joined into the returned error even on
// an otherwise successful commit.
func (s *Store) Update(ctx context.Context, fn func(tx *Tx) error) error {
	lock, err := acquireLock(ctx, s.dir, s.opts.LockTimeout)
	if err != nil {
		return err
	}
	defer lock.release()

	st, quar, err := loadAll(s.dir, &s.selfWrites)
	if err != nil {
		return errors.Join(append(quar, err)...)
	}

	tx := &Tx{
		Servers:    &st.servers.doc,
		Profiles:   &st.profiles.doc,
		Clients:    &st.clients.doc,
		Governance: &st.governance.doc,
		generation: st.meta.doc.V.Generation,
	}
	if err := fn(tx); err != nil {
		return errors.Join(append(quar, err)...)
	}

	changed := false
	sw := &s.selfWrites
	commits := []func() (bool, error){
		func() (bool, error) { return commitDoc(s.dir, DocServers, &st.servers, sw) },
		func() (bool, error) { return commitDoc(s.dir, DocProfiles, &st.profiles, sw) },
		func() (bool, error) { return commitDoc(s.dir, DocClients, &st.clients, sw) },
		func() (bool, error) { return commitDoc(s.dir, DocGovernance, &st.governance, sw) },
	}
	for _, commit := range commits {
		c, err := commit()
		if err != nil {
			return errors.Join(append(quar, err)...)
		}
		changed = changed || c
	}
	if changed {
		// Bump strictly inside the lock; the no-op guard above guarantees
		// the bump corresponds to a real state change (no phantom bumps).
		st.meta.doc.V.Generation++
		if _, err := commitDoc(s.dir, DocMeta, &st.meta, sw); err != nil {
			return errors.Join(append(quar, err)...)
		}
	}

	snap, err := snapshotFromState(st)
	if err != nil {
		return errors.Join(append(quar, err)...)
	}
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()

	if len(quar) > 0 {
		return errors.Join(quar...)
	}
	return nil
}

// docFile pairs a parsed document with the raw bytes it was loaded from (or
// initialized to); raw is the comparison baseline for the no-op guard and
// the content preserved by backup rotation.
type docFile[T any] struct {
	doc Doc[T]
	raw []byte
}

// state is everything loaded under one lock acquisition.
type state struct {
	meta       docFile[MetaDoc]
	servers    docFile[ServersDoc]
	profiles   docFile[ProfilesDoc]
	clients    docFile[ClientsDoc]
	governance docFile[GovernanceDoc]
}

// loadAll loads (initializing or quarantining as needed) every document.
// Quarantine errors are collected, not fatal: the failed document is reset
// to its default so the rest of the registry stays serviceable. Must be
// called with the lock held. sw (nilable) registers any default-writes as
// self-writes so a Watcher on the same Store does not report them.
func loadAll(dir string, sw *selfWriteSet) (*state, []error, error) {
	st := &state{}
	var quar []error
	note := func(u *UnreadableError) {
		if u != nil {
			quar = append(quar, u)
		}
	}

	var u *UnreadableError
	var err error
	if st.meta, u, err = loadDocFile(dir, DocMeta, defaultMetaDoc, sw); err != nil {
		return nil, quar, err
	}
	note(u)
	if st.servers, u, err = loadDocFile(dir, DocServers, defaultServersDoc, sw); err != nil {
		return nil, quar, err
	}
	note(u)
	if st.profiles, u, err = loadDocFile(dir, DocProfiles, defaultProfilesDoc, sw); err != nil {
		return nil, quar, err
	}
	note(u)
	if st.clients, u, err = loadDocFile(dir, DocClients, defaultClientsDoc, sw); err != nil {
		return nil, quar, err
	}
	note(u)
	if st.governance, u, err = loadDocFile(dir, DocGovernance, defaultGovernanceDoc, sw); err != nil {
		return nil, quar, err
	}
	note(u)
	return st, quar, nil
}

// loadDocFile reads and parses one document.
//
//   - Missing file: the default document is written (atomically) so the file
//     exists from first contact on; not counted as a change (no bump).
//   - Parse failure: retried readRetries x readRetryDelay (re-reading each
//     time) to ride out a non-atomic writer, then the file is quarantined to
//     <name>.json.unreadable-<timestamp>, a fresh default is written, and a
//     *UnreadableError is reported alongside a usable default document.
func loadDocFile[T any](dir string, kind DocKind, def func() Doc[T], sw *selfWriteSet) (docFile[T], *UnreadableError, error) {
	path := docPath(dir, kind)

	writeDefault := func() (docFile[T], error) {
		d := def()
		data, err := encodeDoc(d)
		if err != nil {
			return docFile[T]{}, err
		}
		if err := registeredWrite(path, data, sw); err != nil {
			return docFile[T]{}, err
		}
		return docFile[T]{doc: d, raw: data}, nil
	}

	var parseErr error
	for attempt := 0; attempt <= readRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(readRetryDelay)
		}
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			df, werr := writeDefault()
			return df, nil, werr
		}
		if err != nil {
			return docFile[T]{}, nil, err
		}
		var d Doc[T]
		if parseErr = json.Unmarshal(b, &d); parseErr == nil {
			return docFile[T]{doc: d, raw: b}, nil, nil
		}
	}

	qpath, qerr := quarantine(path)
	if qerr != nil {
		return docFile[T]{}, nil, errors.Join(parseErr, qerr)
	}
	df, werr := writeDefault()
	if werr != nil {
		return docFile[T]{}, nil, errors.Join(parseErr, werr)
	}
	return df, &UnreadableError{Kind: kind, Path: path, QuarantinePath: qpath, Err: parseErr}, nil
}

// commitDoc writes df.doc back to disk iff its parsed JSON value differs
// from what was loaded (no-op guard). A real write rotates backups first.
// Returns whether a write happened. Must be called with the lock held.
// sw (nilable) receives the payload fingerprint for self-write suppression.
func commitDoc[T any](dir string, kind DocKind, df *docFile[T], sw *selfWriteSet) (bool, error) {
	data, err := encodeDoc(df.doc)
	if err != nil {
		return false, err
	}
	if canonicallyEqual(data, df.raw) {
		return false, nil
	}
	base := string(kind) + ".json"
	if err := rotateBackups(dir, base, df.raw); err != nil {
		return false, err
	}
	if err := registeredWrite(docPath(dir, kind), data, sw); err != nil {
		return false, err
	}
	df.raw = data
	return true, nil
}

// registeredWrite is atomicWrite bracketed by self-write bookkeeping: the
// payload fingerprint is registered BEFORE the write (the watcher may observe
// the rename at any instant after it) and withdrawn if the write fails — a
// fingerprint for content that never reached disk must not suppress a future
// external write of identical content.
func registeredWrite(path string, data []byte, sw *selfWriteSet) error {
	if sw == nil {
		return atomicWrite(path, data)
	}
	fp := fingerprint(data)
	sw.register(fp)
	if err := atomicWrite(path, data); err != nil {
		sw.withdraw(fp)
		return err
	}
	return nil
}

// snapshotFromState deep-copies the loaded documents into an immutable
// Snapshot (JSON round-trip: cheap, and guaranteed independent of the maps
// the Tx callback may still reference).
func snapshotFromState(st *state) (*Snapshot, error) {
	servers, err := cloneDoc(st.servers.doc)
	if err != nil {
		return nil, err
	}
	profiles, err := cloneDoc(st.profiles.doc)
	if err != nil {
		return nil, err
	}
	clients, err := cloneDoc(st.clients.doc)
	if err != nil {
		return nil, err
	}
	governance, err := cloneDoc(st.governance.doc)
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		Generation: st.meta.doc.V.Generation,
		Servers:    servers,
		Profiles:   profiles,
		Clients:    clients,
		Governance: governance,
	}, nil
}

func cloneDoc[T any](d Doc[T]) (Doc[T], error) {
	b, err := json.Marshal(d)
	if err != nil {
		return Doc[T]{}, err
	}
	var out Doc[T]
	if err := json.Unmarshal(b, &out); err != nil {
		return Doc[T]{}, err
	}
	return out, nil
}

func docPath(dir string, kind DocKind) string {
	return filepath.Join(dir, string(kind)+".json")
}
