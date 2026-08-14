package registry

import (
	"bytes"
	"os"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch defaults (docs/subsystems/registry.md): fsnotify + 200ms debounce as the primary
// signal, a 2s poll as the safety net (fsnotify is unreliable on SMB/network
// mounts, and may fail to initialize at all — then polling carries the whole
// load).
const (
	defaultWatchDebounce = 200 * time.Millisecond
	defaultWatchPoll     = 2 * time.Second

	// watchChanBuffer bounds the event channel. A full channel never blocks
	// the scan loop: undeliverable events are parked per-kind and re-sent on
	// the next trigger (coalescing is safe because Change is a notification,
	// not a snapshot — consumers re-read and apply the >= criterion).
	watchChanBuffer = 16
)

// Change is a watch notification: which document changed and the registry
// generation observed when the change was detected.
//
// Rev is a HINT only, never a snapshot. Consumers must re-read the registry
// and adopt what they read iff its generation >= their last applied
// generation (see Applier; docs/conventions.md#the-hot-reload-path #2). Comparing for equality with
// Rev strands consumers on stale state when rapid writes coalesce.
type Change struct {
	Kind DocKind
	Rev  uint64
}

// WatchOptions tunes a Watcher. The zero value uses the defaults above.
type WatchOptions struct {
	Debounce time.Duration
	Poll     time.Duration

	// DisableFSNotify forces poll-only mode (test hook; the same code path
	// handles fsnotify initialization failure in production).
	DisableFSNotify bool
}

// Watcher reports external changes to the registry directory of the Store it
// was created from. Its own Store's writes are suppressed via the shared
// self-write fingerprint set; writes by any other process or Store instance
// are reported.
//
// Invariants:
//   - Per-document applied baseline: an event for a kind is emitted only when
//     that document's canonical content differs from the last content this
//     Watcher applied — so events carry the precise DocKind, never a blanket
//     "something changed".
//   - Load failure never advances the applied baseline (a half-written file
//     is retried by the next debounce/poll trigger; the pre-failure baseline
//     stays authoritative until a readable state appears).
//   - The scan loop never blocks on a slow consumer (see watchChanBuffer).
type Watcher struct {
	store *Store
	opts  WatchOptions

	ch   chan Change
	done chan struct{}

	closeOnce sync.Once
	wg        sync.WaitGroup

	// usingFSNotify records whether the fsnotify layer initialized; polling
	// runs in both cases. Set once before the run goroutine starts.
	usingFSNotify bool

	// State below is owned by the run goroutine exclusively.
	lastGen uint64
	applied map[DocKind][]byte // canonical content last applied per document
	pending map[DocKind]uint64 // events that could not be delivered yet
}

// contentKinds are the documents that produce Change events. meta.json is
// store-internal: its generation travels as Change.Rev, it is never a Kind.
var contentKinds = [...]DocKind{DocServers, DocProfiles, DocClients, DocGovernance}

// Watch starts watching the Store's registry directory with default options.
// Close the returned Watcher to release its resources; the Events channel is
// closed on Close.
func (s *Store) Watch() (*Watcher, error) {
	return s.WatchWith(WatchOptions{})
}

// WatchWith is Watch with explicit options.
func (s *Store) WatchWith(opts WatchOptions) (*Watcher, error) {
	if opts.Debounce <= 0 {
		opts.Debounce = defaultWatchDebounce
	}
	if opts.Poll <= 0 {
		opts.Poll = defaultWatchPoll
	}
	w := &Watcher{
		store:   s,
		opts:    opts,
		ch:      make(chan Change, watchChanBuffer),
		done:    make(chan struct{}),
		applied: make(map[DocKind][]byte, len(contentKinds)),
		pending: make(map[DocKind]uint64),
	}

	// Seed the applied baseline from the Store's current snapshot: what this
	// process has already applied must not be re-reported.
	snap := s.Snapshot()
	w.lastGen = snap.Generation
	seed := func(kind DocKind, doc any) error {
		data, err := encodeDoc(doc)
		if err != nil {
			return err
		}
		c, err := canonicalize(data)
		if err != nil {
			return err
		}
		w.applied[kind] = c
		return nil
	}
	if err := seed(DocServers, snap.Servers); err != nil {
		return nil, err
	}
	if err := seed(DocProfiles, snap.Profiles); err != nil {
		return nil, err
	}
	if err := seed(DocClients, snap.Clients); err != nil {
		return nil, err
	}
	if err := seed(DocGovernance, snap.Governance); err != nil {
		return nil, err
	}

	// fsnotify is best-effort: any failure here degrades to poll-only mode
	// instead of failing Watch (fail-open toward the polling fallback — a
	// watcher that merely lags is strictly better than none).
	var fsn *fsnotify.Watcher
	if !opts.DisableFSNotify {
		if fw, err := fsnotify.NewWatcher(); err == nil {
			if err := fw.Add(s.dir); err == nil {
				fsn = fw
			} else {
				_ = fw.Close()
			}
		}
	}
	w.usingFSNotify = fsn != nil

	w.wg.Add(1)
	go w.run(fsn)
	return w, nil
}

// Events returns the change channel. It is closed when the Watcher is
// closed.
func (w *Watcher) Events() <-chan Change { return w.ch }

// Close stops the watcher and closes the Events channel. Safe to call more
// than once.
func (w *Watcher) Close() {
	w.closeOnce.Do(func() {
		close(w.done)
		w.wg.Wait()
	})
}

// run is the single goroutine owning all watch state.
func (w *Watcher) run(fsn *fsnotify.Watcher) {
	defer w.wg.Done()
	defer close(w.ch)
	if fsn != nil {
		defer func() { _ = fsn.Close() }()
	}

	poll := time.NewTicker(w.opts.Poll)
	defer poll.Stop()

	// debounce fires opts.Debounce after the last relevant fsnotify event.
	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()

	var fsnEvents chan fsnotify.Event
	var fsnErrors chan error
	if fsn != nil {
		fsnEvents = fsn.Events
		fsnErrors = fsn.Errors
	}

	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-fsnEvents:
			if !ok {
				// fsnotify died mid-flight: fall back to polling alone.
				fsnEvents, fsnErrors = nil, nil
				continue
			}
			if watchRelevant(ev) {
				debounce.Reset(w.opts.Debounce)
			}
		case _, ok := <-fsnErrors:
			if !ok {
				fsnErrors = nil
				continue
			}
			// fsnotify errors are non-fatal: polling covers any missed event.
		case <-debounce.C:
			w.scan()
		case <-poll.C:
			w.scan()
		}
	}
}

// watchRelevant filters fsnotify events down to the registry documents.
// Renames and removes matter too: atomic writes surface as Create/Rename on
// the target, and an external `mv` may only produce Rename.
func watchRelevant(ev fsnotify.Event) bool {
	base := ev.Name
	if i := lastPathSep(base); i >= 0 {
		base = base[i+1:]
	}
	for _, kind := range contentKinds {
		if base == string(kind)+".json" {
			return true
		}
	}
	return base == string(DocMeta)+".json"
}

func lastPathSep(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if os.IsPathSeparator(s[i]) {
			return i
		}
	}
	return -1
}

// scan re-reads the registry and emits Change events for externally modified
// documents. Reads are lock-free: our own writers rename atomically, and a
// torn state from a non-atomic external writer simply fails canonicalization
// and is retried by the next trigger (load failure never advances the
// baseline).
func (w *Watcher) scan() {
	w.flushPending()

	gen, ok := readGeneration(w.store.dir)
	if !ok {
		// meta.json unreadable (likely mid-write): retry next trigger without
		// advancing anything.
		return
	}

	for _, kind := range contentKinds {
		raw, err := os.ReadFile(docPath(w.store.dir, kind))
		if err != nil {
			continue // missing/unreadable: keep old baseline, retry later
		}
		canon, err := canonicalize(raw)
		if err != nil {
			continue // half-written or invalid JSON: keep old baseline
		}
		if bytes.Equal(canon, w.applied[kind]) {
			continue
		}
		if w.store.selfWrites.consume(fingerprint(canon)) {
			// Our own write: advance the baseline silently, no event.
			w.applied[kind] = canon
			continue
		}
		// External change: pending self-write fingerprints no longer describe
		// the on-disk lineage — drop them all (see selfWriteSet.clear).
		w.store.selfWrites.clear()
		w.applied[kind] = canon
		w.emit(kind, gen)
	}
	if gen > w.lastGen {
		w.lastGen = gen
	}
}

// readGeneration reads the current generation from meta.json without taking
// the cross-process lock. ok=false means "could not read a consistent value
// right now" — callers must retry later rather than assume anything.
func readGeneration(dir string) (uint64, bool) {
	b, err := os.ReadFile(docPath(dir, DocMeta))
	if err != nil {
		return 0, false
	}
	var d Doc[MetaDoc]
	if err := d.UnmarshalJSON(b); err != nil {
		return 0, false
	}
	return d.V.Generation, true
}

// emit delivers a Change without ever blocking the scan loop: if the channel
// is full the event is parked per-kind (keeping the newest Rev) and re-sent
// by flushPending on the next trigger.
func (w *Watcher) emit(kind DocKind, rev uint64) {
	select {
	case w.ch <- Change{Kind: kind, Rev: rev}:
	default:
		w.pending[kind] = rev
	}
}

// flushPending retries parked events. Delivery order across kinds is
// unspecified; coalescing per kind is safe because consumers re-read the
// registry rather than trusting Rev (see Change).
func (w *Watcher) flushPending() {
	for kind, rev := range w.pending {
		select {
		case w.ch <- Change{Kind: kind, Rev: rev}:
			delete(w.pending, kind)
		default:
			return // still full; keep the rest parked
		}
	}
}
